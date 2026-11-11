package devices

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v3"
	"github.com/pion/transport/v3/stdnet"
)

type wireKind string

const (
	wireUnknown wireKind = "unknown"
	wireSTUN    wireKind = "stun"
	wireDTLS    wireKind = "dtls"
	wireRTCP    wireKind = "rtcp"
	wireRTP     wireKind = "rtp"
)

type wirePacket struct {
	Kind      wireKind
	Encap     string // "raw", "turn_ch", "turn_ind"
	Payload   []byte // slice starting at unwrapped header
	SSRC      uint32
	Seq       uint16
	Marker    bool
	PayloadPT uint8
	RTCPFmt   uint8
}

// unwrapTURNAndClassify unwraps TURN ChannelData (0x4000-0x7FFF) or TURN STUN
// Data/Send Indications (0x0017/0x0016, attribute 0x0013 DATA) and classifies the
// inner wire header (STUN, DTLS, RTCP, or RTP).
func unwrapTURNAndClassify(raw []byte) wirePacket {
	b := raw
	encap := "raw"
	if len(b) > 4 && b[0] >= 64 && b[0] <= 127 {
		chLen := int(b[2])<<8 | int(b[3])
		b = b[4:]
		if chLen > 0 && chLen < len(b) {
			b = b[:chLen]
		}
		encap = "turn_ch"
	} else if len(b) >= 20 && (b[0]&0xc0) == 0 &&
		b[4] == 0x21 && b[5] == 0x12 && b[6] == 0xa4 && b[7] == 0x42 {
		msgType := binary.BigEndian.Uint16(b[0:2])
		if msgType == 0x0017 || msgType == 0x0016 {
			msgLen := int(binary.BigEndian.Uint16(b[2:4]))
			limit := len(b)
			if 20+msgLen < limit {
				limit = 20 + msgLen
			}
			for off := 20; off+4 <= limit; {
				attrType := binary.BigEndian.Uint16(b[off : off+2])
				attrLen := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
				valStart := off + 4
				if attrType == 0x0013 && valStart < len(b) {
					valEnd := valStart + attrLen
					if valEnd > len(b) {
						valEnd = len(b)
					}
					b = b[valStart:valEnd]
					encap = "turn_ind"
					break
				}
				padded := valStart + ((attrLen + 3) &^ 3)
				if padded <= off {
					break
				}
				off = padded
			}
		}
	}
	if len(b) == 0 {
		return wirePacket{Kind: wireUnknown, Encap: encap}
	}
	first := b[0]
	switch {
	case first < 4:
		return wirePacket{Kind: wireSTUN, Encap: encap, Payload: b}
	case first >= 20 && first <= 63:
		return wirePacket{Kind: wireDTLS, Encap: encap, Payload: b}
	case first < 128 || first > 191 || len(b) < 8:
		return wirePacket{Kind: wireUnknown, Encap: encap, Payload: b}
	}
	if b[1] >= 192 && b[1] <= 223 {
		var ssrc uint32
		if len(b) >= 8 {
			ssrc = binary.BigEndian.Uint32(b[4:8])
		}
		return wirePacket{
			Kind:      wireRTCP,
			Encap:     encap,
			Payload:   b,
			SSRC:      ssrc,
			PayloadPT: b[1],
			RTCPFmt:   b[0] & 0x1f,
		}
	}
	if len(b) < 12 {
		return wirePacket{Kind: wireUnknown, Encap: encap, Payload: b}
	}
	return wirePacket{
		Kind:      wireRTP,
		Encap:     encap,
		Payload:   b,
		SSRC:      binary.BigEndian.Uint32(b[8:12]),
		Seq:       binary.BigEndian.Uint16(b[2:4]),
		Marker:    b[1]&0x80 != 0,
		PayloadPT: b[1] & 0x7f,
	}
}

// loopTimings records, per progress interval, how long the video receive loop
// spent waiting for packets versus processing or writing them. Each field is a
// running maximum in microseconds that snapshot() reads and resets.
type loopTimings struct {
	readWait  atomic.Int64 // blocked inside track.ReadRTP
	readLag   atomic.Int64 // socket arrival to ReadRTP return (queueing before the loop)
	push      atomic.Int64 // diag + SampleBuilder.Push
	pop       atomic.Int64 // Pop, recovery gate, output write, bookkeeping
	write     atomic.Int64 // videoOutput.Write only
	busy      atomic.Int64 // ReadRTP return to next ReadRTP call
	slowWrite atomic.Int64 // writes slower than 50 ms since the last snapshot
	frameAge  atomic.Int64 // first packet of an emitted frame arriving at the socket to frame output
}

func (l *loopTimings) observe(field *atomic.Int64, d time.Duration) {
	us := d.Microseconds()
	for {
		cur := field.Load()
		if us <= cur || field.CompareAndSwap(cur, us) {
			return
		}
	}
}

func (l *loopTimings) snapshot() string {
	ms := func(f *atomic.Int64) float64 { return float64(f.Swap(0)) / 1000 }
	return fmt.Sprintf("rd_wait_ms=%.1f rd_lag_ms=%.1f push_ms=%.1f pop_ms=%.1f wr_ms=%.1f busy_ms=%.1f wr_slow=%d age_ms=%.1f",
		ms(&l.readWait), ms(&l.readLag), ms(&l.push), ms(&l.pop), ms(&l.write), ms(&l.busy), l.slowWrite.Swap(0), ms(&l.frameAge))
}

// rxStream tracks RTP sequence continuity for one SSRC at the UDP socket,
// independent of the bridge's receive loop. Only RTP headers are inspected.
type rxStream struct {
	packets    uint64
	started    bool
	maxSeq     uint16
	missingSet [65536]bool
	gaps       uint64
	outstand   uint64
	skipped    uint64
	late       uint64
	dup        uint64
	seen       [65536]bool
	last       time.Time
	maxGap     time.Duration
	maxGapSeq  uint16
}

type rxArrival struct {
	ssrc uint32
	at   int64 // microseconds since rx.start; 0 means unset
}

type flowKey struct {
	dir    string
	local  string
	remote string
}

type traceFlow struct {
	ID         int    `json:"id"`
	Dir        string `json:"dir"`
	LocalIP    string `json:"local_ip,omitempty"`
	LocalPort  int    `json:"local_port,omitempty"`
	RemoteIP   string `json:"remote_ip,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	FirstUs    int64  `json:"first_us"`
	LastUs     int64  `json:"last_us"`
	Packets    uint64 `json:"packets"`
	Bytes      uint64 `json:"bytes"`
}

type rxEndpointsFile struct {
	StartUTC   string      `json:"start_utc"`
	ICEServers []string    `json:"ice_servers,omitempty"`
	Flows      []traceFlow `json:"flows"`
}

// rxTrace is a header-only receive-side trace. It sits under the ICE agent via
// a wrapped transport.Net, so it sees datagrams as the OS delivers them to the
// ICE read loop, before DTLS/SRTP and before the bridge loop.
type rxTrace struct {
	start   time.Time
	mu      sync.Mutex
	ssrcIdx map[uint32]int
	streams [4]*rxStream
	ring    [65536]rxArrival

	udpPackets   atomic.Uint64
	udpBytes     atomic.Uint64
	rtpPackets   atomic.Uint64
	rtcpPackets  atomic.Uint64
	stunPackets  atomic.Uint64
	dtlsPackets  atomic.Uint64
	nackPackets  atomic.Uint64
	nackSeqs     atomic.Uint64
	pliPackets   atomic.Uint64
	maxIatUs     atomic.Int64
	lastUdpUs    atomic.Int64
	readErrs     atomic.Uint64
	readTimeouts atomic.Uint64
	readClosed   atomic.Uint64
	lastErrMu    sync.Mutex
	lastErrTag   string

	basePath       string
	endpointsPath  string
	iceServers     []string
	flowMap        map[flowKey]int
	flows          []traceFlow
	endpointsDirty bool
	flowsFile      *bufio.Writer
	flowsFH        *os.File

	file *bufio.Writer
	fh   *os.File
}

func newRxTrace(path string) *rxTrace {
	r := &rxTrace{
		start:    time.Now(),
		ssrcIdx:  map[uint32]int{},
		flowMap:  map[flowKey]int{},
		basePath: path,
	}
	if path != "" {
		if fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			r.fh = fh
			r.file = bufio.NewWriterSize(fh, 1<<16)
			fmt.Fprintf(r.file, "# start_utc=%s fields=t_us,stream,seq,bytes,marker\n", r.start.UTC().Format(time.RFC3339Nano))
		}
		if ffh, err := os.OpenFile(path+".flows.csv", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			r.flowsFH = ffh
			r.flowsFile = bufio.NewWriterSize(ffh, 1<<16)
			fmt.Fprintf(r.flowsFile, "# start_utc=%s fields=t_us,dir,flow_id,kind,encap,ssrc,seq,bytes,marker,fmt,meta\n", r.start.UTC().Format(time.RFC3339Nano))
		}
		r.endpointsPath = path + ".endpoints.json"
	}
	return r
}

func (r *rxTrace) recordICEServers(servers []string) {
	if r == nil || len(servers) == 0 {
		return
	}
	r.mu.Lock()
	r.iceServers = append([]string(nil), servers...)
	r.endpointsDirty = true
	r.mu.Unlock()
	r.flushEndpoints()
}

func splitHostPortSafe(addr string) (string, int) {
	if addr == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func (r *rxTrace) ensureFlowLocked(dir, localAddr, remoteAddr string, us int64, pktBytes uint64) int {
	key := flowKey{dir: dir, local: localAddr, remote: remoteAddr}
	if id, ok := r.flowMap[key]; ok {
		r.flows[id].LastUs = us
		r.flows[id].Packets++
		r.flows[id].Bytes += pktBytes
		return id
	}
	id := len(r.flows)
	lip, lport := splitHostPortSafe(localAddr)
	rip, rport := splitHostPortSafe(remoteAddr)
	r.flowMap[key] = id
	r.flows = append(r.flows, traceFlow{
		ID:         id,
		Dir:        dir,
		LocalIP:    lip,
		LocalPort:  lport,
		RemoteIP:   rip,
		RemotePort: rport,
		FirstUs:    us,
		LastUs:     us,
		Packets:    1,
		Bytes:      pktBytes,
	})
	r.endpointsDirty = true
	return id
}

func (r *rxTrace) flushEndpoints() {
	if r == nil || r.endpointsPath == "" {
		return
	}
	r.mu.Lock()
	if !r.endpointsDirty && len(r.flows) == 0 && len(r.iceServers) == 0 {
		r.mu.Unlock()
		return
	}
	doc := rxEndpointsFile{
		StartUTC:   r.start.UTC().Format(time.RFC3339Nano),
		ICEServers: append([]string(nil), r.iceServers...),
		Flows:      append([]traceFlow(nil), r.flows...),
	}
	r.endpointsDirty = false
	r.mu.Unlock()

	if encoded, err := json.MarshalIndent(doc, "", "  "); err == nil {
		tmp := r.endpointsPath + ".tmp"
		if os.WriteFile(tmp, append(encoded, '\n'), 0o600) == nil {
			_ = os.Rename(tmp, r.endpointsPath)
		}
	}
}

func (r *rxTrace) observeFeedback(kind string, ssrc uint32, seqs []uint16) {
	if r == nil {
		return
	}
	switch kind {
	case "tx_nack":
		r.nackPackets.Add(1)
		r.nackSeqs.Add(uint64(len(seqs)))
	case "tx_pli":
		r.pliPackets.Add(1)
	}
	if r.flowsFile == nil {
		return
	}
	us := r.micros(time.Now())
	var seqVal uint16
	meta := "-"
	if len(seqs) > 0 {
		seqVal = seqs[0]
		parts := make([]string, 0, minInt(len(seqs), 16))
		for i := 0; i < len(seqs) && i < 16; i++ {
			parts = append(parts, strconv.Itoa(int(seqs[i])))
		}
		meta = strings.Join(parts, ";")
		if len(seqs) > 16 {
			meta += fmt.Sprintf("+%d", len(seqs)-16)
		}
	}
	r.mu.Lock()
	if r.flowsFile != nil {
		fmt.Fprintf(r.flowsFile, "%d,%s,-1,rtcp,ctrl,%d,%d,0,0,0,%s\n", us, kind, ssrc, seqVal, meta)
	}
	r.mu.Unlock()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (r *rxTrace) micros(t time.Time) int64 { return t.Sub(r.start).Microseconds() + 1 }

func (r *rxTrace) observe(b []byte) {
	r.observeWithAddr("rx", "", "", b)
}

func (r *rxTrace) observeWithAddr(dir, localAddr, remoteAddr string, raw []byte) {
	if r == nil || len(raw) == 0 {
		return
	}
	now := time.Now()
	us := r.micros(now)
	if dir != "tx" {
		r.udpPackets.Add(1)
		r.udpBytes.Add(uint64(len(raw)))
		if prev := r.lastUdpUs.Swap(us); prev > 0 {
			gap := us - prev
			for {
				cur := r.maxIatUs.Load()
				if gap <= cur || r.maxIatUs.CompareAndSwap(cur, gap) {
					break
				}
			}
		}
	}
	wp := unwrapTURNAndClassify(raw)

	r.mu.Lock()
	flowID := -1
	newFlow := false
	if localAddr != "" || remoteAddr != "" {
		prevLen := len(r.flows)
		flowID = r.ensureFlowLocked(dir, localAddr, remoteAddr, us, uint64(len(raw)))
		newFlow = len(r.flows) > prevLen
	}
	if dir == "tx" {
		if r.flowsFile != nil && (wp.Kind == wireRTCP || wp.Kind == wireRTP) {
			fmt.Fprintf(r.flowsFile, "%d,tx,%d,%s,%s,%d,%d,%d,0,%d,-\n",
				us, flowID, wp.Kind, wp.Encap, wp.SSRC, wp.Seq, len(raw), wp.RTCPFmt)
		}
		r.mu.Unlock()
		if newFlow {
			r.flushEndpoints()
		}
		return
	}

	switch wp.Kind {
	case wireSTUN:
		r.stunPackets.Add(1)
		r.mu.Unlock()
		if newFlow {
			r.flushEndpoints()
		}
		return
	case wireDTLS:
		r.dtlsPackets.Add(1)
		r.mu.Unlock()
		if newFlow {
			r.flushEndpoints()
		}
		return
	case wireRTCP:
		r.rtcpPackets.Add(1)
		if r.flowsFile != nil {
			fmt.Fprintf(r.flowsFile, "%d,rx,%d,rtcp,%s,%d,0,%d,0,%d,-\n",
				us, flowID, wp.Encap, wp.SSRC, len(raw), wp.RTCPFmt)
		}
		r.mu.Unlock()
		if newFlow {
			r.flushEndpoints()
		}
		return
	case wireUnknown:
		r.mu.Unlock()
		return
	}

	r.rtpPackets.Add(1)
	b := wp.Payload
	seq := wp.Seq
	ssrc := wp.SSRC
	idx, ok := r.ssrcIdx[ssrc]
	if !ok {
		if len(r.ssrcIdx) >= len(r.streams) {
			r.mu.Unlock()
			return
		}
		idx = len(r.ssrcIdx)
		r.ssrcIdx[ssrc] = idx
		r.streams[idx] = &rxStream{}
	}
	s := r.streams[idx]
	s.packets++
	if s.started {
		if gap := now.Sub(s.last); gap > s.maxGap {
			s.maxGap, s.maxGapSeq = gap, seq
		}
	}
	s.last = now
	if !s.started {
		s.started, s.maxSeq, s.seen[seq] = true, seq, true
	} else {
		delta := int16(seq - s.maxSeq)
		switch {
		case delta == 1:
			s.seen[seq], s.maxSeq = true, seq
		case delta > 1:
			for q := s.maxSeq + 1; q != seq; q++ {
				s.seen[q], s.missingSet[q] = false, true
			}
			s.seen[seq], s.maxSeq = true, seq
			s.gaps++
			s.skipped += uint64(delta - 1)
			s.outstand += uint64(delta - 1)
		case s.missingSet[seq]:
			s.missingSet[seq], s.seen[seq] = false, true
			s.late++
			if s.outstand > 0 {
				s.outstand--
			}
		case s.seen[seq]:
			s.dup++
		default:
			s.seen[seq] = true
		}
	}
	r.ring[seq] = rxArrival{ssrc: ssrc, at: us}
	marker := 0
	if wp.Marker {
		marker = 1
	}
	if r.file != nil {
		fmt.Fprintf(r.file, "%d,%d,%d,%d,%d\n", us, idx, seq, len(b), marker)
	}
	if r.flowsFile != nil {
		fmt.Fprintf(r.flowsFile, "%d,rx,%d,rtp,%s,%d,%d,%d,%d,0,-\n",
			us, flowID, wp.Encap, ssrc, seq, len(b), marker)
	}
	r.mu.Unlock()
	if newFlow {
		r.flushEndpoints()
	}
}

// lag returns how long a packet sat between socket arrival and the bridge loop
// receiving it, or false if the trace never saw that sequence number.
func (r *rxTrace) lag(ssrc uint32, seq uint16, now time.Time) (time.Duration, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	a := r.ring[seq]
	r.mu.Unlock()
	if a.at == 0 || a.ssrc != ssrc {
		return 0, false
	}
	return time.Duration(r.micros(now)-a.at) * time.Microsecond, true
}

func (r *rxTrace) fields() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	var best *rxStream
	for _, s := range r.streams {
		if s != nil && (best == nil || s.packets > best.packets) {
			best = s
		}
	}
	var video rxStream
	if best != nil {
		video = rxStream{packets: best.packets, gaps: best.gaps, outstand: best.outstand, skipped: best.skipped,
			late: best.late, dup: best.dup, maxGap: best.maxGap}
	}
	if r.file != nil {
		_ = r.file.Flush()
	}
	if r.flowsFile != nil {
		_ = r.flowsFile.Flush()
	}
	dirty := r.endpointsDirty
	r.mu.Unlock()
	if dirty {
		r.flushEndpoints()
	}
	return fmt.Sprintf("rx_udp=%d rx_rtp=%d rx_rtcp=%d rx_stun=%d rx_dtls=%d rx_vpk=%d rx_gaps=%d rx_missing=%d rx_skipped=%d rx_late=%d rx_dup=%d rx_max_vgap_ms=%d rx_max_iat_ms=%d rx_errs=%d rx_timeouts=%d rx_closed=%d rx_last_err=%s nacks=%d nack_seqs=%d plis=%d",
		r.udpPackets.Load(), r.rtpPackets.Load(), r.rtcpPackets.Load(), r.stunPackets.Load(), r.dtlsPackets.Load(),
		video.packets, video.gaps, video.outstand, video.skipped, video.late, video.dup,
		video.maxGap.Milliseconds(), r.maxIatUs.Load()/1000,
		r.readErrs.Load(), r.readTimeouts.Load(), r.readClosed.Load(), r.lastErr(),
		r.nackPackets.Load(), r.nackSeqs.Load(), r.pliPackets.Load())
}

func (r *rxTrace) close() {
	if r == nil {
		return
	}
	r.flushEndpoints()
	r.mu.Lock()
	if r.file != nil {
		_ = r.file.Flush()
	}
	if r.fh != nil {
		_ = r.fh.Close()
		r.file, r.fh = nil, nil
	}
	if r.flowsFile != nil {
		_ = r.flowsFile.Flush()
	}
	if r.flowsFH != nil {
		_ = r.flowsFH.Close()
		r.flowsFile, r.flowsFH = nil, nil
	}
	r.mu.Unlock()
}

// tracedNet makes the ICE agent's UDP sockets report inbound datagram headers.
type tracedNet struct {
	*stdnet.Net
	rx *rxTrace
}

func newTracedNet(rx *rxTrace) (*tracedNet, error) {
	n, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &tracedNet{Net: n, rx: rx}, nil
}

func (n *tracedNet) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	return &tracedUDPConn{UDPConn: c, rx: n.rx}, nil
}

func (n *tracedNet) ListenPacket(network, address string) (net.PacketConn, error) {
	c, err := n.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	return &tracedPacketConn{PacketConn: c, rx: n.rx}, nil
}

type tracedUDPConn struct {
	transport.UDPConn
	rx *rxTrace
}

func (r *rxTrace) lastErr() string {
	if r == nil {
		return "-"
	}
	r.lastErrMu.Lock()
	defer r.lastErrMu.Unlock()
	if r.lastErrTag == "" {
		return "-"
	}
	return r.lastErrTag
}

func (r *rxTrace) observeErr(err error) {
	if r == nil || err == nil {
		return
	}
	closed, timeout, tag := classifySocketReadErr(err)
	switch {
	case closed:
		r.readClosed.Add(1)
	case timeout:
		r.readTimeouts.Add(1)
	default:
		r.readErrs.Add(1)
		if tag != "" {
			r.lastErrMu.Lock()
			r.lastErrTag = tag
			r.lastErrMu.Unlock()
		}
	}
}

func addrStr(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

func (c *tracedUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := c.UDPConn.ReadFrom(b)
	if err == nil {
		c.rx.observeWithAddr("rx", addrStr(c.UDPConn.LocalAddr()), addrStr(a), b[:n])
	} else {
		c.rx.observeErr(err)
	}
	return n, a, err
}

func (c *tracedUDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, a, err := c.UDPConn.ReadFromUDP(b)
	if err == nil {
		rem := ""
		if a != nil {
			rem = a.String()
		}
		c.rx.observeWithAddr("rx", addrStr(c.UDPConn.LocalAddr()), rem, b[:n])
	} else {
		c.rx.observeErr(err)
	}
	return n, a, err
}

func (c *tracedUDPConn) Read(b []byte) (int, error) {
	n, err := c.UDPConn.Read(b)
	if err == nil {
		c.rx.observeWithAddr("rx", addrStr(c.UDPConn.LocalAddr()), addrStr(c.UDPConn.RemoteAddr()), b[:n])
	} else {
		c.rx.observeErr(err)
	}
	return n, err
}

func (c *tracedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.UDPConn.WriteTo(b, addr)
	if err == nil && n > 0 {
		c.rx.observeWithAddr("tx", addrStr(c.UDPConn.LocalAddr()), addrStr(addr), b[:n])
	}
	return n, err
}

func (c *tracedUDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	n, err := c.UDPConn.WriteToUDP(b, addr)
	if err == nil && n > 0 {
		rem := ""
		if addr != nil {
			rem = addr.String()
		}
		c.rx.observeWithAddr("tx", addrStr(c.UDPConn.LocalAddr()), rem, b[:n])
	}
	return n, err
}

func (c *tracedUDPConn) Write(b []byte) (int, error) {
	n, err := c.UDPConn.Write(b)
	if err == nil && n > 0 {
		c.rx.observeWithAddr("tx", addrStr(c.UDPConn.LocalAddr()), addrStr(c.UDPConn.RemoteAddr()), b[:n])
	}
	return n, err
}

type tracedPacketConn struct {
	net.PacketConn
	rx *rxTrace
}

func (c *tracedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(b)
	if err == nil {
		c.rx.observeWithAddr("rx", addrStr(c.PacketConn.LocalAddr()), addrStr(a), b[:n])
	} else {
		c.rx.observeErr(err)
	}
	return n, a, err
}

func (c *tracedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, addr)
	if err == nil && n > 0 {
		c.rx.observeWithAddr("tx", addrStr(c.PacketConn.LocalAddr()), addrStr(addr), b[:n])
	}
	return n, err
}
