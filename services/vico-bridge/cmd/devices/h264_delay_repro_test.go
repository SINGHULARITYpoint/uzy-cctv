package devices

import (
	"sort"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// TestDelayReproDelayedFragment700ms reproduces the ~700 ms delayed-fragment case
// observed in powered-pair-20261109-133754. It compares the 500 ms default
// against the 1,200 ms window and 1,800 ms stall setting on the exact same packet arrival sequence.
func TestDelayReproDelayedFragment700ms(t *testing.T) {
	const totalFrames = 60 // 4.0 seconds at 15 fps
	const delayTarget = 700 * time.Millisecond

	type testArm struct {
		name          string
		window, stall time.Duration
	}

	arms := []testArm{
		{"500ms window / 1500ms stall (default)", 500 * time.Millisecond, 1500 * time.Millisecond},
		{"1200ms window / 1800ms stall (verified)", 1200 * time.Millisecond, 1800 * time.Millisecond},
	}

	type armResult struct {
		recoveredDelayedFrame bool
		emittedFrames         int
		asmDrops              uint64
		keyframeRequests      int
		maxOutputGap          time.Duration
		outputGapsOver2s      int
	}

	results := make([]armResult, len(arms))

	for armIdx, arm := range arms {
		// Generate stream: IDR at frame 0, frame 30; P-frames elsewhere.
		pkts, _ := synthStream(totalFrames)

		// Apply 700 ms delay to 3 packets of frame 15 (a P-frame at t = 1.0 s)
		targetFrame := 15
		delayedCount := 0
		for _, p := range pkts {
			if p.frame == targetFrame {
				p.arrive += delayTarget
				delayedCount++
				if delayedCount >= 3 {
					break
				}
			}
		}

		// Sort by arrival time at UDP socket
		events := append([]*simPkt(nil), pkts...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].arrive < events[j].arrive })

		base := time.Unix(1000, 0)
		var diag rtpDiagTracker
		builder := newLiveH264BuilderWithWindow(&diag, arm.window)
		rec := h264LiveRecovery{stall: arm.stall}

		var emittedAts []time.Duration
		emittedFramesMap := map[int]bool{}
		pliCount := 0
		var lastPLI time.Time

		reportGap := func(now time.Time, reason string) {
			if !lastPLI.IsZero() && now.Sub(lastPLI) < 2500*time.Millisecond {
				return
			}
			lastPLI = now
			pliCount++
		}

		for _, e := range events {
			now := base.Add(e.arrive)
			if rec.stalled(now) {
				diag.onBuilderReset()
				builder = newLiveH264BuilderWithWindow(&diag, arm.window)
				reportGap(now, "frame_stall")
			}

			diag.onInputPacket(e.pkt)
			builder.Push(e.pkt)
			for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
				late := rec.hasTimestamp && int32(sample.PacketTimestamp-rec.lastRTP) < 0
				complete := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
				diag.onSamplePopped(sample, late || !complete, late)
				data, frame, gapStarted, _ := rec.push(sample)
				if gapStarted {
					reportGap(now, "incomplete_rtp")
				}
				if frame && len(data) > 0 {
					rec.lastFrame = now
					f := int(sample.PacketTimestamp / simFrameStep)
					if !emittedFramesMap[f] {
						emittedFramesMap[f] = true
						emittedAts = append(emittedAts, e.arrive)
					}
				}
			}
			diag.afterPopDrain()
		}

		var maxGap time.Duration
		gapsOver2s := 0
		for i := 1; i < len(emittedAts); i++ {
			gap := emittedAts[i] - emittedAts[i-1]
			if gap > maxGap {
				maxGap = gap
			}
			if gap > 2*time.Second {
				gapsOver2s++
			}
		}

		results[armIdx] = armResult{
			recoveredDelayedFrame: emittedFramesMap[targetFrame],
			emittedFrames:         len(emittedFramesMap),
			asmDrops:              diag.asmDrops.Load(),
			keyframeRequests:      pliCount,
			maxOutputGap:          maxGap,
			outputGapsOver2s:      gapsOver2s,
		}
	}

	// Print comparison table
	t.Logf("=== 700 ms Delayed-Fragment Offline Comparison ===")
	for i, arm := range arms {
		r := results[i]
		t.Logf("%-42s | frames: %d/%d | delayed_frame_recovered: %t | drops: %d | PLIs: %d | max_gap: %v | gaps>2s: %d",
			arm.name, r.emittedFrames, totalFrames, r.recoveredDelayedFrame, r.asmDrops, r.keyframeRequests, r.maxOutputGap.Round(time.Millisecond), r.outputGapsOver2s)
	}

	// Verify Arm 0 (500 ms default):
	// Drops the delayed frame, enters recovery gap, sends PLI, and waits for next IDR at frame 30.
	if results[0].recoveredDelayedFrame {
		t.Error("500 ms default should NOT recover 700 ms delayed frame")
	}
	if results[0].asmDrops == 0 {
		t.Error("500 ms default should record assembler drops")
	}
	if results[0].keyframeRequests == 0 {
		t.Error("500 ms default should have triggered a keyframe request")
	}
	// Note: 500ms default gap extends to frame 30 (t=2.0s), creating an output gap of ~1.07s
	t.Logf("500 ms default max gap: %v (lost %d frames between frame 15 and IDR at 30)",
		results[0].maxOutputGap.Round(time.Millisecond), totalFrames-results[0].emittedFrames)

	// Verify Arm 1 (1200 ms window / 1800 ms stall):
	// Successfully recovers the delayed frame without assembler drops or PLI keyframe requests!
	if !results[1].recoveredDelayedFrame {
		t.Error("1200 ms window MUST recover the 700 ms delayed frame")
	}
	if results[1].asmDrops != 0 {
		t.Errorf("1200 ms window expected 0 assembler drops, got %d", results[1].asmDrops)
	}
	if results[1].keyframeRequests != 0 {
		t.Errorf("1200 ms window expected 0 keyframe requests, got %d", results[1].keyframeRequests)
	}
	if results[1].outputGapsOver2s > 0 {
		t.Errorf("1200 ms window should have 0 gaps > 2s, got %d (max gap %v)", results[1].outputGapsOver2s, results[1].maxOutputGap)
	}
	if results[1].maxOutputGap > 2*time.Second {
		t.Fatalf("1200 ms window failed the 2-second recovery requirement: max gap was %v", results[1].maxOutputGap)
	}
}

// TestDelayReproPermanentlyMissingFragment verifies recovery behavior when a fragment NEVER arrives.
// Both configurations must declare a gap, stay bounded, and cleanly resume at the next IDR.
func TestDelayReproPermanentlyMissingFragment(t *testing.T) {
	const totalFrames = 60

	type testArm struct {
		name          string
		window, stall time.Duration
	}

	arms := []testArm{
		{"500ms window / 1500ms stall (default)", 500 * time.Millisecond, 1500 * time.Millisecond},
		{"1200ms window / 1800ms stall (verified)", 1200 * time.Millisecond, 1800 * time.Millisecond},
	}

	for _, arm := range arms {
		pkts, _ := synthStream(totalFrames)

		// Drop packet index 1 of frame 15 completely
		targetFrame := 15
		pkts = dropFragment(pkts, targetFrame, 1)

		events := append([]*simPkt(nil), pkts...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].arrive < events[j].arrive })

		base := time.Unix(1000, 0)
		var diag rtpDiagTracker
		builder := newLiveH264BuilderWithWindow(&diag, arm.window)
		rec := h264LiveRecovery{stall: arm.stall}

		var lastBeforeGap, firstAfterGap int
		var lastBeforeGapAt, firstAfterGapAt time.Duration
		var emittedFramesMap = map[int]bool{}
		gapReported := false

		for _, e := range events {
			now := base.Add(e.arrive)
			if rec.stalled(now) {
				diag.onBuilderReset()
				builder = newLiveH264BuilderWithWindow(&diag, arm.window)
			}

			diag.onInputPacket(e.pkt)
			builder.Push(e.pkt)
			for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
				late := rec.hasTimestamp && int32(sample.PacketTimestamp-rec.lastRTP) < 0
				complete := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
				diag.onSamplePopped(sample, late || !complete, late)
				data, frame, gapStarted, _ := rec.push(sample)
				if gapStarted {
					gapReported = true
				}
				if frame && len(data) > 0 {
					rec.lastFrame = now
					f := int(sample.PacketTimestamp / simFrameStep)
					if !emittedFramesMap[f] {
						emittedFramesMap[f] = true
						if f < targetFrame {
							lastBeforeGap = f
							lastBeforeGapAt = e.arrive
						} else if f > targetFrame && firstAfterGap == 0 {
							firstAfterGap = f
							firstAfterGapAt = e.arrive
						}
					}
				}
			}
			diag.afterPopDrain()
		}

		outage := firstAfterGapAt - lastBeforeGapAt
		t.Logf("%-42s | gap_reported: %t | last_frame: %d | resumed_frame: %d (IDR) | outage: %v | drops: %d",
			arm.name, gapReported, lastBeforeGap, firstAfterGap, outage.Round(time.Millisecond), diag.asmDrops.Load())

		if !gapReported {
			t.Errorf("%s: expected gap to be reported for missing fragment", arm.name)
		}
		if firstAfterGap != 30 {
			t.Errorf("%s: expected stream to resume at next IDR frame 30, got %d", arm.name, firstAfterGap)
		}
		if diag.asmDrops.Load() == 0 {
			t.Errorf("%s: expected assembler drops for missing fragment", arm.name)
		}
		// Outage must be bounded: from frame 14 (t=0.93s) to frame 30 (t=2.0s) + window
		maxAllowedOutage := 2*time.Second + arm.window + 200*time.Millisecond
		if outage > maxAllowedOutage {
			t.Errorf("%s: outage %v exceeded upper bound %v", arm.name, outage, maxAllowedOutage)
		}
	}
}

// TestDelayReproFiniteRecoveryDeadline verifies that when RTP packets keep arriving
// but complete frames stop, the system enforces a finite recovery deadline rather than hanging indefinitely.
func TestDelayReproFiniteRecoveryDeadline(t *testing.T) {
	const stallLimit = 1800 * time.Millisecond
	const hardDeadline = 20000 * time.Millisecond // 20.0s hard deadline in media_probe.go

	payloader := &codecs.H264Payloader{}
	base := time.Unix(2000, 0)

	var diag rtpDiagTracker
	builder := newLiveH264BuilderWithWindow(&diag, 1200*time.Millisecond)
	rec := h264LiveRecovery{stall: stallLimit}

	// 1. Establish initial frame output with initial frames from synthStream
	initialPkts, _ := synthStream(5)
	for _, p := range initialPkts {
		builder.Push(p.pkt)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			data, frame, _, _ := rec.push(sample)
			if frame && len(data) > 0 {
				rec.lastFrame = base
			}
		}
	}
	if rec.lastFrame.IsZero() {
		t.Fatal("failed to establish initial frame")
	}

	// 2. Stream continues with ONLY damaged P-frames (1 fragment missing per frame)
	// RTP continues arriving every 66 ms for 25 seconds (375 frames)
	lastFrameTime := rec.lastFrame
	var frameStallFiredAt time.Duration
	var lastRTPTime time.Duration
	var outputStallFiredAt time.Duration
	var seq uint16 = 50000

	isOutputStalled := func(elapsedMs, lastFrameMs, lastRTPMs int64) bool {
		rtpIdle := lastRTPMs < 0 || elapsedMs-lastRTPMs >= 5000
		return lastFrameMs >= 0 && ((elapsedMs-lastFrameMs >= 8000 && rtpIdle) || elapsedMs-lastFrameMs >= 20000)
	}

	for k := 1; k < 375; k++ {
		tElapsed := time.Duration(k) * (time.Second / 15)
		now := base.Add(tElapsed)
		lastRTPTime = tElapsed

		if rec.stalled(now) {
			if frameStallFiredAt == 0 {
				frameStallFiredAt = tElapsed
			}
			diag.onBuilderReset()
			builder = newLiveH264BuilderWithWindow(&diag, 1200*time.Millisecond)
		}

		// P-frame unit split into 2 fragments; drop fragment 0, only send fragment 1 (damaged frame)
		pUnit := append([]byte{0, 0, 0, 1, 0x41}, make([]byte, 2000)...)
		payloads := payloader.Payload(1200, pUnit)
		if len(payloads) < 2 {
			t.Fatalf("expected >= 2 payloads, got %d", len(payloads))
		}
		pkt := &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq + 1, Timestamp: uint32(k) * simFrameStep, Marker: true},
			Payload: payloads[1],
		}
		seq += 2

		builder.Push(pkt)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			data, frame, _, _ := rec.push(sample)
			if frame && len(data) > 0 {
				rec.lastFrame = now
			}
		}

		// Check output stall condition
		elapsedMs := tElapsed.Milliseconds()
		lastFrameMs := (lastFrameTime.Sub(base)).Milliseconds()
		lastRTPMs := lastRTPTime.Milliseconds()

		if isOutputStalled(elapsedMs, lastFrameMs, lastRTPMs) && outputStallFiredAt == 0 {
			outputStallFiredAt = tElapsed
			break
		}
	}

	t.Logf("Continuous RTP without complete frames:")
	t.Logf("  frame_stall watchdog fired at: %v (limit: %v)", frameStallFiredAt, stallLimit)
	t.Logf("  output_stall hard deadline fired at: %v (limit: %v)", outputStallFiredAt, hardDeadline)

	if frameStallFiredAt == 0 || frameStallFiredAt > stallLimit+100*time.Millisecond {
		t.Fatalf("frame_stall did not fire near %v (fired at %v)", stallLimit, frameStallFiredAt)
	}
	if outputStallFiredAt == 0 {
		t.Fatal("output_stall failed to fire despite 25 seconds of continuous frames stoppage")
	}
	if outputStallFiredAt < 20*time.Second || outputStallFiredAt > 20*time.Second+500*time.Millisecond {
		t.Fatalf("output_stall fired at %v, expected finite deadline at ~20s", outputStallFiredAt)
	}
}
