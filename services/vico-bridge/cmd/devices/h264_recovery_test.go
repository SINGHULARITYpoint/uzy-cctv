package devices

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestLiveH264RecoversFromLostFragment(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required for the full H.264 decode check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	input, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x96:rate=10",
		"-frames:v", "30", "-c:v", "libx264", "-profile:v", "high", "-level:v", "5.0",
		"-x264-params", "keyint=10:scenecut=0:bframes=0:aud=1:repeat-headers=1", "-f", "h264", "pipe:1").Output()
	if err != nil {
		t.Fatalf("encode synthetic H.264: %v", err)
	}
	var accessUnits [][]byte
	for _, nalu := range annexBNALUs(input) {
		typ := nalu[0] & 0x1f
		if typ == 9 {
			accessUnits = append(accessUnits, nil)
		}
		if len(accessUnits) == 0 {
			t.Fatal("expected an access-unit delimiter before the first NAL")
		}
		// Recovery must also work when the sender omits repeated SPS/PPS.
		if len(accessUnits) > 1 && (typ == 7 || typ == 8) {
			continue
		}
		index := len(accessUnits) - 1
		accessUnits[index] = append(accessUnits[index], 0, 0, 0, 1)
		accessUnits[index] = append(accessUnits[index], nalu...)
	}
	if len(accessUnits) != 30 {
		t.Fatalf("expected 30 synthetic access units, got %d", len(accessUnits))
	}

	builder := newLiveH264Builder()
	payloader := &codecs.H264Payloader{}
	var recovery h264LiveRecovery
	var output bytes.Buffer
	var sequence uint16 = 65500
	dropped := false
	gapAt, resumedAt := -1, -1
	var frames []int
	for i, unit := range accessUnits {
		payloads := payloader.Payload(100, unit)
		for j, payload := range payloads {
			packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: uint32(i) * 9000, Marker: j == len(payloads)-1}, Payload: payload}
			sequence++
			if i == 3 && !dropped && len(payload) >= 3 && payload[0]&0x1f == 28 && payload[1] == 1 {
				dropped = true
				continue
			}
			builder.Push(packet)
			for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
				data, frame, gap, recovered := recovery.push(sample)
				if gap {
					if gapAt >= 0 {
						t.Fatal("reported more than one gap for one missing fragment")
					}
					gapAt = i
				}
				if recovered {
					resumedAt = int(sample.PacketTimestamp / 9000)
				}
				if frame {
					frames = append(frames, int(sample.PacketTimestamp/9000))
				}
				output.Write(data)
			}
		}
	}
	if !dropped || gapAt < 0 || gapAt > 9 || resumedAt != 10 {
		t.Fatalf("dropped=%t gap detected by input frame %d, resumed at source frame %d", dropped, gapAt, resumedAt)
	}
	for i, frame := range frames {
		if frame >= 3 && frame < 10 || i > 0 && frame <= frames[i-1] {
			t.Fatalf("damaged, dependent, or nonmonotonic frame escaped recovery: %v", frames)
		}
	}
	if len(frames) != 22 {
		t.Fatalf("expected 22 frames before and after the gap, got %v", frames)
	}
	decode := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-xerror", "-err_detect", "explode", "-f", "h264", "-i", "pipe:0", "-f", "null", "-")
	decode.Stdin = &output
	if log, err := decode.CombinedOutput(); err != nil || len(log) != 0 {
		t.Fatalf("decode recovered H.264: %v\n%s", err, log)
	}
}

func TestLiveH264SignalsFrameStallOnce(t *testing.T) {
	var recovery h264LiveRecovery
	stamp := time.Unix(1, 0)
	recovery.gate.started = true
	recovery.lastFrame = stamp
	if recovery.stalled(stamp.Add(liveFrameStall - time.Millisecond)) {
		t.Fatal("requested recovery before the stall deadline")
	}
	if !recovery.stalled(stamp.Add(liveFrameStall)) || recovery.stalled(stamp.Add(2*liveFrameStall)) {
		t.Fatal("expected one recovery request at the stall deadline")
	}
}

func TestLiveH264RejectsFrameWithoutMarker(t *testing.T) {
	var recovery h264LiveRecovery
	recovery.gate.started = true
	sample := &media.Sample{Data: []byte{0, 0, 0, 1, 0x65, 1}, RTPHeaders: []*rtp.Header{{Marker: false}}}
	data, frame, gap, recovered := recovery.push(sample)
	if len(data) != 0 || frame || !gap || recovered {
		t.Fatal("accepted a keyframe with a missing frame boundary")
	}
}

func TestLiveH264AcceptsCompleteIDRAfterDroppedPackets(t *testing.T) {
	var recovery h264LiveRecovery
	spsPPS := []byte{0, 0, 0, 1, 0x67, 100, 0, 50, 0, 0, 0, 1, 0x68, 2}
	idr := append(append([]byte(nil), spsPPS...), 0, 0, 0, 1, 0x65, 1)

	// Start stream even if preview packets were dropped immediately before the first main IDR.
	data, frame, gap, recovered := recovery.push(&media.Sample{
		Data:               idr,
		PacketTimestamp:    90000,
		PrevDroppedPackets: 3,
		RTPHeaders:         []*rtp.Header{{Marker: true}},
	})
	if len(data) == 0 || !frame || gap || recovered {
		t.Fatalf("expected initial IDR after preview drop to start stream: len=%d frame=%t gap=%t recovered=%t", len(data), frame, gap, recovered)
	}

	// Enter recovery on an incomplete P-frame, then recover on a complete IDR that also reports prior dropped packets.
	_, frame, gap, _ = recovery.push(&media.Sample{
		Data:            []byte{0, 0, 0, 1, 0x41, 1},
		PacketTimestamp: 96000,
		RTPHeaders:      []*rtp.Header{{Marker: false}},
	})
	if frame || !gap {
		t.Fatalf("expected incomplete P-frame to start gap: frame=%t gap=%t", frame, gap)
	}
	data, frame, gap, recovered = recovery.push(&media.Sample{
		Data:               []byte{0, 0, 0, 1, 0x65, 2},
		PacketTimestamp:    102000,
		PrevDroppedPackets: 5,
		RTPHeaders:         []*rtp.Header{{Marker: true}},
	})
	if len(data) == 0 || !frame || gap || !recovered {
		t.Fatalf("expected complete IDR after dropped packets to recover stream: len=%d frame=%t gap=%t recovered=%t", len(data), frame, gap, recovered)
	}
}

func TestRTPDiagDistinguishesInputSequenceGapFromLateAssemblyDrop(t *testing.T) {
	payloader := &codecs.H264Payloader{}
	spsPPS := []byte{0, 0, 0, 1, 0x67, 100, 0, 50, 0, 0, 0, 1, 0x68, 2}
	idr := append(append([]byte(nil), spsPPS...), 0, 0, 0, 1, 0x65)
	for i := 0; i < 300; i++ {
		idr = append(idr, byte(i+1))
	}

	// Case 1: One fragment never arrives at RTP input.
	var diagMissing rtpDiagTracker
	builderMissing := newLiveH264BuilderWithDiag(&diagMissing)
	var recoveryMissing h264LiveRecovery
	var seq uint16 = 1000
	for frameIdx := 0; frameIdx < 12; frameIdx++ {
		payloads := payloader.Payload(100, idr)
		ts := uint32(frameIdx) * 9000
		for j, p := range payloads {
			pkt := &rtp.Packet{
				Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: j == len(payloads)-1},
				Payload: p,
			}
			seq++
			if frameIdx == 1 && j == 1 {
				continue // lost before reaching RTP input
			}
			diagMissing.onInputPacket(pkt)
			builderMissing.Push(pkt)
			for sample := builderMissing.Pop(); sample != nil; sample = builderMissing.Pop() {
				lateSample := recoveryMissing.hasTimestamp && int32(sample.PacketTimestamp-recoveryMissing.lastRTP) < 0
				completeSample := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
				diagMissing.onSamplePopped(sample, lateSample || !completeSample, lateSample)
				recoveryMissing.push(sample)
			}
			diagMissing.afterPopDrain()
		}
	}
	if diagMissing.seqGaps.Load() != 1 || diagMissing.seqMissing.Load() != 1 || diagMissing.seqLate.Load() != 0 ||
		diagMissing.asmLate.Load() != 0 || diagMissing.asmDrops.Load() == 0 {
		t.Fatalf("unexpected missing-packet diag: gaps=%d missing=%d late_in=%d asm_drops=%d asm_late=%d",
			diagMissing.seqGaps.Load(), diagMissing.seqMissing.Load(), diagMissing.seqLate.Load(),
			diagMissing.asmDrops.Load(), diagMissing.asmLate.Load())
	}

	// Case 2: The skipped fragment arrives late, after the 500ms reorder window has already advanced assembly.
	var diagLate rtpDiagTracker
	builderLate := newLiveH264BuilderWithDiag(&diagLate)
	var recoveryLate h264LiveRecovery
	seq = 2000
	var delayed *rtp.Packet
	for frameIdx := 0; frameIdx < 12; frameIdx++ {
		payloads := payloader.Payload(100, idr)
		ts := uint32(frameIdx) * 9000
		for j, p := range payloads {
			pkt := &rtp.Packet{
				Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: j == len(payloads)-1},
				Payload: p,
			}
			seq++
			if frameIdx == 1 && j == 1 {
				delayed = pkt
				continue
			}
			diagLate.onInputPacket(pkt)
			builderLate.Push(pkt)
			for sample := builderLate.Pop(); sample != nil; sample = builderLate.Pop() {
				lateSample := recoveryLate.hasTimestamp && int32(sample.PacketTimestamp-recoveryLate.lastRTP) < 0
				completeSample := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
				diagLate.onSamplePopped(sample, lateSample || !completeSample, lateSample)
				recoveryLate.push(sample)
			}
			diagLate.afterPopDrain()
		}
		if frameIdx == 9 && delayed != nil {
			diagLate.onInputPacket(delayed)
			builderLate.Push(delayed)
			for sample := builderLate.Pop(); sample != nil; sample = builderLate.Pop() {
				lateSample := recoveryLate.hasTimestamp && int32(sample.PacketTimestamp-recoveryLate.lastRTP) < 0
				completeSample := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
				diagLate.onSamplePopped(sample, lateSample || !completeSample, lateSample)
				recoveryLate.push(sample)
			}
			diagLate.afterPopDrain()
		}
	}
	if diagLate.seqGaps.Load() != 1 || diagLate.seqMissing.Load() != 0 || diagLate.seqLate.Load() != 1 ||
		diagLate.asmLate.Load() != 1 || diagLate.asmDrops.Load() == 0 {
		t.Fatalf("unexpected late-packet diag: gaps=%d missing=%d late_in=%d asm_drops=%d asm_late=%d",
			diagLate.seqGaps.Load(), diagLate.seqMissing.Load(), diagLate.seqLate.Load(),
			diagLate.asmDrops.Load(), diagLate.asmLate.Load())
	}
}
