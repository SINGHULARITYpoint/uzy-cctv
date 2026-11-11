package devices

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Offline model of the live receive path. A synthetic 15 fps stream with a
// fragmented ~250-packet IDR every 2 s is delayed using the pattern measured
// in two-camera-20261107-181934 (bursts of consecutive packets arriving 0.2-3.0 s
// late while later packets arrive on time), then replayed through the real
// SampleBuilder + h264LiveRecovery, including the live frame-stall reset.

const (
	simFrameStep   = 6000 // 90 kHz ticks at 15 fps
	simFrameTime   = time.Second / 15
	simIDREvery    = 30
	simBaseLatency = 20 * time.Millisecond
)

type simPkt struct {
	send, arrive time.Duration
	pkt          *rtp.Packet
	frame, index int
	ep           int
}

type simEpisode struct{ start, width, delay time.Duration }

type simConfig struct {
	name          string
	window, stall time.Duration
}

type simOut struct {
	frame   int
	at      time.Duration
	idr     bool
	latency time.Duration
}

type simEpisodeStat struct {
	label         string
	frames, lost  int
	spanMs        float64
	spanPkts      int
	drops, resets uint64
	maxLatencyMs  float64
}

type simResult struct {
	out         []simOut
	totalFrames int
	stallResets int
	asmDrops    uint64
	asmLate     uint64
	maxGap      time.Duration
	maxLatency  time.Duration
	p95Latency  time.Duration
	episodes    []simEpisodeStat
}

func synthStream(frames int) (pkts []*simPkt, frameDone []time.Duration) {
	payloader := &codecs.H264Payloader{}
	var seq uint16 = 60000 // exercises sequence wrap
	frameDone = make([]time.Duration, frames)
	fill := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = 0x11
		}
		return b
	}
	for k := 0; k < frames; k++ {
		var unit []byte
		span := 8 * time.Millisecond
		if k%simIDREvery == 0 {
			unit = append(unit, 0, 0, 0, 1, 0x67, 0x64, 0x00, 0x32, 0xac)
			unit = append(unit, 0, 0, 0, 1, 0x68, 0xee, 0x3c, 0x80)
			unit = append(unit, 0, 0, 0, 1, 0x65)
			unit = append(unit, fill(250*1150)...)
			span = 50 * time.Millisecond
		} else {
			unit = append(unit, 0, 0, 0, 1, 0x41)
			unit = append(unit, fill(3*1150)...)
		}
		payloads := payloader.Payload(1200, unit)
		for j, payload := range payloads {
			send := time.Duration(k)*simFrameTime + span*time.Duration(j)/time.Duration(len(payloads))
			p := &simPkt{send: send, arrive: send + simBaseLatency, frame: k, index: j, ep: -1,
				pkt: &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(k) * simFrameStep,
					Marker: j == len(payloads)-1}, Payload: payload}}
			seq++
			pkts = append(pkts, p)
			frameDone[k] = send
		}
	}
	return pkts, frameDone
}

func applyEpisodes(pkts []*simPkt, eps []simEpisode) {
	for _, p := range pkts {
		for i, e := range eps {
			if p.send >= e.start && p.send < e.start+e.width {
				p.arrive += e.delay
				p.ep = i
			}
		}
	}
}

func dropFragment(pkts []*simPkt, frame, index int) []*simPkt {
	out := pkts[:0:0]
	for _, p := range pkts {
		if p.frame == frame && p.index == index {
			continue
		}
		out = append(out, p)
	}
	return out
}

func runSim(pkts []*simPkt, frameDone []time.Duration, eps []simEpisode, cfg simConfig) simResult {
	events := append([]*simPkt(nil), pkts...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].arrive < events[j].arrive })
	base := time.Unix(1000, 0)
	var diag rtpDiagTracker
	builder := newLiveH264BuilderWithWindow(&diag, cfg.window)
	rec := h264LiveRecovery{stall: cfg.stall}
	res := simResult{totalFrames: len(frameDone), episodes: make([]simEpisodeStat, len(eps))}
	type snap struct {
		at           time.Duration
		drops, reset uint64
	}
	var snaps []snap
	var maxSeq uint16
	var maxTS uint32
	started := false
	got := map[int]bool{}
	for _, e := range events {
		now := base.Add(e.arrive)
		if rec.stalled(now) {
			diag.onBuilderReset()
			builder = newLiveH264BuilderWithWindow(&diag, cfg.window)
			res.stallResets++
		}
		if started && int16(e.pkt.SequenceNumber-maxSeq) < 0 && e.ep >= 0 {
			st := &res.episodes[e.ep]
			if ms := float64(maxTS-e.pkt.Timestamp) / 90; ms > st.spanMs {
				st.spanMs = ms
			}
			if n := int(maxSeq - e.pkt.SequenceNumber); n > st.spanPkts {
				st.spanPkts = n
			}
		}
		if !started || int16(e.pkt.SequenceNumber-maxSeq) > 0 {
			maxSeq, maxTS, started = e.pkt.SequenceNumber, e.pkt.Timestamp, true
		}
		diag.onInputPacket(e.pkt)
		builder.Push(e.pkt)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			late := rec.hasTimestamp && int32(sample.PacketTimestamp-rec.lastRTP) < 0
			complete := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
			diag.onSamplePopped(sample, late || !complete, late)
			data, frame, _, _ := rec.push(sample)
			if frame && len(data) > 0 {
				rec.lastFrame = now
				f := int(sample.PacketTimestamp / simFrameStep)
				if !got[f] {
					got[f] = true
					res.out = append(res.out, simOut{frame: f, at: e.arrive, idr: f%simIDREvery == 0,
						latency: e.arrive - frameDone[f] - simBaseLatency})
				}
			}
		}
		diag.afterPopDrain()
		snaps = append(snaps, snap{e.arrive, diag.asmDrops.Load(), uint64(res.stallResets)})
	}
	res.asmDrops, res.asmLate = diag.asmDrops.Load(), diag.asmLate.Load()
	var lats []time.Duration
	for i, o := range res.out {
		lats = append(lats, o.latency)
		if o.latency > res.maxLatency {
			res.maxLatency = o.latency
		}
		if i > 0 && o.at-res.out[i-1].at > res.maxGap {
			res.maxGap = o.at - res.out[i-1].at
		}
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	if len(lats) > 0 {
		res.p95Latency = lats[len(lats)*95/100]
	}
	at := func(t time.Duration) snap {
		last := snap{}
		for _, s := range snaps {
			if s.at > t {
				break
			}
			last = s
		}
		return last
	}
	for i, ep := range eps {
		st := &res.episodes[i]
		frames := map[int]bool{}
		for _, p := range pkts {
			if p.ep == i {
				frames[p.frame] = true
			}
		}
		st.frames = len(frames)
		for f := range frames {
			if !got[f] {
				st.lost++
			}
		}
		from, to := ep.start, ep.start+ep.width+ep.delay+cfg.window+500*time.Millisecond
		a, b := at(from), at(to)
		st.drops, st.resets = b.drops-a.drops, b.reset-a.reset
		for _, o := range res.out {
			if o.at >= from+simBaseLatency && o.at <= to && float64(o.latency.Milliseconds()) > st.maxLatencyMs {
				st.maxLatencyMs = float64(o.latency.Milliseconds())
			}
		}
		var limits []string
		if st.resets > 0 {
			limits = append(limits, fmt.Sprintf("frame-stall reset (%v)", cfg.stall))
		}
		if st.drops > 0 {
			if st.spanPkts >= liveRTPMaxLate {
				limits = append(limits, fmt.Sprintf("packet limit (%d)", liveRTPMaxLate))
			} else {
				limits = append(limits, fmt.Sprintf("time window (%v)", cfg.window))
			}
		}
		st.label = strings.Join(limits, " + ")
		if st.label == "" {
			st.label = "none"
		}
	}
	return res
}

var simObservedEpisodes = func() []simEpisode {
	// Delay values follow the measured late-arrival quantiles (median 0.45 s,
	// p90 1.54 s, p99 3.0 s). Each burst holds 150 ms of consecutive packets.
	delays := []float64{0.30, 0.45, 0.50, 0.80, 1.20, 1.60, 2.20, 3.00}
	var eps []simEpisode
	for i, d := range delays {
		eps = append(eps, simEpisode{
			start: 2*time.Second + time.Duration(i)*4*time.Second + 20*time.Millisecond*time.Duration(i*3),
			width: 150 * time.Millisecond, delay: time.Duration(d * float64(time.Second))})
	}
	return eps
}()

var simConfigs = []simConfig{
	{"A current: 500 ms window, 1.5 s stall", 500 * time.Millisecond, liveFrameStall},
	{"B proposed: 3.1 s window, 1.5 s stall", 3100 * time.Millisecond, liveFrameStall},
	{"C 3.1 s window, 4 s stall", 3100 * time.Millisecond, 4 * time.Second},
	{"D 1.8 s window, 2.5 s stall", 1800 * time.Millisecond, 2500 * time.Millisecond},
}

func logSim(t *testing.T, title string, results []simResult, eps []simEpisode) {
	t.Logf("== %s ==", title)
	for i, cfg := range simConfigs {
		r := results[i]
		t.Logf("%-40s frames out %d/%d  asm_drops=%d asm_late=%d  stall_resets=%d  max output gap=%v  latency max=%v p95=%v",
			cfg.name, len(r.out), r.totalFrames, r.asmDrops, r.asmLate, r.stallResets,
			r.maxGap.Round(time.Millisecond), r.maxLatency.Round(time.Millisecond), r.p95Latency.Round(time.Millisecond))
	}
	for e, ep := range eps {
		for i, cfg := range simConfigs {
			st := results[i].episodes[e]
			t.Logf("  episode %d delay=%v frames=%d | %s: lost=%d drops=%d limit=%s span=%.0fms/%dpkts latency<=%.0fms",
				e, ep.delay, st.frames, strings.SplitN(cfg.name, ":", 2)[0], st.lost, st.drops, st.label, st.spanMs, st.spanPkts, st.maxLatencyMs)
		}
	}
}

func TestLiveReassemblyWindowObservedDelays(t *testing.T) {
	const frames = 15 * 40
	pkts, done := synthStream(frames)
	eps := simObservedEpisodes
	applyEpisodes(pkts, eps)
	var results []simResult
	for _, cfg := range simConfigs {
		results = append(results, runSim(pkts, done, eps, cfg))
	}
	logSim(t, "observed delay pattern, no permanent loss", results, eps)
	a, b, c := results[0], results[1], results[2]
	if len(c.out) < frames-1 {
		got := map[int]bool{}
		for _, o := range c.out {
			got[o.frame] = true
		}
		var missing []int
		for f := 0; f < frames; f++ {
			if !got[f] {
				missing = append(missing, f)
			}
		}
		t.Fatalf("3.1 s window with a 4 s stall should recover every frame, missing %v", missing)
	}
	if len(c.out) <= len(a.out) || c.asmDrops >= a.asmDrops {
		t.Fatalf("larger window should discard less than 500 ms: %d vs %d frames, %d vs %d drops", len(c.out), len(a.out), c.asmDrops, a.asmDrops)
	}
	if b.stallResets == 0 {
		t.Fatal("expected the 1.5 s frame-stall reset to fire with a 3.1 s window")
	}
	if c.maxLatency > 3100*time.Millisecond+500*time.Millisecond {
		t.Fatalf("added delay exceeds the window: %v", c.maxLatency)
	}
}

func TestLiveReassemblyWindowMissingFragmentStaysBounded(t *testing.T) {
	const frames = 15 * 30
	pkts, done := synthStream(frames)
	// Frame 150 is an IDR (t = 10 s); one middle fragment never arrives.
	pkts = dropFragment(pkts, 150, 100)
	var results []simResult
	for _, cfg := range simConfigs {
		results = append(results, runSim(pkts, done, nil, cfg))
	}
	logSim(t, "one IDR fragment permanently missing", results, nil)
	for i, cfg := range simConfigs {
		r := results[i]
		var last, first = -1, -1
		for _, o := range r.out {
			if o.frame < 150 {
				last = o.frame
			} else if o.frame > 150 && first < 0 {
				first = o.frame
			}
		}
		if first < 0 || last < 0 {
			t.Fatalf("%s: never resumed (last before %d, first after %d)", cfg.name, last, first)
		}
		var lastAt, firstAt time.Duration
		for _, o := range r.out {
			if o.frame == last {
				lastAt = o.at
			}
			if o.frame == first {
				firstAt = o.at
			}
		}
		outage := firstAt - lastAt
		t.Logf("%-40s last good frame %d, resumed at IDR frame %d, output outage %v", cfg.name, last, first, outage.Round(time.Millisecond))
		if first%simIDREvery != 0 {
			t.Fatalf("%s: resumed on a non-IDR frame %d", cfg.name, first)
		}
		if limit := 2*time.Second + cfg.window + 500*time.Millisecond; outage > limit {
			t.Fatalf("%s: outage %v exceeds bound %v", cfg.name, outage, limit)
		}
	}
}
