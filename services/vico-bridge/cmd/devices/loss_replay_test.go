package devices

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type replayFrameMeta struct {
	frameIdx int
	naluData []byte
	isIDR    bool
}

type replayPktEvent struct {
	frameIdx int
	pktIdx   int
	isIDR    bool
	arrive   time.Duration
	pkt      *rtp.Packet
}

type replayStageStats struct {
	inputPkts        int
	seqGaps          uint64
	seqMissing       uint64
	seqDup           uint64
	asmDrops         uint64
	asmLate          uint64
	stallResets      int
	emittedFrames    int
	firstFrameAt     time.Duration
	maxOutputGap     time.Duration
	idrArrivedAt     map[int]time.Duration
	idrEmittedAt     map[int]time.Duration
	maxIDREmitDelay  time.Duration
	decodeErrors     int
	decodeErrText    string
	verified2KCount  int
	nonMonotonicPTS  int
	nonMonotonicEmit int
}

func generateReal2KH264Stream(t *testing.T, frames int) []replayFrameMeta {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("ffmpeg not found in PATH: %v", err)
	}
	durSec := float64(frames) / 15.0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Encode real 2304x1296 @ 15fps H.264 High Profile (keyint=30 -> 2.0s IDRs) matching camera SPS dims.
	raw, err := exec.CommandContext(ctx, ffmpeg, "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=2304x1296:rate=15",
		"-t", fmt.Sprintf("%.3f", durSec),
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "high", "-g", "30", "-keyint_min", "30", "-sc_threshold", "0",
		"-bf", "0", "-x264-params", "aud=1:repeat-headers=1",
		"-b:v", "2800k", "-maxrate", "3200k", "-bufsize", "3200k",
		"-f", "h264", "pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg encode 2304x1296 failed: %v", err)
	}

	// Split Annex B stream into access units cleanly on AUD (NAL type 9).
	var out []replayFrameMeta
	var cur bytes.Buffer
	hasAU := false
	nalus := annexBNALUs(raw)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		typ := nalu[0] & 0x1f
		if typ == 9 {
			if hasAU && cur.Len() > 0 {
				b := append([]byte(nil), cur.Bytes()...)
				out = append(out, replayFrameMeta{
					frameIdx: len(out),
					naluData: b,
					isIDR:    containsIDR(b),
				})
				cur.Reset()
			}
			hasAU = true
		}
		if hasAU {
			cur.Write([]byte{0, 0, 0, 1})
			cur.Write(nalu)
		}
	}
	if hasAU && cur.Len() > 0 {
		b := append([]byte(nil), cur.Bytes()...)
		out = append(out, replayFrameMeta{
			frameIdx: len(out),
			naluData: b,
			isIDR:    containsIDR(b),
		})
	}
	if len(out) < frames-1 {
		t.Fatalf("expected ~%d access units from ffmpeg, got %d", frames, len(out))
	}
	return out
}

func containsIDR(annexB []byte) bool {
	for _, n := range annexBNALUs(annexB) {
		if len(n) > 0 && n[0]&0x1f == 5 {
			return true
		}
	}
	return false
}

func packetizeFrames530(frames []replayFrameMeta, startSeq uint16) []*replayPktEvent {
	payloader := &codecs.H264Payloader{}
	seq := startSeq
	var events []*replayPktEvent
	for _, f := range frames {
		// Strip AUD for RTP payload or keep full Annex B: H264Payloader handles 4-byte Annex B start codes cleanly.
		payloads := payloader.Payload(530, f.naluData)
		frameBase := time.Duration(f.frameIdx) * (time.Second / 15)
		spread := 12 * time.Millisecond
		if f.isIDR {
			// Real store 2K IDRs (~160-330 packets) take ~150-300ms to pace over TURN.
			spread = 220 * time.Millisecond
		}
		for i, pl := range payloads {
			at := frameBase + 20*time.Millisecond + spread*time.Duration(i)/time.Duration(max(1, len(payloads)))
			events = append(events, &replayPktEvent{
				frameIdx: f.frameIdx,
				pktIdx:   i,
				isIDR:    f.isIDR,
				arrive:   at,
				pkt: &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    121,
						SequenceNumber: seq,
						Timestamp:      uint32(f.frameIdx) * 6000,
						Marker:         i == len(payloads)-1,
					},
					Payload: pl,
				},
			})
			seq++
		}
	}
	return events
}

func verifyFFmpegDecode2K(t *testing.T, bitstream []byte) (int, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 1, "ffmpeg missing"
	}
	if len(bitstream) == 0 {
		return 1, "empty bitstream"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-xerror", "-err_detect", "explode", "-f", "h264", "-i", "pipe:0", "-f", "null", "-")
	cmd.Stdin = bytes.NewReader(bitstream)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 1, fmt.Sprintf("%v: %s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		return 1, stderr.String()
	}
	return 0, ""
}

func runPipelineVariant(t *testing.T, events []*replayPktEvent, spec streamProfileSpec, useNewAssembler bool) (replayStageStats, []byte) {
	t.Helper()
	sorted := append([]*replayPktEvent(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].arrive < sorted[j].arrive })

	stats := replayStageStats{
		idrArrivedAt: make(map[int]time.Duration),
		idrEmittedAt: make(map[int]time.Duration),
	}
	for _, ev := range sorted {
		if ev.isIDR && ev.pkt.Marker {
			if _, ok := stats.idrArrivedAt[ev.frameIdx]; !ok {
				stats.idrArrivedAt[ev.frameIdx] = ev.arrive
			}
		}
	}

	var diag rtpDiagTracker
	builder := newLiveH264BuilderWithWindow(&diag, 1200*time.Millisecond)
	asm := newLiveH264Assembler(&diag, 1200*time.Millisecond)
	rec := h264LiveRecovery{stall: 1800 * time.Millisecond}
	var ready atomic.Bool
	ready.Store(true)
	var gate h264LiveGate
	gate.configureProfile(spec, &ready)

	base := time.Unix(1700000000, 0)
	var out bytes.Buffer
	var prevEmit time.Duration
	var prevRTP uint32
	hasPrevRTP := false

	handleSample := func(sample *media.Sample, now time.Time, arrive time.Duration) {
		lateSample := rec.hasTimestamp && int32(sample.PacketTimestamp-rec.lastRTP) < 0
		completeSample := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
		diag.onSamplePopped(sample, lateSample || !completeSample, lateSample)
		data, frame, _, _ := rec.push(sample)
		if !frame || len(data) == 0 {
			return
		}
		accepted, ok := gate.pushFrame(data)
		if !ok || len(accepted) == 0 {
			return
		}
		rec.lastFrame = now
		out.Write(accepted)
		stats.emittedFrames++
		if stats.emittedFrames == 1 {
			stats.firstFrameAt = arrive
		} else if gap := arrive - prevEmit; gap > stats.maxOutputGap {
			stats.maxOutputGap = gap
		}
		if arrive < prevEmit {
			stats.nonMonotonicEmit++
		}
		prevEmit = arrive
		if hasPrevRTP && int32(sample.PacketTimestamp-prevRTP) <= 0 {
			stats.nonMonotonicPTS++
		}
		prevRTP = sample.PacketTimestamp
		hasPrevRTP = true
		if gate.lastSPS.Width == spec.Width && gate.lastSPS.Height == spec.Height {
			stats.verified2KCount++
		}
		fIdx := int(sample.PacketTimestamp / 6000)
		if containsIDR(accepted) {
			if _, ok := stats.idrEmittedAt[fIdx]; !ok {
				stats.idrEmittedAt[fIdx] = arrive
				if arr, ok2 := stats.idrArrivedAt[fIdx]; ok2 {
					if d := arrive - arr; d > stats.maxIDREmitDelay {
						stats.maxIDREmitDelay = d
					}
				}
			}
		}
	}

	for _, ev := range sorted {
		now := base.Add(ev.arrive)
		if rec.stalled(now) {
			stats.stallResets++
			if useNewAssembler {
				asm.PruneOnStall(now)
			} else {
				diag.onBuilderReset()
				builder = newLiveH264BuilderWithWindow(&diag, 1200*time.Millisecond)
			}
		}
		stats.inputPkts++
		pCopy := *ev.pkt
		diag.onInputPacket(&pCopy)
		if useNewAssembler {
			asm.Push(&pCopy, now)
			needIDR := rec.recovering || !rec.gate.started
			for sample := asm.Pop(now, needIDR); sample != nil; sample = asm.Pop(now, needIDR) {
				handleSample(sample, now, ev.arrive)
				needIDR = rec.recovering || !rec.gate.started
			}
		} else {
			builder.Push(&pCopy)
			for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
				handleSample(sample, now, ev.arrive)
			}
		}
		diag.afterPopDrain()
	}

	stats.seqGaps = diag.seqGaps.Load()
	stats.seqMissing = diag.seqMissing.Load()
	stats.seqDup = diag.seqDup.Load()
	stats.asmDrops = diag.asmDrops.Load()
	stats.asmLate = diag.asmLate.Load()
	stats.decodeErrors, stats.decodeErrText = verifyFFmpegDecode2K(t, out.Bytes())
	return stats, out.Bytes()
}

func TestLossRecoveryComparison(t *testing.T) {
	prof, err := parseStreamProfile("2k")
	if err != nil {
		t.Fatal(err)
	}
	frames := generateReal2KH264Stream(t, 95) // 6.33s @ 15fps, IDRs at 0, 30, 60, 90

	// Scenario 1: Stale duplicates arriving 950-1360 packets late mid-IDR 60 (145712 dining_area + 131114).
	t.Run("StaleDuplicateRewind", func(t *testing.T) {
		baseEvents := packetizeFrames530(frames, 1000)
		var staleTargets []*replayPktEvent
		var filtered []*replayPktEvent
		var frame60Mid time.Duration
		for _, ev := range baseEvents {
			if ev.frameIdx == 60 && ev.pktIdx == 20 {
				frame60Mid = ev.arrive
			}
			// Drop 3 packets from frame 5 (~800-1100 pkts behind frame 60) and re-inject 3x during frame 60 IDR!
			if ev.frameIdx == 5 && (ev.pktIdx == 0 || ev.pktIdx == 1 || ev.pktIdx == 2) {
				staleTargets = append(staleTargets, ev)
				continue
			}
			filtered = append(filtered, ev)
		}
		for rep := 0; rep < 3; rep++ {
			for j, st := range staleTargets {
				clone := *st
				clone.arrive = frame60Mid + time.Duration(rep*15+j*3)*time.Millisecond
				filtered = append(filtered, &clone)
			}
		}
		leg, _ := runPipelineVariant(t, filtered, prof, false)
		fix, _ := runPipelineVariant(t, filtered, prof, true)
		t.Logf("BEFORE StaleDup: in=%d seq_dup=%d asm_drops=%d asm_late=%d resets=%d frames=%d max_gap=%v idr_delay=%v decode_errs=%d",
			leg.inputPkts, leg.seqDup, leg.asmDrops, leg.asmLate, leg.stallResets, leg.emittedFrames, leg.maxOutputGap.Round(time.Millisecond), leg.maxIDREmitDelay.Round(time.Millisecond), leg.decodeErrors)
		t.Logf("AFTER  StaleDup: in=%d seq_dup=%d asm_drops=%d asm_late=%d resets=%d frames=%d max_gap=%v idr_delay=%v decode_errs=%d 2k_verified=%d non_mono_pts=%d",
			fix.inputPkts, fix.seqDup, fix.asmDrops, fix.asmLate, fix.stallResets, fix.emittedFrames, fix.maxOutputGap.Round(time.Millisecond), fix.maxIDREmitDelay.Round(time.Millisecond), fix.decodeErrors, fix.verified2KCount, fix.nonMonotonicPTS)
		if fix.decodeErrors != 0 {
			t.Fatalf("AFTER decode errors: %d (%s)", fix.decodeErrors, fix.decodeErrText)
		}
		if fix.verified2KCount != fix.emittedFrames || fix.nonMonotonicPTS != 0 || fix.nonMonotonicEmit != 0 {
			t.Fatalf("invariant violation: 2k=%d/%d non_mono_pts=%d non_mono_emit=%d", fix.verified2KCount, fix.emittedFrames, fix.nonMonotonicPTS, fix.nonMonotonicEmit)
		}
		if fix.maxIDREmitDelay > 100*time.Millisecond {
			t.Fatalf("expected stale duplicates not to delay complete IDR, got %v", fix.maxIDREmitDelay)
		}
		if fix.asmDrops > leg.asmDrops && leg.asmDrops > 0 {
			t.Fatalf("expected fixed assembler drops (%d) <= legacy drops (%d)", fix.asmDrops, leg.asmDrops)
		}
	})

	// Scenario 2: Complete 2304x1296 IDR arriving right after an incomplete P-frame + 2.2s pause (145712 t=31.1s..35.34s).
	t.Run("CompleteIDRTrappedAndPaused", func(t *testing.T) {
		baseEvents := packetizeFrames530(frames, 2000)
		var filtered []*replayPktEvent
		for _, ev := range baseEvents {
			if ev.frameIdx == 27 && ev.pktIdx == 0 {
				continue // missing hole before frame 30 IDR
			}
			cp := *ev
			if ev.frameIdx >= 31 {
				cp.arrive += 2200 * time.Millisecond
			}
			filtered = append(filtered, &cp)
		}
		leg, _ := runPipelineVariant(t, filtered, prof, false)
		fix, _ := runPipelineVariant(t, filtered, prof, true)
		_, legHadIDR30 := leg.idrEmittedAt[30]
		idr30At, fixHadIDR30 := fix.idrEmittedAt[30]
		t.Logf("BEFORE IDRTrap: idr30_emitted=%t asm_drops=%d resets=%d frames=%d max_gap=%v decode_errs=%d",
			legHadIDR30, leg.asmDrops, leg.stallResets, leg.emittedFrames, leg.maxOutputGap.Round(time.Millisecond), leg.decodeErrors)
		t.Logf("AFTER  IDRTrap: idr30_emitted=%t (delay=%v) asm_drops=%d resets=%d frames=%d max_gap=%v decode_errs=%d 2k=%d",
			fixHadIDR30, (idr30At - fix.idrArrivedAt[30]).Round(time.Millisecond), fix.asmDrops, fix.stallResets, fix.emittedFrames, fix.maxOutputGap.Round(time.Millisecond), fix.decodeErrors, fix.verified2KCount)
		if !fixHadIDR30 || (idr30At-fix.idrArrivedAt[30]) > 50*time.Millisecond {
			t.Fatalf("expected frame 30 complete IDR to emit immediately on arrival, got emitted=%t delay=%v", fixHadIDR30, idr30At-fix.idrArrivedAt[30])
		}
		if fix.decodeErrors != 0 || fix.verified2KCount != fix.emittedFrames {
			t.Fatalf("decode failure: errs=%d (%s) 2k=%d/%d", fix.decodeErrors, fix.decodeErrText, fix.verified2KCount, fix.emittedFrames)
		}
		if fix.emittedFrames <= leg.emittedFrames {
			t.Fatalf("expected AFTER to emit strictly more frames than BEFORE (%d <= %d)", fix.emittedFrames, leg.emittedFrames)
		}
	})

	// Scenario 3: frame_stall (1800ms) firing mid-IDR and destroying buffered SPS/PPS/head packets (124301 & 131114).
	t.Run("StallResetMidIDR", func(t *testing.T) {
		baseEvents := packetizeFrames530(frames, 3000)
		var filtered []*replayPktEvent
		var idr30Total int
		for _, ev := range baseEvents {
			if ev.frameIdx == 30 {
				idr30Total++
			}
		}
		// Verify how many packets are in frames 2..29 vs IDR 30 and trace StallResetMidIDR drops cleanly.
		var f2to29pkts int
		for _, ev := range baseEvents {
			if ev.frameIdx >= 2 && ev.frameIdx <= 29 {
				f2to29pkts++
			}
		}
		t.Logf("DEBUG StallMidIDR: frames[2..29] pkts=%d idr30 pkts=%d", f2to29pkts, idr30Total)
		// Drop packet 0 only on frames 15..29 (from t=1.00s onwards), delay IDR 30 across 2.75s..3.15s? Wait: keep 2.0s IDR at 1.90s..2.25s?
		// Hold lastFrame at t=0.30s by opening gap at frame 15? No: drop pkt 0 on frames 2..29 AND drop pkt 0 on frames 2..29 where frame 15..29 take longer than 1200ms OR test real 1800ms stall mid-IDR cleanly:
		for _, ev := range baseEvents {
			// Gap opens at frame 18 (t=1.20s, last emitted=frame 17 at t=1.15s); IDR 30 arrives slowly across 2.75s..3.25s -> 1.80s stall fires at 2.95s mid-IDR!
			if ev.frameIdx >= 18 && ev.frameIdx <= 29 && ev.pktIdx == 0 {
				continue
			}
			cp := *ev
			if ev.frameIdx >= 18 {
				// Shift 18..29 slightly or put IDR 30 across 2.75s..3.25s (simulating congested 1.5s burst + mid-IDR 1.8s stall!)
				if ev.frameIdx == 30 {
					cp.arrive = 2750*time.Millisecond + time.Duration(ev.pktIdx)*500*time.Millisecond/time.Duration(max(1, idr30Total))
				} else if ev.frameIdx > 30 {
					cp.arrive += 1100 * time.Millisecond
				} else {
					// frames 18..29 span 2.00s..2.70s so 1200ms reorder window does NOT expire before 2.95s!
					cp.arrive += 800 * time.Millisecond
				}
			}
			filtered = append(filtered, &cp)
		}
		leg, _ := runPipelineVariant(t, filtered, prof, false)
		fix, _ := runPipelineVariant(t, filtered, prof, true)
		_, legHadIDR30 := leg.idrEmittedAt[30]
		_, fixHadIDR30 := fix.idrEmittedAt[30]
		t.Logf("BEFORE StallMidIDR: idr30_emitted=%t asm_drops=%d resets=%d frames=%d max_gap=%v decode_errs=%d",
			legHadIDR30, leg.asmDrops, leg.stallResets, leg.emittedFrames, leg.maxOutputGap.Round(time.Millisecond), leg.decodeErrors)
		t.Logf("AFTER  StallMidIDR: idr30_emitted=%t asm_drops=%d resets=%d frames=%d max_gap=%v decode_errs=%d 2k=%d",
			fixHadIDR30, fix.asmDrops, fix.stallResets, fix.emittedFrames, fix.maxOutputGap.Round(time.Millisecond), fix.decodeErrors, fix.verified2KCount)
		if !fixHadIDR30 {
			t.Fatal("expected AFTER PruneOnStall to preserve mid-IDR frame 30")
		}
		if legHadIDR30 {
			t.Fatal("expected BEFORE legacy hard reset to destroy mid-IDR frame 30")
		}
		if fix.decodeErrors != 0 || fix.verified2KCount != fix.emittedFrames || fix.maxOutputGap >= leg.maxOutputGap {
			t.Fatalf("unexpected AFTER result: errs=%d max_gap=%v vs before=%v", fix.decodeErrors, fix.maxOutputGap, leg.maxOutputGap)
		}
	})

	// Scenario 4: Bounded Pion NACK generator avoids RTCP duplicate flooding over TURN RTT.
	t.Run("NackGeneratorRTTComparison", func(t *testing.T) {
		measureNack := func(label string, opts ...nack.GeneratorOption) (nackRTCPs, nackSeqs int64) {
			factory, err := nack.NewGeneratorInterceptor(opts...)
			if err != nil {
				t.Fatal(err)
			}
			ic, err := factory.NewInterceptor("")
			if err != nil {
				t.Fatal(err)
			}
			defer ic.Close()

			var rtcpPkts, seqCount atomic.Int64
			nackWriter := interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, _ interceptor.Attributes) (int, error) {
				rtcpPkts.Add(1)
				for _, p := range pkts {
					if tn, ok := p.(*rtcp.TransportLayerNack); ok {
						for _, n := range tn.Nacks {
							seqCount.Add(int64(len(n.PacketList())))
						}
					}
				}
				return 0, nil
			})
			_ = ic.BindRTCPWriter(nackWriter)
			streamInfo := &interceptor.StreamInfo{
				SSRC:         12345,
				RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
			}
			reader := ic.BindRemoteStream(streamInfo, interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
				return len(b), a, nil
			}))
			defer ic.UnbindRemoteStream(streamInfo)

			feedSeq := func(seq uint16) {
				raw, err := (&rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: 121, SequenceNumber: seq, SSRC: 12345},
					Payload: []byte{0x01, 0x02},
				}).Marshal()
				if err != nil {
					t.Fatal(err)
				}
				_, _, _ = reader.Read(raw, nil)
			}
			var delayed []uint16
			for seq := uint16(1); seq <= 300; seq++ {
				if seq >= 10 && seq <= 13 {
					delayed = append(delayed, seq)
					continue
				}
				if seq == 50 || seq == 51 {
					continue
				}
				feedSeq(seq)
				if seq == 70 {
					// Simulate 550ms TURN congestion RTT before retransmissions arrive (must still get NACKed!).
					time.Sleep(550 * time.Millisecond)
					for _, d := range delayed {
						feedSeq(d)
					}
				}
				if seq%30 == 0 {
					time.Sleep(25 * time.Millisecond)
				}
			}
			time.Sleep(1400 * time.Millisecond)
			nackRTCPs, nackSeqs = rtcpPkts.Load(), seqCount.Load()
			t.Logf("NACK %-40s rtcp_pkts=%d total_nack_seqs=%d", label, nackRTCPs, nackSeqs)
			return nackRTCPs, nackSeqs
		}
		defRTCP, defSeqs := measureNack("Pion default (max=0 unlimited, 100ms, skip=0)")
		bndRTCP, bndSeqs := measureNack("Bounded 1s (max=10, 100ms, skip=0)",
			nack.GeneratorSize(512),
			nack.GeneratorMaxNacksPerPacket(10),
			nack.GeneratorInterval(100*time.Millisecond),
			nack.GeneratorSkipLastN(0),
		)
		if bndRTCP >= defRTCP || bndSeqs >= defSeqs {
			t.Fatalf("expected bounded 1s NACK to stop stale repeats vs default: bounded=(%d,%d) vs default=(%d,%d)", bndRTCP, bndSeqs, defRTCP, defSeqs)
		}
	})

	// Scenario 5: Slow 1.25s-paced 2304x1296 IDR with a late tail packet delayed 450ms past the next P-frame (123534 dynamics).
	t.Run("SlowIDRTailGapAndNoPrematureDrop", func(t *testing.T) {
		baseEvents := packetizeFrames530(frames, 4000)
		var idr30Pkts []*replayPktEvent
		for _, ev := range baseEvents {
			if ev.frameIdx == 30 {
				idr30Pkts = append(idr30Pkts, ev)
			}
		}
		if len(idr30Pkts) < 20 {
			t.Fatalf("expected >=20 packets for IDR 30, got %d", len(idr30Pkts))
		}
		tailGapIdx := len(idr30Pkts) - 5
		const idrPace = 1250 * time.Millisecond
		var reordered []*replayPktEvent
		for _, ev := range baseEvents {
			cp := *ev
			switch {
			case ev.frameIdx < 30:
				reordered = append(reordered, &cp)
			case ev.frameIdx == 30:
				base30 := 2000*time.Millisecond + time.Duration(ev.pktIdx)*idrPace/time.Duration(len(idr30Pkts))
				if ev.pktIdx == tailGapIdx {
					cp.arrive = base30 + 450*time.Millisecond
				} else {
					cp.arrive = base30
				}
				reordered = append(reordered, &cp)
			default:
				cp.arrive = 3280*time.Millisecond + time.Duration(ev.frameIdx-31)*(time.Second/15) + time.Duration(ev.pktIdx)*time.Millisecond
				reordered = append(reordered, &cp)
			}
		}
		sort.SliceStable(reordered, func(i, j int) bool { return reordered[i].arrive < reordered[j].arrive })

		var diagCheck rtpDiagTracker
		asmCheck := newLiveH264Assembler(&diagCheck, 1200*time.Millisecond)
		base := time.Unix(1700000000, 0)
		var maxGapAgeBeforeHole time.Duration
		holeTime := 2000*time.Millisecond + time.Duration(tailGapIdx+1)*idrPace/time.Duration(len(idr30Pkts))
		for _, ev := range reordered {
			now := base.Add(ev.arrive)
			pCopy := *ev.pkt
			asmCheck.Push(&pCopy, now)
			for s := asmCheck.Pop(now, false); s != nil; s = asmCheck.Pop(now, false) {
			}
			if ev.arrive < holeTime && ev.frameIdx == 30 {
				if age := asmCheck.oldestGapAge(now); age > maxGapAgeBeforeHole {
					maxGapAgeBeforeHole = age
				}
			}
		}
		if maxGapAgeBeforeHole != 0 {
			t.Fatalf("expected oldestGapAge==0 during contiguous slow IDR before any hole, got %v", maxGapAgeBeforeHole)
		}

		res, _ := runPipelineVariant(t, reordered, prof, true)
		idr30At, hasIDR30 := res.idrEmittedAt[30]
		t.Logf("SlowIDRTailGap: idr30_emitted=%t idr30_at=%v asm_drops=%d asm_late=%d frames=%d max_gap=%v decode_errs=%d",
			hasIDR30, idr30At.Round(time.Millisecond), res.asmDrops, res.asmLate, res.emittedFrames, res.maxOutputGap.Round(time.Millisecond), res.decodeErrors)
		if !hasIDR30 || res.decodeErrors != 0 || res.verified2KCount != res.emittedFrames {
			t.Fatalf("SlowIDRTailGap failed: idr30=%t errs=%d (%s) 2k=%d/%d", hasIDR30, res.decodeErrors, res.decodeErrText, res.verified2KCount, res.emittedFrames)
		}
		if res.asmDrops != 0 || res.asmLate != 0 {
			t.Fatalf("expected 0 drops/late when tail NACK arrives 450ms after hole in 1.25s IDR, got drops=%d late=%d", res.asmDrops, res.asmLate)
		}
	})
}

func TestConfigureLiveInterceptorsAndAssemblerWrap(t *testing.T) {
	// Verify configureLiveInterceptors registers NACK + NACK/PLI cleanly with Pion MediaEngine.
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f",
		}, PayloadType: 121,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	reg := &interceptor.Registry{}
	if err := configureLiveInterceptors(me, reg); err != nil {
		t.Fatalf("configureLiveInterceptors failed: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg))
	peer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(offer.SDP), []byte("a=rtcp-fb:121 nack")) || !bytes.Contains([]byte(offer.SDP), []byte("a=rtcp-fb:121 nack pli")) {
		t.Fatalf("expected nack and nack pli in SDP offer:\n%s", offer.SDP)
	}

	// Verify uint16 wraparound (65530 -> 0 -> 20) and duplicate suppression in liveH264Assembler.
	var diag rtpDiagTracker
	asm := newLiveH264Assembler(&diag, 1200*time.Millisecond)
	base := time.Unix(1700000000, 0)
	seq := uint16(65530)
	emitted := 0
	for frameIdx := 0; frameIdx < 10; frameIdx++ {
		ts := uint32(90000 + frameIdx*6000)
		for frag := 0; frag < 3; frag++ {
			pl := []byte{0x41, 0x9a, 0x01}
			if frameIdx == 0 && frag == 0 {
				pl = []byte{0x78, 0x00, 0x04, 0x67, 100, 0, 50, 0x00, 0x02, 0x68, 2, 0x00, 0x04, 0x65, 0x88, 0x84, 0x21}
			}
			now := base.Add(time.Duration(frameIdx*66+frag*5) * time.Millisecond)
			pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: frag == 2}, Payload: pl}
			if !asm.Push(pkt, now) {
				t.Fatalf("failed push at seq=%d", seq)
			}
			if asm.Push(pkt, now.Add(2*time.Millisecond)) {
				t.Fatalf("expected duplicate packet seq=%d to be rejected", seq)
			}
			for s := asm.Pop(now, false); s != nil; s = asm.Pop(now, false) {
				emitted++
			}
			seq++
		}
	}
	// Inject a stale packet from well before wrap (65530, 30 slots back behind frontier).
	if asm.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 65530, Timestamp: 90000, Marker: false}, Payload: []byte{0x41, 0x01}}, base.Add(time.Second)) {
		t.Fatal("expected pre-wrap stale packet behind frontier to be rejected")
	}
	if emitted != 10 {
		t.Fatalf("expected 10 emitted frames across wraparound, got %d", emitted)
	}

	// Verify markerless truncated AU advances cleanly when a newer RTP timestamp arrives without a sequence gap.
	_ = asm.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: 160000, Marker: false}, Payload: []byte{0x41, 0x01}}, base.Add(1100*time.Millisecond))
	seq++
	_ = asm.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: 166000, Marker: true}, Payload: []byte{0x41, 0x02}}, base.Add(1166*time.Millisecond))
	pop := asm.Pop(base.Add(1166*time.Millisecond), false)
	if pop == nil || pop.PacketTimestamp != 166000 {
		t.Fatalf("expected markerless head AU to drop and emit TS=166000 cleanly, got %+v", pop)
	}
}
