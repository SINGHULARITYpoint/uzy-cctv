package devices

import (
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type rtcpObserverFactory struct {
	onWrite func([]rtcp.Packet)
}

func (f rtcpObserverFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &rtcpObserverInterceptor{onWrite: f.onWrite}, nil
}

type rtcpObserverInterceptor struct {
	interceptor.NoOp
	onWrite func([]rtcp.Packet)
}

func (i *rtcpObserverInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	if i.onWrite == nil {
		return writer
	}
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		i.onWrite(pkts)
		return writer.Write(pkts, attributes)
	})
}

// configureLiveInterceptors mirrors webrtc.RegisterDefaultInterceptors while bounding
// the NACK generator for TURN/Wi-Fi RTT so dead packets cannot trigger multi-second
// duplicate retransmission storms.
func configureLiveInterceptors(mediaEngine *webrtc.MediaEngine, registry *interceptor.Registry) error {
	return configureLiveInterceptorsWithObserver(mediaEngine, registry, nil)
}

func configureLiveInterceptorsWithObserver(mediaEngine *webrtc.MediaEngine, registry *interceptor.Registry, onRTCP func([]rtcp.Packet)) error {
	if onRTCP != nil {
		registry.Add(rtcpObserverFactory{onWrite: onRTCP})
	}
	generator, err := nack.NewGeneratorInterceptor(
		nack.GeneratorSize(512),
		nack.GeneratorInterval(100*time.Millisecond),
		nack.GeneratorSkipLastN(0),
		nack.GeneratorMaxNacksPerPacket(10),
	)
	if err != nil {
		return err
	}
	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		return err
	}
	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	mediaEngine.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	registry.Add(responder)
	registry.Add(generator)

	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(mediaEngine); err != nil {
		return err
	}
	if err := webrtc.ConfigureStatsInterceptor(registry); err != nil {
		return err
	}
	return webrtc.ConfigureTWCCSender(mediaEngine, registry)
}

const (
	liveAsmWindow      = 2048
	liveAsmWindowMask  = liveAsmWindow - 1
	liveIDRBypassGrace = 400 * time.Millisecond
)

type asmSlot struct {
	present   bool
	pkt       *rtp.Packet
	arrivedAt time.Time
}

// liveH264Assembler reorders H.264 RTP packets without permitting stale/duplicate
// sequence numbers to rewind the frontier or trapping complete IDRs behind unrecovered gaps.
type liveH264Assembler struct {
	reorderWindow time.Duration
	depacketizer  liveH264Depacketizer
	diag          *rtpDiagTracker

	slots         [liveAsmWindow]asmSlot
	started       bool
	frontierSet   bool
	frontierSeq   uint16
	headSeq       uint16
	maxSeq        uint16
	maxTS         uint32
	lastPushAt    time.Time
	pendingDrops  uint32
	lastPopMarker bool
	lastPopTS     uint32
	hasLastPop    bool
	holeSeq       uint16
	holeFirstSeen time.Time
	holeTracking  bool
}

func newLiveH264Assembler(diag *rtpDiagTracker, window time.Duration) *liveH264Assembler {
	if window <= 0 {
		window = liveRTPReorderWindow
	}
	return &liveH264Assembler{
		reorderWindow: window,
		diag:          diag,
	}
}

func (a *liveH264Assembler) pendingDropsUint16() uint16 {
	if a.pendingDrops > 0xffff {
		return 0xffff
	}
	return uint16(a.pendingDrops)
}

func seqAfter(a, b uint16) bool {
	d := a - b
	return d != 0 && d < 0x8000
}

func seqBeforeOrEqual(a, b uint16) bool {
	return a == b || seqAfter(b, a)
}

func seqBefore(a, b uint16) bool {
	return seqAfter(b, a)
}

func startsMidFragment(pkt *rtp.Packet) bool {
	if pkt == nil || len(pkt.Payload) == 0 {
		return false
	}
	naluType := pkt.Payload[0] & 0x1f
	if (naluType == 28 || naluType == 29) && len(pkt.Payload) >= 2 {
		return pkt.Payload[1]&0x80 == 0
	}
	return false
}

func hasCompleteKeyframeHeaders(annexB []byte) bool {
	var hasSPS, hasPPS, hasIDR bool
	for _, nalu := range annexBNALUs(annexB) {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 5:
			hasIDR = true
		case 7:
			hasSPS = true
		case 8:
			hasPPS = true
		}
	}
	return hasIDR && hasSPS && hasPPS
}

func (a *liveH264Assembler) clonePacket(pkt *rtp.Packet) *rtp.Packet {
	cp := *pkt
	cp.Payload = append([]byte(nil), pkt.Payload...)
	return &cp
}

func (a *liveH264Assembler) countLiveInRange(fromSeq, toSeq uint16) int {
	if seqAfter(fromSeq, toSeq) {
		return 0
	}
	n := 0
	span := int(toSeq - fromSeq)
	for i := 0; i <= span && i < liveAsmWindow; i++ {
		s := fromSeq + uint16(i)
		sl := &a.slots[s&liveAsmWindowMask]
		if sl.present && sl.pkt.SequenceNumber == s {
			n++
		}
	}
	return n
}

func (a *liveH264Assembler) advanceFrontierTo(nextSeq uint16, countAsDropped bool) {
	if !a.started {
		return
	}
	for seqBefore(a.headSeq, nextSeq) {
		sl := &a.slots[a.headSeq&liveAsmWindowMask]
		if sl.present && sl.pkt.SequenceNumber == a.headSeq {
			if countAsDropped {
				a.pendingDrops++
				if a.diag != nil {
					a.diag.onBuilderRelease(sl.pkt)
				}
			}
			*sl = asmSlot{}
		} else if countAsDropped {
			// Also note a skipped sequence slot so h264LiveRecovery demands IDR across holes.
			a.pendingDrops++
		}
		a.headSeq++
	}
	a.frontierSet = true
	a.frontierSeq = nextSeq - 1
	a.holeTracking = false
}

// Push inserts pkt if it belongs in the active window. Duplicates and stale packets behind
// the emitted/abandoned frontier are rejected before modifying buffer state.
func (a *liveH264Assembler) Push(pkt *rtp.Packet, now time.Time) bool {
	if pkt == nil || len(pkt.Payload) == 0 {
		return false
	}
	seq := pkt.SequenceNumber
	if !a.started {
		a.started = true
		a.headSeq = seq
		a.maxSeq = seq
		a.maxTS = pkt.Timestamp
		a.lastPushAt = now
		a.slots[seq&liveAsmWindowMask] = asmSlot{
			present:   true,
			pkt:       a.clonePacket(pkt),
			arrivedAt: now,
		}
		return true
	}

	// Never rewind past an emitted or permanently abandoned frontier.
	if a.frontierSet && seqBeforeOrEqual(seq, a.frontierSeq) {
		if a.diag != nil {
			a.diag.onBuilderRelease(pkt)
		}
		return false
	}

	// Startup pre-frontier guard: ignore grossly old packets outside reorder span.
	if !a.frontierSet && seqBefore(seq, a.headSeq) {
		if a.maxSeq-seq >= liveAsmWindow {
			if a.diag != nil {
				a.diag.onBuilderRelease(pkt)
			}
			return false
		}
		a.headSeq = seq
	}

	// Reject exact duplicate within active window before modifying slots or counters.
	sl := &a.slots[seq&liveAsmWindowMask]
	if sl.present && sl.pkt.SequenceNumber == seq {
		return false
	}

	// Ignore massive forward wrap jumps (> 32767) that look like stale seq behind head.
	if seqBefore(seq, a.headSeq) {
		if a.diag != nil {
			a.diag.onBuilderRelease(pkt)
		}
		return false
	}

	// If window would overflow forward past liveAsmWindow slots from headSeq, trim oldest cleanly.
	if seq-a.headSeq >= liveAsmWindow {
		targetHead := seq - liveAsmWindow + 1
		a.advanceFrontierTo(targetHead, true)
	}

	a.slots[seq&liveAsmWindowMask] = asmSlot{
		present:   true,
		pkt:       a.clonePacket(pkt),
		arrivedAt: now,
	}
	a.lastPushAt = now
	if seqAfter(seq, a.maxSeq) {
		a.maxSeq = seq
	}
	if int32(pkt.Timestamp-a.maxTS) > 0 {
		a.maxTS = pkt.Timestamp
	}
	return true
}

// buildContiguousAU extracts and depacketizes a complete contiguous AU starting at startSeq
// if all fragments through Marker=true are present with matching RTP timestamp.
func (a *liveH264Assembler) buildContiguousAU(startSeq uint16) (endSeq uint16, sample *media.Sample, completeIDR bool, ok bool) {
	first := &a.slots[startSeq&liveAsmWindowMask]
	if !first.present || first.pkt.SequenceNumber != startSeq {
		return 0, nil, false, false
	}
	if startsMidFragment(first.pkt) {
		return 0, nil, false, false
	}
	auTS := first.pkt.Timestamp
	span := int(a.maxSeq - startSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}

	a.depacketizer = liveH264Depacketizer{}
	var headers []*rtp.Header
	var buf []byte
	var foundMarker bool
	var curEnd uint16

	for i := 0; i <= span; i++ {
		s := startSeq + uint16(i)
		sl := &a.slots[s&liveAsmWindowMask]
		if !sl.present || sl.pkt.SequenceNumber != s {
			return 0, nil, false, false
		}
		if sl.pkt.Timestamp != auTS {
			return 0, nil, false, false
		}
		hdr := sl.pkt.Header
		headers = append(headers, &hdr)
		part, err := a.depacketizer.Unmarshal(sl.pkt.Payload)
		if err != nil {
			return 0, nil, false, false
		}
		if len(part) > 0 {
			buf = append(buf, part...)
		}
		curEnd = s
		if sl.pkt.Marker {
			foundMarker = true
			break
		}
	}
	if !foundMarker || len(buf) == 0 {
		return 0, nil, false, false
	}
	completeKeyframe := hasCompleteKeyframeHeaders(buf)
	return curEnd, &media.Sample{
		Data:               buf,
		Timestamp:          first.arrivedAt,
		PacketTimestamp:    auTS,
		Duration:           time.Second / 15,
		RTPHeaders:         headers,
		PrevDroppedPackets: a.pendingDropsUint16(),
	}, completeKeyframe, true
}

func (a *liveH264Assembler) findCompleteIDR(fromSeq uint16) (idrStart, idrEnd uint16, idrSample *media.Sample, found bool) {
	span := int(a.maxSeq - fromSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}
	for i := 0; i <= span; i++ {
		s := fromSeq + uint16(i)
		sl := &a.slots[s&liveAsmWindowMask]
		if !sl.present || sl.pkt.SequenceNumber != s {
			continue
		}
		// Only inspect potential AU boundaries (first slot at head or timestamp transition or marker+1).
		if i > 0 {
			prevSeq := s - 1
			prev := &a.slots[prevSeq&liveAsmWindowMask]
			if prev.present && prev.pkt.SequenceNumber == prevSeq && prev.pkt.Timestamp == sl.pkt.Timestamp && !prev.pkt.Marker {
				continue
			}
		}
		endSeq, sample, completeKeyframe, ok := a.buildContiguousAU(s)
		if !ok || !completeKeyframe {
			continue
		}
		return s, endSeq, sample, true
	}
	return 0, 0, nil, false
}

func (a *liveH264Assembler) firstPresentFromHead() (seq uint16, sl asmSlot, ok bool) {
	if !a.started || seqAfter(a.headSeq, a.maxSeq) {
		return 0, asmSlot{}, false
	}
	span := int(a.maxSeq - a.headSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}
	for i := 0; i <= span; i++ {
		s := a.headSeq + uint16(i)
		cur := a.slots[s&liveAsmWindowMask]
		if cur.present && cur.pkt.SequenceNumber == s {
			return s, cur, true
		}
	}
	return 0, asmSlot{}, false
}

func (a *liveH264Assembler) firstHoleBeforeMax() (seq uint16, ok bool) {
	if !a.started || seqAfter(a.headSeq, a.maxSeq) {
		return 0, false
	}
	span := int(a.maxSeq - a.headSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}
	// Scan [headSeq .. maxSeq) for an actual missing sequence slot strictly behind maxSeq.
	for i := 0; i < span; i++ {
		s := a.headSeq + uint16(i)
		cur := a.slots[s&liveAsmWindowMask]
		if !cur.present || cur.pkt.SequenceNumber != s {
			return s, true
		}
	}
	return 0, false
}

func (a *liveH264Assembler) oldestGapAge(now time.Time) time.Duration {
	if !a.holeTracking || a.holeFirstSeen.IsZero() {
		return 0
	}
	return now.Sub(a.holeFirstSeen)
}

func (a *liveH264Assembler) headTimestampDelay() time.Duration {
	_, first, ok := a.firstPresentFromHead()
	if !ok {
		return 0
	}
	dt := int32(a.maxTS - first.pkt.Timestamp)
	if dt <= 0 {
		return 0
	}
	return time.Duration(dt) * time.Second / 90000
}

func (a *liveH264Assembler) Pop(now time.Time, needIDR bool) *media.Sample {
	for a.started && !seqAfter(a.headSeq, a.maxSeq) {
		headSlot := &a.slots[a.headSeq&liveAsmWindowMask]
		if headSlot.present && headSlot.pkt.SequenceNumber == a.headSeq {
			endSeq, sample, _, ok := a.buildContiguousAU(a.headSeq)
			if ok {
				for s := a.headSeq; seqBeforeOrEqual(s, endSeq); s++ {
					a.slots[s&liveAsmWindowMask] = asmSlot{}
				}
				a.headSeq = endSeq + 1
				a.frontierSet = true
				a.frontierSeq = endSeq
				a.pendingDrops = 0
				a.hasLastPop = true
				a.lastPopMarker = true
				a.lastPopTS = sample.PacketTimestamp
				a.holeTracking = false
				return sample
			}
			// Contiguous head AU is either incomplete (waiting for packet) or truncated mid-fragment.
			if startsMidFragment(headSlot.pkt) {
				// Cannot ever start a valid NALU cleanly at headSeq; drop this broken head AU up to next timestamp/marker.
				brokenEnd := a.findBrokenAUEnd(a.headSeq)
				a.advanceFrontierTo(brokenEnd+1, true)
				continue
			}
		}

		// Distinguish an actual missing sequence hole behind maxSeq from a contiguous in-flight AU.
		firstSeq, firstSlot, ok := a.firstPresentFromHead()
		if !ok {
			a.headSeq = a.maxSeq + 1
			a.holeTracking = false
			return nil
		}
		holeSeq, hasHole := a.firstHoleBeforeMax()
		if !hasHole {
			if firstSeq == a.headSeq && firstSlot.pkt.Timestamp == a.maxTS {
				// Contiguous AU still arriving without sequence gaps; do not age a nonexistent hole.
				a.holeTracking = false
				return nil
			}
			// Adjacent frame moved to a newer RTP timestamp without Marker=1 on head AU; drop the truncated head AU.
			brokenEnd := a.findBrokenAUEnd(a.headSeq)
			a.advanceFrontierTo(brokenEnd+1, true)
			continue
		}
		if !a.holeTracking || a.holeSeq != holeSeq {
			a.holeTracking = true
			a.holeSeq = holeSeq
			a.holeFirstSeen = now
		}

		// Fast path: if a complete decodable IDR already sits behind this hole, skip the dead pre-IDR
		// packets immediately when recovering (needIDR) or after a short reorder grace window.
		if idrStart, idrEnd, idrSample, found := a.findCompleteIDR(a.headSeq); found {
			gapAge := now.Sub(a.holeFirstSeen)
			if needIDR || gapAge >= liveIDRBypassGrace || a.headTimestampDelay() >= liveIDRBypassGrace {
				a.advanceFrontierTo(idrStart, true)
				idrSample.PrevDroppedPackets = a.pendingDropsUint16()
				for s := idrStart; seqBeforeOrEqual(s, idrEnd); s++ {
					a.slots[s&liveAsmWindowMask] = asmSlot{}
				}
				a.headSeq = idrEnd + 1
				a.frontierSet = true
				a.frontierSeq = idrEnd
				a.pendingDrops = 0
				a.hasLastPop = true
				a.lastPopMarker = true
				a.lastPopTS = idrSample.PacketTimestamp
				a.holeTracking = false
				return idrSample
			}
		}

		// Wait until both the head AU and the active hole age past reorderWindow (or RTP timestamp span does),
		// so a tail loss inside a slow >1s IDR still gets its full NACK/reorder recovery window.
		wallAged := now.Sub(firstSlot.arrivedAt) >= a.reorderWindow &&
			now.Sub(a.holeFirstSeen) >= a.reorderWindow &&
			(a.maxTS != firstSlot.pkt.Timestamp || now.Sub(a.lastPushAt) >= a.reorderWindow)
		rtpAged := int32(a.maxTS-firstSlot.pkt.Timestamp) >= int32(a.reorderWindow.Seconds()*90000)
		if !wallAged && !rtpAged {
			return nil
		}

		// Window expired: advance strictly past the oldest incomplete gap/AU, preserving newer AUs/IDRs.
		if firstSeq != a.headSeq {
			a.advanceFrontierTo(firstSeq, true)
			continue
		}
		brokenEnd := a.findBrokenAUEnd(a.headSeq)
		a.advanceFrontierTo(brokenEnd+1, true)
	}
	return nil
}

func (a *liveH264Assembler) findBrokenAUEnd(startSeq uint16) uint16 {
	sl := &a.slots[startSeq&liveAsmWindowMask]
	if !sl.present || sl.pkt.SequenceNumber != startSeq {
		return startSeq
	}
	ts := sl.pkt.Timestamp
	endSeq := startSeq
	span := int(a.maxSeq - startSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}
	for i := 1; i <= span; i++ {
		s := startSeq + uint16(i)
		cur := &a.slots[s&liveAsmWindowMask]
		if !cur.present || cur.pkt.SequenceNumber != s {
			// Stop before the next present slot only if timestamp changes later; consume missing hole tail of same AU.
			continue
		}
		if cur.pkt.Timestamp != ts {
			break
		}
		endSeq = s
		if cur.pkt.Marker {
			break
		}
	}
	return endSeq
}

// PruneOnStall discards obsolete incomplete frames older than the newest active RTP timestamp
// when frame_stall fires, preserving any in-progress IDR packets currently arriving.
func (a *liveH264Assembler) PruneOnStall(now time.Time) {
	if !a.started || seqAfter(a.headSeq, a.maxSeq) {
		return
	}
	span := int(a.maxSeq - a.headSeq)
	if span >= liveAsmWindow {
		span = liveAsmWindow - 1
	}
	for i := 0; i <= span; i++ {
		s := a.headSeq + uint16(i)
		cur := &a.slots[s&liveAsmWindowMask]
		if cur.present && cur.pkt.SequenceNumber == s && cur.pkt.Timestamp == a.maxTS {
			if seqAfter(s, a.headSeq) {
				a.advanceFrontierTo(s, true)
			}
			return
		}
	}
	_, firstSlot, ok := a.firstPresentFromHead()
	if ok && now.Sub(firstSlot.arrivedAt) >= a.reorderWindow && now.Sub(a.lastPushAt) >= a.reorderWindow {
		a.advanceFrontierTo(a.maxSeq+1, true)
	}
}
