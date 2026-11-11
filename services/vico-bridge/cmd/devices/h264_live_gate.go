package devices

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const liveRTPMaxLate = 2048
const liveRTPReorderWindow = 500 * time.Millisecond
const liveFrameStall = 1500 * time.Millisecond

type liveH264Depacketizer struct{ codecs.H264Packet }

func (p *liveH264Depacketizer) Unmarshal(payload []byte) ([]byte, error) {
	// An abandoned FU-A must never contribute bytes to the next FU-A.
	if len(payload) >= 2 && payload[0]&0x1f == 28 && payload[1]&0x80 != 0 {
		p.H264Packet = codecs.H264Packet{}
	}
	return p.H264Packet.Unmarshal(payload)
}

func newLiveH264Builder() *samplebuilder.SampleBuilder {
	return newLiveH264BuilderWithDiag(nil)
}

func newLiveH264BuilderWithDiag(diag *rtpDiagTracker) *samplebuilder.SampleBuilder {
	return newLiveH264BuilderWithWindow(diag, liveEnvMillis("VICO_REASSEMBLY_WINDOW_MS", liveRTPReorderWindow))
}

// liveEnvMillis reads a test-only millisecond override; unset keeps the live default.
func liveEnvMillis(name string, def time.Duration) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return def
}

// newLiveH264BuilderWithWindow lets tests compare reassembly windows; the live
// default stays liveRTPReorderWindow.
func newLiveH264BuilderWithWindow(diag *rtpDiagTracker, window time.Duration) *samplebuilder.SampleBuilder {
	opts := []samplebuilder.Option{
		samplebuilder.WithMaxTimeDelay(window),
		samplebuilder.WithRTPHeaders(true),
	}
	if diag != nil {
		opts = append(opts, samplebuilder.WithPacketReleaseHandler(diag.onBuilderRelease))
	}
	return samplebuilder.New(liveRTPMaxLate, &liveH264Depacketizer{}, 90000, opts...)
}

// rtpDiagTracker separates sequence gaps at the bridge's RTP input from
// arrived packets that are delayed or discarded during H.264 sample assembly.
type rtpDiagTracker struct {
	mu             sync.Mutex
	startedInput   bool
	maxInputSeq    uint16
	seenInput      [65536]bool
	missingSet     [65536]bool
	inBuilder      [65536]bool
	released       [65536]bool
	lateForAsm     [65536]bool
	releasedList   []uint16
	hasFrontierSeq bool
	frontierSeq    uint16
	hasFrontierTS  bool
	frontierTS     uint32
	hasSweptSeq    bool
	sweptSeq       uint16

	seqGaps    atomic.Uint64
	seqMissing atomic.Uint64
	seqLate    atomic.Uint64
	seqDup     atomic.Uint64
	seqUnknown atomic.Uint64
	asmDrops   atomic.Uint64
	asmLate    atomic.Uint64
}

func (d *rtpDiagTracker) onInputPacket(pkt *rtp.Packet) {
	if d == nil || pkt == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	seq := pkt.SequenceNumber
	ts := pkt.Timestamp
	if !d.startedInput {
		d.startedInput = true
		d.maxInputSeq = seq
		d.seenInput[seq] = true
		d.hasSweptSeq = true
		d.sweptSeq = seq
	} else {
		delta := int16(seq - d.maxInputSeq)
		if delta == 1 {
			d.seenInput[seq] = true
			d.maxInputSeq = seq
		} else if delta > 1 {
			for s := d.maxInputSeq + 1; s != seq; s++ {
				d.seenInput[s] = false
				d.missingSet[s] = true
			}
			d.seenInput[seq] = true
			d.maxInputSeq = seq
			d.seqGaps.Add(1)
			d.seqMissing.Add(uint64(uint16(delta - 1)))
		} else if d.missingSet[seq] {
			// Only the exact sequence number that was skipped clears its gap.
			d.missingSet[seq] = false
			d.seenInput[seq] = true
			d.seqLate.Add(1)
			if d.seqMissing.Load() > 0 {
				d.seqMissing.Add(^uint64(0))
			}
		} else if d.seenInput[seq] {
			d.seqDup.Add(1)
		} else {
			// Behind the session start or outside the tracked window; it never
			// owned a gap, so it must not shrink the missing count.
			d.seenInput[seq] = true
			d.seqUnknown.Add(1)
		}
	}
	d.lateForAsm[seq] = (d.hasFrontierSeq && int16(seq-d.frontierSeq) < 0) ||
		(d.hasFrontierTS && int32(ts-d.frontierTS) < 0)
	d.inBuilder[seq] = true
	d.released[seq] = false
}

func (d *rtpDiagTracker) onBuilderRelease(pkt *rtp.Packet) {
	if d == nil || pkt == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	seq := pkt.SequenceNumber
	d.released[seq] = true
	d.releasedList = append(d.releasedList, seq)
	if !d.hasFrontierSeq || int16(seq+1-d.frontierSeq) > 0 {
		d.hasFrontierSeq, d.frontierSeq = true, seq+1
	}
	if !d.hasFrontierTS || int32(pkt.Timestamp-d.frontierTS) > 0 {
		d.hasFrontierTS, d.frontierTS = true, pkt.Timestamp
	}
}

func (d *rtpDiagTracker) onSamplePopped(sample *media.Sample, rejected, lateSample bool) {
	if d == nil || sample == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n := uint64(len(sample.RTPHeaders))
	for _, h := range sample.RTPHeaders {
		d.inBuilder[h.SequenceNumber] = false
		d.released[h.SequenceNumber] = false
		d.lateForAsm[h.SequenceNumber] = false
	}
	if len(sample.RTPHeaders) > 0 {
		tailSeq := sample.RTPHeaders[len(sample.RTPHeaders)-1].SequenceNumber
		if !d.hasFrontierSeq || int16(tailSeq+1-d.frontierSeq) > 0 {
			d.hasFrontierSeq, d.frontierSeq = true, tailSeq+1
		}
		if !d.hasSweptSeq {
			d.hasSweptSeq, d.sweptSeq = true, sample.RTPHeaders[0].SequenceNumber
		}
		for s := d.sweptSeq; int16(tailSeq-s) >= 0; s++ {
			if d.inBuilder[s] {
				d.inBuilder[s] = false
				d.released[s] = false
				d.asmDrops.Add(1)
				if d.lateForAsm[s] {
					d.asmLate.Add(1)
					d.lateForAsm[s] = false
				}
			}
		}
		d.sweptSeq = tailSeq + 1
	}
	if !d.hasFrontierTS || int32(sample.PacketTimestamp-d.frontierTS) > 0 {
		d.hasFrontierTS, d.frontierTS = true, sample.PacketTimestamp
	}
	if rejected {
		d.asmDrops.Add(n)
		if lateSample {
			d.asmLate.Add(n)
		}
	}
}

func (d *rtpDiagTracker) afterPopDrain() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, seq := range d.releasedList {
		if d.inBuilder[seq] {
			d.inBuilder[seq] = false
			d.released[seq] = false
			d.asmDrops.Add(1)
			if d.lateForAsm[seq] {
				d.asmLate.Add(1)
				d.lateForAsm[seq] = false
			}
		}
	}
	d.releasedList = d.releasedList[:0]
}

func (d *rtpDiagTracker) onBuilderReset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startedInput && d.hasSweptSeq {
		for s := d.sweptSeq; int16(d.maxInputSeq-s) >= 0; s++ {
			if d.inBuilder[s] {
				d.inBuilder[s] = false
				d.released[s] = false
				d.asmDrops.Add(1)
				if d.lateForAsm[s] {
					d.asmLate.Add(1)
					d.lateForAsm[s] = false
				}
			}
		}
		d.sweptSeq = d.maxInputSeq + 1
		d.hasFrontierSeq, d.frontierSeq = true, d.maxInputSeq+1
	}
	d.releasedList = d.releasedList[:0]
}

type h264LiveRecovery struct {
	gate         h264LiveGate
	stall        time.Duration // zero means liveFrameStall
	recovering   bool
	lastFrame    time.Time
	lastRTP      uint32
	hasTimestamp bool
}

func (r *h264LiveRecovery) beginGap() bool {
	if !r.gate.started {
		return false
	}
	r.gate.started = false
	r.recovering = true
	return true
}

func (r *h264LiveRecovery) stalled(now time.Time) bool {
	limit := r.stall
	if limit == 0 {
		limit = liveFrameStall
	}
	return !r.lastFrame.IsZero() && now.Sub(r.lastFrame) >= limit && r.beginGap()
}

func (r *h264LiveRecovery) push(sample *media.Sample) (data []byte, frame, gapStarted, recovered bool) {
	if r.hasTimestamp && int32(sample.PacketTimestamp-r.lastRTP) < 0 {
		return nil, false, false, false
	}
	r.lastRTP, r.hasTimestamp = sample.PacketTimestamp, true
	complete := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker
	if !complete || len(sample.Data) == 0 {
		return nil, false, r.beginGap(), false
	}
	if sample.PrevDroppedPackets > 0 {
		gapStarted = r.beginGap()
	}
	data, frame = r.gate.pushFrame(sample.Data)
	if frame {
		recovered, r.recovering = r.recovering && !gapStarted, false
		gapStarted = false
	}
	return data, frame, gapStarted, recovered
}

type streamProfileSpec struct {
	Name       string
	Size       string
	Resolution string
	LevelIDC   uint8
	Width      int
	Height     int
}

func parseStreamProfile(name string) (streamProfileSpec, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "2k", "2304x1296":
		return streamProfileSpec{
			Name: "2k", Size: "1920x1080", Resolution: "2304x1296",
			LevelIDC: 50, Width: 2304, Height: 1296,
		}, nil
	case "360p", "low", "640x360":
		return streamProfileSpec{
			Name: "360p", Size: "640x360", Resolution: "640x360",
			LevelIDC: 22, Width: 640, Height: 360,
		}, nil
	default:
		return streamProfileSpec{}, fmt.Errorf("unsupported stream profile %q (expected 2k or 360p)", name)
	}
}

// h264LiveGate waits for the target stream's parameter sets and an IDR.
// Zero-value defaults preserve the 2K (2304x1296 High@L5.0) behavior.
type h264LiveGate struct {
	expectedLevel    uint8 // zero defaults to 50 (2K High@L5.0)
	expectedWidth    int
	expectedHeight   int
	requirePostStart bool
	postStartReady   *atomic.Bool
	postStartCleared bool
	mainSPS          []byte
	mainPPS          []byte
	lastSPS          h264SPSInfo
	started          bool
}

func (g *h264LiveGate) configureProfile(spec streamProfileSpec, postStartReady *atomic.Bool) {
	g.expectedLevel = spec.LevelIDC
	g.expectedWidth = spec.Width
	g.expectedHeight = spec.Height
	g.requirePostStart = spec.Name == "360p"
	g.postStartReady = postStartReady
}

func (g *h264LiveGate) matchesSPS(sps h264SPSInfo) bool {
	targetLevel := g.expectedLevel
	if targetLevel == 0 {
		targetLevel = 50
	}
	if sps.LevelIDC != targetLevel {
		return false
	}
	if g.expectedWidth > 0 && g.expectedHeight > 0 && sps.HasDimensions {
		if sps.Width != g.expectedWidth || sps.Height != g.expectedHeight {
			return false
		}
	}
	return true
}

func (g *h264LiveGate) push(data []byte) []byte {
	out, _ := g.pushFrame(data)
	return out
}

func (g *h264LiveGate) pushFrame(data []byte) ([]byte, bool) {
	if g.requirePostStart {
		if g.postStartReady == nil || !g.postStartReady.Load() {
			return nil, false
		}
		if !g.postStartCleared {
			g.postStartCleared = true
			g.mainSPS, g.mainPPS, g.started = nil, nil, false
		}
	}
	var frame, idr bool
	for _, nalu := range annexBNALUs(data) {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 7:
			g.mainSPS, g.mainPPS = nil, nil
			if sps, ok := parseH264SPS(nalu); ok && g.matchesSPS(sps) {
				g.mainSPS = append([]byte(nil), nalu...)
				g.lastSPS = sps
			} else if ok && g.started {
				// Resolution/level drifted away from the configured profile; re-gate cleanly.
				g.started = false
			}
		case 8:
			if g.mainSPS != nil {
				g.mainPPS = append([]byte(nil), nalu...)
			}
		case 5:
			frame, idr = true, true
		case 1, 2, 3, 4:
			frame = true
		}
	}
	if g.started {
		return data, frame
	}
	if idr && g.mainSPS != nil && g.mainPPS != nil {
		g.started = true
		out := append([]byte{0, 0, 0, 1}, g.mainSPS...)
		out = append(out, 0, 0, 0, 1)
		out = append(out, g.mainPPS...)
		return append(out, data...), true
	}
	return nil, false
}

func annexBNALUs(data []byte) [][]byte {
	var result [][]byte
	start, zeros := -1, 0
	for i, value := range data {
		if value == 0 {
			zeros++
			continue
		}
		if value == 1 && zeros >= 2 {
			if end := i - zeros; start >= 0 && end > start {
				result = append(result, data[start:end])
			}
			start = i + 1
		}
		zeros = 0
	}
	if start >= 0 && start < len(data)-zeros {
		result = append(result, data[start:len(data)-zeros])
	}
	return result
}
