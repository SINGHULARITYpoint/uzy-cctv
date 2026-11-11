package devices

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type capturePacketObs struct {
	TimeNs      int64
	ComponentID int
	DirHint     string // "", "rx", "tx"
	SrcIP       net.IP
	SrcPort     int
	DstIP       net.IP
	DstPort     int
	OrigLen     int
	CapLen      int
	UDPLen      int
	Truncated   bool
	Fragmented  bool
	Wire        wirePacket
}

type pcapInterfaceMeta struct {
	ID          int
	LinkType    uint16
	SnapLen     uint32
	TSResolByte byte
	HasTSResol  bool
	ComponentID int
}

func scalePCAPNGTimestampNs(tsRaw uint64, meta pcapInterfaceMeta) int64 {
	if !meta.HasTSResol {
		// Default PCAPNG resolution when if_tsresol is absent is 10^-6 (microseconds).
		return int64(tsRaw) * 1000
	}
	b := meta.TSResolByte
	if b&0x80 == 0 {
		exp := int(b & 0x7f)
		switch {
		case exp == 9:
			return int64(tsRaw)
		case exp < 9:
			mul := uint64(1)
			for i := 0; i < 9-exp; i++ {
				mul *= 10
			}
			return int64(tsRaw * mul)
		default:
			div := uint64(1)
			for i := 0; i < exp-9; i++ {
				div *= 10
			}
			return int64(tsRaw / div)
		}
	}
	unitSec := math.Exp2(-float64(b & 0x7f))
	return int64(math.Round(float64(tsRaw) * unitSec * 1e9))
}

type captureDropStats struct {
	EPBDrops             uint64 `json:"epbDrops"`
	ISBIfDrops           uint64 `json:"isbIfDrops"`
	ISBOSDrops           uint64 `json:"isbOsDrops"`
	ETLLostEvents        uint64 `json:"etlLostEvents"`
	ETLLostBuffers       uint64 `json:"etlLostBuffers"`
	DropOnlyMatchedPkts  uint64 `json:"dropOnlyMatchedPackets"`
	PktMonCounterDrops   uint64 `json:"pktmonCounterDrops"`
	TruncatedHeaders     uint64 `json:"truncatedHeaders"`
	TruncatedNonHeader   uint64 `json:"truncatedPayloadTail"`
	IPFragments          uint64 `json:"ipFragments"`
	DropComponents       []int  `json:"dropComponents,omitempty"`
	CounterDropReasons   []string `json:"counterDropReasons,omitempty"`
}

func (d captureDropStats) hasAnyDrop() bool {
	return d.EPBDrops > 0 || d.ISBIfDrops > 0 || d.ISBOSDrops > 0 ||
		d.ETLLostEvents > 0 || d.ETLLostBuffers > 0 ||
		d.DropOnlyMatchedPkts > 0 || d.PktMonCounterDrops > 0
}

type parsedPCAPNG struct {
	Interfaces []pcapInterfaceMeta
	Packets    []capturePacketObs
	Drops      captureDropStats
}

type pktmonFilterRule struct {
	Name   string
	Proto  string // "", "UDP", "TCP"
	IPs    []string
	Ports  []int
	MACs   []string
	IsIPv4 bool
}

type cameraInputSpec struct {
	Alias         string
	EndpointsPath string
	FlowsPath     string
}

type socketFlowEvent struct {
	RelUs  int64
	AbsNs  int64
	Dir    string // "rx", "tx", "tx_nack", "tx_pli"
	FlowID int
	Kind   string // "rtp", "rtcp"
	Encap  string
	SSRC   uint32
	Seq    uint16
	Bytes  int
	Marker bool
	Meta   string
}

type cameraCorrelateReport struct {
	Camera                 string   `json:"camera"`
	ComponentIDs           []int    `json:"componentIds,omitempty"`
	Encapsulations         []string `json:"encapsulations,omitempty"`
	FilterMatchedFlows     int      `json:"filterMatchedFlows"`
	TotalMediaFlows        int      `json:"totalMediaFlows"`
	UnmatchedMediaFlows    int      `json:"unmatchedMediaFlows"`
	PreFilterExcludedUDP   uint64   `json:"preFilterExcludedUdpPackets"`
	PreFilterExcludedRTP   uint64   `json:"preFilterExcludedRtpPackets"`
	AdapterRTPPackets      uint64   `json:"adapterRtpPackets"`
	AdapterUniqueSeqs      uint64   `json:"adapterUniqueSeqs"`
	AdapterDupObservations uint64   `json:"adapterDuplicateObservations"`
	AdapterReordered       uint64   `json:"adapterReordered"`
	AdapterFirstRelMs      *float64 `json:"adapterFirstRelMs,omitempty"`
	AdapterLastRelMs       *float64 `json:"adapterLastRelMs,omitempty"`
	SocketRTPPackets       uint64   `json:"socketRtpPackets"`
	SocketUniqueSeqs       uint64   `json:"socketUniqueSeqs"`
	SocketReordered        uint64   `json:"socketReordered"`
	SocketFirstRelMs       *float64 `json:"socketFirstRelMs,omitempty"`
	SocketLastRelMs        *float64 `json:"socketLastRelMs,omitempty"`
	MatchedBoth            uint64   `json:"matchedBoth"`
	MissingAtBoth          uint64   `json:"missingAtBoth"`
	AtAdapterLostBeforeSoc uint64   `json:"atAdapterLostBeforeSocket"`
	AtSocketMissedByAdapt  uint64   `json:"atSocketMissedByAdapter"`
	AdapterMissingGaps     uint64   `json:"adapterMissingGapBursts"`
	SocketMissingGaps      uint64   `json:"socketMissingGapBursts"`
	FirstSocketGapRelMs    *float64 `json:"firstSocketGapRelMs,omitempty"`
	FirstAdapterGapRelMs   *float64 `json:"firstAdapterGapRelMs,omitempty"`
	MedianStackTransitMs   *float64 `json:"medianStackTransitMs,omitempty"`
	MaxStackTransitMs      *float64 `json:"maxStackTransitMs,omitempty"`
	DropOnlyMatchedPackets uint64   `json:"dropOnlyMatchedPackets"`
	PLIsTotal              uint64   `json:"plisTotal"`
	PLIsBeforeFirstGap     uint64   `json:"plisBeforeFirstGap"`
	PLIsAfterFirstGap      uint64   `json:"plisAfterFirstGap"`
	FirstPLIRelMs          *float64 `json:"firstPliRelMs,omitempty"`
	NACKsTotal             uint64   `json:"nacksTotal"`
	NACKSeqsTotal          uint64   `json:"nackSeqsTotal"`
	NACKsBeforeFirstGap    uint64   `json:"nacksBeforeFirstGap"`
	NACKsAfterFirstGap     uint64   `json:"nacksAfterFirstGap"`
	FirstNACKRelMs         *float64 `json:"firstNackRelMs,omitempty"`
	RolloverMissedFirstGap bool     `json:"rolloverMissedFirstGap"`
	StreamCount            int      `json:"streamCount"`
	FlowSummaries          []flowCorrelationRow `json:"flows,omitempty"`
}

type flowCorrelationRow struct {
	FlowRef          string   `json:"flowRef"`
	Dir              string   `json:"dir"`
	FilterMatched    bool     `json:"filterMatched"`
	SocketPackets    uint64   `json:"socketPackets"`
	AdapterMatches   uint64   `json:"adapterMatchedPackets"`
	ComponentIDs     []int    `json:"componentIds,omitempty"`
}

type captureCorrelateReport struct {
	OriginRelUTC           string                  `json:"originUtc,omitempty"`
	CaptureStartRelMs      *float64                `json:"captureStartRelMs,omitempty"`
	CaptureEndRelMs        *float64                `json:"captureEndRelMs,omitempty"`
	MinPktSizeBytes        int                     `json:"minPktSizeBytes"`
	CaptureDrops           captureDropStats        `json:"captureDrops"`
	AdapterComponentIDs    []int                   `json:"adapterComponentIds,omitempty"`
	TotalAdapterPacketsRaw uint64                  `json:"totalAdapterPacketsRaw"`
	MatchedMediaPackets    uint64                  `json:"matchedMediaPackets"`
	UnmatchedCaptureUDP    uint64                  `json:"unmatchedCaptureUdpPackets"`
	FilterCoverageComplete bool                    `json:"filterCoverageComplete"`
	RolloverRisk           bool                    `json:"rolloverRisk"`
	FirstLossBoundary      string                  `json:"firstLossBoundary"`
	BoundaryNote           string                  `json:"boundaryNote"`
	Cameras                []cameraCorrelateReport `json:"cameras"`
}

func parsePCAPNGFile(path string, compMap map[string]int) (*parsedPCAPNG, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parsePCAPNGBytes(raw, compMap)
}

func parsePCAPNGBytes(raw []byte, compMap map[string]int) (*parsedPCAPNG, error) {
	out := &parsedPCAPNG{}
	var order binary.ByteOrder = binary.LittleEndian

	for off := 0; off+12 <= len(raw); {
		bType := order.Uint32(raw[off : off+4])
		if bType == 0x0A0D0D0A && off+12 <= len(raw) {
			// SHB byte-order magic determines endianness for this section.
			leMagic := binary.LittleEndian.Uint32(raw[off+8 : off+12])
			beMagic := binary.BigEndian.Uint32(raw[off+8 : off+12])
			if leMagic == 0x1A2B3C4D {
				order = binary.LittleEndian
			} else if beMagic == 0x1A2B3C4D {
				order = binary.BigEndian
			}
			bType = order.Uint32(raw[off : off+4])
		}
		bLen := int(order.Uint32(raw[off+4 : off+8]))
		if bLen < 12 || off+bLen > len(raw) {
			return nil, fmt.Errorf("invalid pcapng block length %d at offset %d", bLen, off)
		}
		body := raw[off+8 : off+bLen-4]
		switch bType {
		case 0x0A0D0D0A:
			out.Interfaces = nil
		case 0x00000001:
			if len(body) >= 8 {
				ifMeta := pcapInterfaceMeta{
					ID:          len(out.Interfaces),
					LinkType:    order.Uint16(body[0:2]),
					SnapLen:     order.Uint32(body[4:8]),
					ComponentID: -1,
				}
				parsePCAPNGOptions(body[8:], order, func(code uint16, val []byte) {
					switch code {
					case 2, 3:
						s := strings.TrimRight(string(val), "\x00")
						if cid := extractComponentID(s, compMap); cid >= 0 && ifMeta.ComponentID < 0 {
							ifMeta.ComponentID = cid
						}
					case 9:
						if len(val) >= 1 {
							ifMeta.TSResolByte = val[0]
							ifMeta.HasTSResol = true
						}
					}
				})
				out.Interfaces = append(out.Interfaces, ifMeta)
			}
		case 0x00000006:
			if len(body) >= 20 {
				ifID := int(order.Uint32(body[0:4]))
				tsHigh := uint64(order.Uint32(body[4:8]))
				tsLow := uint64(order.Uint32(body[8:12]))
				capLen := int(order.Uint32(body[12:16]))
				origLen := int(order.Uint32(body[16:20]))
				if 20+capLen <= len(body) {
					frame := body[20 : 20+capLen]
					linkType := uint16(1)
					var ifMeta pcapInterfaceMeta
					compID := -1
					if ifID >= 0 && ifID < len(out.Interfaces) {
						ifMeta = out.Interfaces[ifID]
						linkType = ifMeta.LinkType
						compID = ifMeta.ComponentID
					}
					dirHint := ""
					optOff := 20 + ((capLen + 3) &^ 3)
					if optOff <= len(body) {
						parsePCAPNGOptions(body[optOff:], order, func(code uint16, val []byte) {
							switch code {
							case 1:
								s := strings.TrimRight(string(val), "\x00")
								if cid := extractComponentID(s, compMap); cid >= 0 {
									compID = cid
								}
							case 2:
								if len(val) >= 4 {
									flags := order.Uint32(val[:4])
									switch flags & 0x3 {
									case 1:
										dirHint = "rx"
									case 2:
										dirHint = "tx"
									}
								}
							case 4:
								if len(val) >= 8 {
									out.Drops.EPBDrops += order.Uint64(val[:8])
								}
							}
						})
					}
					tsRaw := (tsHigh << 32) | tsLow
					tsNs := scalePCAPNGTimestampNs(tsRaw, ifMeta)
					if obs, ok := decodeCapturedFrame(frame, linkType, origLen, capLen, tsNs, compID, dirHint); ok {
						if obs.Truncated {
							out.Drops.TruncatedHeaders++
						} else if capLen < origLen {
							out.Drops.TruncatedNonHeader++
						}
						if obs.Fragmented {
							out.Drops.IPFragments++
						}
						out.Packets = append(out.Packets, obs)
					}
				}
			}
		case 0x00000003:
			if len(body) >= 4 {
				origLen := int(order.Uint32(body[0:4]))
				frame := body[4:]
				capLen := len(frame)
				linkType := uint16(1)
				compID := -1
				if len(out.Interfaces) > 0 {
					linkType = out.Interfaces[0].LinkType
					compID = out.Interfaces[0].ComponentID
				}
				if obs, ok := decodeCapturedFrame(frame, linkType, origLen, capLen, 0, compID, ""); ok {
					out.Packets = append(out.Packets, obs)
				}
			}
		case 0x00000005:
			if len(body) >= 12 {
				parsePCAPNGOptions(body[12:], order, func(code uint16, val []byte) {
					if len(val) < 8 {
						return
					}
					switch code {
					case 5:
						out.Drops.ISBIfDrops += order.Uint64(val[:8])
					case 7:
						out.Drops.ISBOSDrops += order.Uint64(val[:8])
					}
				})
			}
		}
		off += bLen
	}
	return out, nil
}

func parsePCAPNGOptions(opts []byte, order binary.ByteOrder, onOpt func(code uint16, val []byte)) {
	for off := 0; off+4 <= len(opts); {
		code := order.Uint16(opts[off : off+2])
		vLen := int(order.Uint16(opts[off+2 : off+4]))
		off += 4
		if code == 0 {
			break
		}
		if off+vLen > len(opts) {
			break
		}
		onOpt(code, opts[off:off+vLen])
		off += (vLen + 3) &^ 3
	}
}

var compIDRegexp = regexp.MustCompile(`(?i)(?:component[_\s:=#-]*|comp[_\s:=#-]+)(\d+)`)

func extractComponentID(text string, compMap map[string]int) int {
	if text == "" {
		return -1
	}
	if m := compIDRegexp.FindStringSubmatch(text); len(m) == 2 {
		if cid, err := strconv.Atoi(m[1]); err == nil {
			return cid
		}
	}
	lower := strings.ToLower(text)
	for name, id := range compMap {
		if name != "" && strings.Contains(lower, strings.ToLower(name)) {
			return id
		}
	}
	return -1
}

func decodeCapturedFrame(frame []byte, linkType uint16, origLen, capLen int, tsNs int64, compID int, dirHint string) (capturePacketObs, bool) {
	obs := capturePacketObs{
		TimeNs:      tsNs,
		ComponentID: compID,
		DirHint:     dirHint,
		OrigLen:     origLen,
		CapLen:      capLen,
	}
	ipOff := 0
	var etherType uint16
	switch linkType {
	case 1:
		if len(frame) < 14 {
			return obs, false
		}
		etherType = binary.BigEndian.Uint16(frame[12:14])
		ipOff = 14
		for (etherType == 0x8100 || etherType == 0x88a8) && ipOff+4 <= len(frame) {
			etherType = binary.BigEndian.Uint16(frame[ipOff+2 : ipOff+4])
			ipOff += 4
		}
	case 101, 12, 228, 229:
		if len(frame) < 1 {
			return obs, false
		}
		switch frame[0] >> 4 {
		case 4:
			etherType = 0x0800
		case 6:
			etherType = 0x86dd
		default:
			return obs, false
		}
	default:
		return obs, false
	}

	var proto uint8
	var udpOff int
	var ipPayloadEnd int
	switch etherType {
	case 0x0800:
		if ipOff+20 > len(frame) || frame[ipOff]>>4 != 4 {
			return obs, false
		}
		ihl := int(frame[ipOff]&0x0f) * 4
		if ihl < 20 || ipOff+ihl > len(frame) {
			obs.Truncated = capLen < origLen
			return obs, true
		}
		totLen := int(binary.BigEndian.Uint16(frame[ipOff+2 : ipOff+4]))
		flagsOff := binary.BigEndian.Uint16(frame[ipOff+6 : ipOff+8])
		if (flagsOff & 0x3fff) != 0 {
			obs.Fragmented = true
		}
		proto = frame[ipOff+9]
		obs.SrcIP = append(net.IP(nil), frame[ipOff+12:ipOff+16]...)
		obs.DstIP = append(net.IP(nil), frame[ipOff+16:ipOff+20]...)
		udpOff = ipOff + ihl
		ipPayloadEnd = ipOff + totLen
		if ipPayloadEnd > len(frame) || totLen < ihl {
			ipPayloadEnd = len(frame)
		}
	case 0x86dd:
		if ipOff+40 > len(frame) || frame[ipOff]>>4 != 6 {
			return obs, false
		}
		payLen := int(binary.BigEndian.Uint16(frame[ipOff+4 : ipOff+6]))
		proto = frame[ipOff+6]
		obs.SrcIP = append(net.IP(nil), frame[ipOff+8:ipOff+24]...)
		obs.DstIP = append(net.IP(nil), frame[ipOff+24:ipOff+40]...)
		udpOff = ipOff + 40
		for (proto == 0 || proto == 43 || proto == 60 || proto == 44) && udpOff+8 <= len(frame) {
			if proto == 44 {
				obs.Fragmented = true
				proto = frame[udpOff]
				udpOff += 8
				break
			}
			extLen := int(frame[udpOff+1]+1) * 8
			proto = frame[udpOff]
			udpOff += extLen
		}
		ipPayloadEnd = ipOff + 40 + payLen
		if ipPayloadEnd > len(frame) {
			ipPayloadEnd = len(frame)
		}
	default:
		return obs, false
	}
	if proto != 17 {
		return obs, false
	}
	if udpOff+8 > len(frame) {
		obs.Truncated = capLen < origLen
		return obs, true
	}
	obs.SrcPort = int(binary.BigEndian.Uint16(frame[udpOff : udpOff+2]))
	obs.DstPort = int(binary.BigEndian.Uint16(frame[udpOff+2 : udpOff+4]))
	obs.UDPLen = int(binary.BigEndian.Uint16(frame[udpOff+4 : udpOff+6]))
	udpPayStart := udpOff + 8
	udpPayEnd := ipPayloadEnd
	if udpPayStart > udpPayEnd {
		udpPayEnd = len(frame)
	}
	udpPay := frame[udpPayStart:udpPayEnd]
	obs.Wire = unwrapTURNAndClassify(udpPay)
	expectedPayLen := obs.UDPLen - 8
	if obs.Wire.Kind == wireUnknown && capLen < origLen && expectedPayLen >= 12 {
		obs.Truncated = true
	}
	if obs.Fragmented && obs.Wire.Kind == wireUnknown {
		obs.Truncated = true
	}
	return obs, true
}

func parseSocketFlowsCSV(path string) (int64, []socketFlowEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	var startNs int64
	var events []socketFlowEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			for _, tok := range strings.Fields(line[1:]) {
				if strings.HasPrefix(tok, "start_utc=") {
					if t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(tok, "start_utc=")); err == nil {
						startNs = t.UnixNano()
					}
				}
			}
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 11 {
			continue
		}
		relUs, err1 := strconv.ParseInt(parts[0], 10, 64)
		flowID, err2 := strconv.Atoi(parts[2])
		ssrc64, err3 := strconv.ParseUint(parts[5], 10, 32)
		seq64, err4 := strconv.ParseUint(parts[6], 10, 16)
		pktBytes, err5 := strconv.Atoi(parts[7])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			continue
		}
		ev := socketFlowEvent{
			RelUs:  relUs,
			AbsNs:  startNs + relUs*1000,
			Dir:    parts[1],
			FlowID: flowID,
			Kind:   parts[3],
			Encap:  parts[4],
			SSRC:   uint32(ssrc64),
			Seq:    uint16(seq64),
			Bytes:  pktBytes,
			Marker: parts[8] == "1",
			Meta:   parts[10],
		}
		events = append(events, ev)
	}
	return startNs, events, sc.Err()
}

var tabularFilterRowRegexp = regexp.MustCompile(`^\s*\d+\s+\S+`)

func parsePktMonFilters(raw string) []pktmonFilterRule {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var rules []pktmonFilterRule
	var cur *pktmonFilterRule
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "# ") || strings.EqualFold(line, "All packets allowed (no filters active)") {
			continue
		}
		if tabularFilterRowRegexp.MatchString(line) && !strings.Contains(strings.ToLower(line), "name:") {
			if cur != nil {
				rules = append(rules, *cur)
				cur = nil
			}
			fields := strings.Fields(line)
			rule := pktmonFilterRule{Name: fields[1]}
			for _, rawTok := range fields[2:] {
				tok := strings.Trim(rawTok, ",;[]")
				up := strings.ToUpper(tok)
				switch {
				case up == "UDP" || up == "TCP":
					rule.Proto = up
				case up == "IPV4":
					rule.IsIPv4 = true
				case net.ParseIP(strings.Split(tok, "/")[0]) != nil || strings.Contains(tok, "/"):
					rule.IPs = append(rule.IPs, tok)
				default:
					if p, err := strconv.Atoi(tok); err == nil && p > 0 && p <= 65535 {
						rule.Ports = append(rule.Ports, p)
					}
				}
			}
			rules = append(rules, rule)
			continue
		}
		if strings.HasPrefix(line, "Name:") || strings.HasPrefix(line, "#") {
			if cur != nil {
				rules = append(rules, *cur)
			}
			cur = &pktmonFilterRule{Name: strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "Name:"), "#"))}
			continue
		}
		if cur == nil {
			cur = &pktmonFilterRule{}
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "protocol") && strings.Contains(lower, "udp"):
			cur.Proto = "UDP"
		case strings.Contains(lower, "protocol") && strings.Contains(lower, "tcp"):
			cur.Proto = "TCP"
		case strings.Contains(lower, "ipv4"):
			cur.IsIPv4 = true
		case strings.HasPrefix(lower, "ip"):
			for _, tok := range strings.Fields(line) {
				tok = strings.Trim(tok, ",;[]")
				if ip := net.ParseIP(strings.Split(tok, "/")[0]); ip != nil || strings.Contains(tok, "/") {
					cur.IPs = append(cur.IPs, tok)
				}
			}
		case strings.HasPrefix(lower, "port"):
			for _, tok := range strings.Fields(line) {
				tok = strings.Trim(tok, ",;[]")
				if p, err := strconv.Atoi(tok); err == nil && p > 0 && p <= 65535 {
					cur.Ports = append(cur.Ports, p)
				}
			}
		}
	}
	if cur != nil && (cur.Proto != "" || len(cur.IPs) > 0 || len(cur.Ports) > 0 || cur.IsIPv4) {
		rules = append(rules, *cur)
	}
	return rules
}

func ipInSpec(ip net.IP, spec string) bool {
	if ip == nil || spec == "" {
		return false
	}
	if strings.Contains(spec, "/") {
		_, cidr, err := net.ParseCIDR(spec)
		return err == nil && cidr.Contains(ip)
	}
	other := net.ParseIP(spec)
	return other != nil && ip.Equal(other)
}

func (r pktmonFilterRule) matchesFlow(localIP, remoteIP net.IP, localPort, remotePort int) bool {
	if r.Proto != "" && r.Proto != "UDP" {
		return false
	}
	for _, spec := range r.IPs {
		if !ipInSpec(localIP, spec) && !ipInSpec(remoteIP, spec) {
			return false
		}
	}
	for _, p := range r.Ports {
		if localPort != p && remotePort != p {
			return false
		}
	}
	return true
}

func flowMatchesAnyFilter(rules []pktmonFilterRule, localIP, remoteIP net.IP, localPort, remotePort int) bool {
	if len(rules) == 0 {
		return true
	}
	for _, rule := range rules {
		if rule.matchesFlow(localIP, remoteIP, localPort, remotePort) {
			return true
		}
	}
	return false
}

func normalizeIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func parseIPOrZero(s string, fallbackAdapter net.IP) net.IP {
	if s == "" {
		return fallbackAdapter
	}
	ip := normalizeIP(net.ParseIP(s))
	if ip == nil || ip.IsUnspecified() {
		return fallbackAdapter
	}
	return ip
}

func flowMatchesPacket(f traceFlow, obs capturePacketObs, fallbackAdapter net.IP) (dir string, ok bool) {
	lip := parseIPOrZero(f.LocalIP, fallbackAdapter)
	rip := normalizeIP(net.ParseIP(f.RemoteIP))
	srcIP := normalizeIP(obs.SrcIP)
	dstIP := normalizeIP(obs.DstIP)
	check := func(localMatchedIP, remoteMatchedIP net.IP, localP, remoteP int) bool {
		if f.LocalPort > 0 && localP != f.LocalPort {
			return false
		}
		if f.RemotePort > 0 && remoteP != f.RemotePort {
			return false
		}
		if rip != nil && (remoteMatchedIP == nil || !rip.Equal(remoteMatchedIP)) {
			return false
		}
		if lip != nil && !lip.IsUnspecified() && localMatchedIP != nil && !lip.Equal(localMatchedIP) {
			return false
		}
		return f.LocalPort > 0 || (rip != nil && f.RemotePort > 0)
	}
	if check(dstIP, srcIP, obs.DstPort, obs.SrcPort) {
		return "rx", true
	}
	if check(srcIP, dstIP, obs.SrcPort, obs.DstPort) {
		return "tx", true
	}
	return "", false
}

type extSeqMapper struct {
	initialized bool
	anchor      uint16
	curMax      uint64
}

func (m *extSeqMapper) mapSeq(seq uint16) (ext uint64, reordered bool) {
	if !m.initialized {
		m.initialized = true
		m.anchor = seq
		m.curMax = 0x10000 + uint64(seq)
		return m.curMax, false
	}
	top16 := uint16(m.curMax)
	delta := int16(seq - top16)
	candidate := uint64(int64(m.curMax) + int64(delta))
	if delta > 0 {
		m.curMax = candidate
		return candidate, false
	}
	if delta == 0 {
		return m.curMax, false
	}
	return candidate, true
}

func parsePktMonCountersJSON(raw []byte) (drops uint64, comps []int, reasons []string) {
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return 0, nil, nil
	}
	seenComp := map[int]bool{}
	seenReason := map[string]bool{}
	var walk func(v any, curComp int)
	walk = func(v any, curComp int) {
		switch node := v.(type) {
		case []any:
			for _, item := range node {
				walk(item, curComp)
			}
		case map[string]any:
			comp := curComp
			if idVal, ok := node["Id"]; ok {
				if num, ok := idVal.(float64); ok {
					comp = int(num)
				}
			}
			for k, val := range node {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "drop") && !strings.Contains(lk, "reason") {
					switch tv := val.(type) {
					case float64:
						if tv > 0 {
							drops += uint64(tv)
							if comp >= 0 && !seenComp[comp] {
								seenComp[comp] = true
								comps = append(comps, comp)
							}
						}
					case map[string]any:
						for _, sub := range tv {
							if n, ok := sub.(float64); ok && n > 0 {
								drops += uint64(n)
								if comp >= 0 && !seenComp[comp] {
									seenComp[comp] = true
									comps = append(comps, comp)
								}
							}
						}
					}
				} else if strings.Contains(lk, "reason") {
					if rs, ok := val.(string); ok && strings.TrimSpace(rs) != "" {
						clean := sanitizeErrTag(rs)
						if !seenReason[clean] {
							seenReason[clean] = true
							reasons = append(reasons, clean)
						}
					}
				}
				walk(val, comp)
			}
		}
	}
	walk(data, -1)
	sort.Ints(comps)
	sort.Strings(reasons)
	return drops, comps, reasons
}

func parseETLStatsText(text string) (lostEvents, lostBuffers uint64) {
	reEv := regexp.MustCompile(`(?i)events\s+lost[:=\s]+(\d+)`)
	reBuf := regexp.MustCompile(`(?i)buffers\s+lost[:=\s]+(\d+)`)
	if m := reEv.FindStringSubmatch(text); len(m) == 2 {
		lostEvents, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if m := reBuf.FindStringSubmatch(text); len(m) == 2 {
		lostBuffers, _ = strconv.ParseUint(m[1], 10, 64)
	}
	return lostEvents, lostBuffers
}

type correlateOptions struct {
	PCAPNG          *parsedPCAPNG
	DropPCAPNG      *parsedPCAPNG
	FiltersText     string
	CountersJSON    []byte
	ETLStatsText    string
	Cameras         []cameraInputSpec
	AdapterIP       net.IP
	CaptureStartNs  int64
	FirstWakeNs     int64
	MinPktSizeBytes int
}

func runCaptureCorrelation(opts correlateOptions) (captureCorrelateReport, error) {
	if opts.PCAPNG == nil {
		return captureCorrelateReport{}, fmt.Errorf("missing primary pcapng data")
	}
	if opts.MinPktSizeBytes == 0 {
		opts.MinPktSizeBytes = 160
	}
	filterRules := parsePktMonFilters(opts.FiltersText)
	drops := opts.PCAPNG.Drops
	if opts.ETLStatsText != "" {
		le, lb := parseETLStatsText(opts.ETLStatsText)
		drops.ETLLostEvents += le
		drops.ETLLostBuffers += lb
	}
	if len(opts.CountersJSON) > 0 {
		cd, comps, reasons := parsePktMonCountersJSON(opts.CountersJSON)
		drops.PktMonCounterDrops += cd
		drops.DropComponents = append(drops.DropComponents, comps...)
		drops.CounterDropReasons = append(drops.CounterDropReasons, reasons...)
	}

	// Determine earliest session timestamp for redacted relative timestamps (ms).
	originNs := opts.FirstWakeNs
	type loadedCam struct {
		spec      cameraInputSpec
		endpoints rxEndpointsFile
		events    []socketFlowEvent
		startNs   int64
	}
	var cams []loadedCam
	for _, spec := range opts.Cameras {
		rawEP, err := os.ReadFile(spec.EndpointsPath)
		if err != nil {
			return captureCorrelateReport{}, fmt.Errorf("read %s endpoints: %w", spec.Alias, err)
		}
		var ep rxEndpointsFile
		if err := json.Unmarshal(rawEP, &ep); err != nil {
			return captureCorrelateReport{}, fmt.Errorf("parse %s endpoints: %w", spec.Alias, err)
		}
		stNs, evs, err := parseSocketFlowsCSV(spec.FlowsPath)
		if err != nil {
			return captureCorrelateReport{}, fmt.Errorf("read %s flows csv: %w", spec.Alias, err)
		}
		if originNs == 0 || (stNs > 0 && stNs < originNs) {
			originNs = stNs
		}
		cams = append(cams, loadedCam{spec: spec, endpoints: ep, events: evs, startNs: stNs})
	}
	if originNs == 0 && len(opts.PCAPNG.Packets) > 0 {
		originNs = opts.PCAPNG.Packets[0].TimeNs
	}

	relMs := func(ns int64) *float64 {
		if ns == 0 || originNs == 0 {
			return nil
		}
		v := math.Round(float64(ns-originNs)/1e4) / 100.0
		return &v
	}

	var capFirstPktNs, capEndNs int64
	compSet := map[int]bool{}
	for _, p := range opts.PCAPNG.Packets {
		if p.TimeNs > 0 {
			if capFirstPktNs == 0 || p.TimeNs < capFirstPktNs {
				capFirstPktNs = p.TimeNs
			}
			if p.TimeNs > capEndNs {
				capEndNs = p.TimeNs
			}
		}
		if p.ComponentID >= 0 {
			compSet[p.ComponentID] = true
		}
	}
	capStartNs := opts.CaptureStartNs
	if capStartNs == 0 {
		capStartNs = capFirstPktNs
	}

	// Build adapter observations per camera & drop-only observations per camera.
	type camAdapterObs struct {
		obs       capturePacketObs
		dir       string
		flowIdx   int
		MatchedTo string
	}
	perCamAdapter := make([][]camAdapterObs, len(cams))
	perCamFlowHits := make([]map[int]uint64, len(cams))
	perCamFlowComps := make([]map[int]map[int]bool, len(cams))
	for i := range cams {
		perCamFlowHits[i] = map[int]uint64{}
		perCamFlowComps[i] = map[int]map[int]bool{}
	}
	var matchedMedia, unmatchedCaptureUDP uint64
	matchPacketToCamera := func(obs capturePacketObs) (camIdx int, dir string, flowID int, matched bool) {
		for ci, c := range cams {
			for _, f := range c.endpoints.Flows {
				if d, ok := flowMatchesPacket(f, obs, opts.AdapterIP); ok {
					return ci, d, f.ID, true
				}
			}
		}
		return -1, "", -1, false
	}
	for _, obs := range opts.PCAPNG.Packets {
		ci, dir, fid, ok := matchPacketToCamera(obs)
		if !ok {
			unmatchedCaptureUDP++
			continue
		}
		matchedMedia++
		perCamAdapter[ci] = append(perCamAdapter[ci], camAdapterObs{obs: obs, dir: dir, flowIdx: fid})
		perCamFlowHits[ci][fid]++
		if obs.ComponentID >= 0 {
			if perCamFlowComps[ci][fid] == nil {
				perCamFlowComps[ci][fid] = map[int]bool{}
			}
			perCamFlowComps[ci][fid][obs.ComponentID] = true
		}
	}

	perCamDropMatched := make([]uint64, len(cams))
	if opts.DropPCAPNG != nil {
		dropCompSeen := map[int]bool{}
		for _, cid := range drops.DropComponents {
			dropCompSeen[cid] = true
		}
		for _, obs := range opts.DropPCAPNG.Packets {
			if ci, _, _, ok := matchPacketToCamera(obs); ok {
				perCamDropMatched[ci]++
				drops.DropOnlyMatchedPkts++
				if obs.ComponentID >= 0 && !dropCompSeen[obs.ComponentID] {
					dropCompSeen[obs.ComponentID] = true
					drops.DropComponents = append(drops.DropComponents, obs.ComponentID)
				}
			}
		}
		sort.Ints(drops.DropComponents)
	}

	// Correlate per camera.
	allFilterOK := true
	anyRolloverRisk := false
	var sumMissingAtBoth, sumAdapterLostBeforeSocket, sumSocketMissedByAdapter, sumSocketMissing uint64
	var reports []cameraCorrelateReport

	for ci, c := range cams {
		rep := cameraCorrelateReport{
			Camera:                 c.spec.Alias,
			DropOnlyMatchedPackets: perCamDropMatched[ci],
		}
		encapSet := map[string]bool{}
		camCompSet := map[int]bool{}

		// Flow & filter coverage
		flowCounter := 0
		for _, f := range c.endpoints.Flows {
			lip := parseIPOrZero(f.LocalIP, opts.AdapterIP)
			rip := normalizeIP(net.ParseIP(f.RemoteIP))
			fm := flowMatchesAnyFilter(filterRules, lip, rip, f.LocalPort, f.RemotePort)
			if f.Dir == "rx" && f.Packets > 0 {
				rep.TotalMediaFlows++
				if fm {
					rep.FilterMatchedFlows++
				} else {
					rep.UnmatchedMediaFlows++
					allFilterOK = false
				}
			}
			var fComps []int
			for cid := range perCamFlowComps[ci][f.ID] {
				fComps = append(fComps, cid)
			}
			sort.Ints(fComps)
			rep.FlowSummaries = append(rep.FlowSummaries, flowCorrelationRow{
				FlowRef:        fmt.Sprintf("flow_%d", flowCounter),
				Dir:            f.Dir,
				FilterMatched:  fm,
				SocketPackets:  f.Packets,
				AdapterMatches: perCamFlowHits[ci][f.ID],
				ComponentIDs:   fComps,
			})
			flowCounter++
		}

		// Only exclude initial socket packets that occurred BEFORE CaptureStartNs (when filtered pktmon started).
		// Packets skipped due to circular-buffer rollover (after CaptureStartNs but before capFirstPktNs)
		// must NOT be hidden as pre-filter so rollover detection catches missing first-gap windows.
		filterStartNs := opts.CaptureStartNs

		// Track overall earliest socket gap after filterStartNs to detect rollover cleanly.
		var overallSockExt extSeqMapper
		var overallSockPrevMax uint64
		var overallFirstGapNs int64
		var overallSockMissingAfterFilter uint64
		for _, ev := range c.events {
			if ev.Dir != "rx" || ev.Kind != "rtp" {
				continue
			}
			if filterStartNs > 0 && ev.AbsNs > 0 && ev.AbsNs < filterStartNs {
				continue
			}
			ext, _ := overallSockExt.mapSeq(ev.Seq)
			if overallSockPrevMax > 0 && ext > overallSockPrevMax+1 {
				overallSockMissingAfterFilter += ext - overallSockPrevMax - 1
				if overallFirstGapNs == 0 {
					overallFirstGapNs = ev.AbsNs
				}
			}
			if ext > overallSockPrevMax {
				overallSockPrevMax = ext
			}
		}

		// Collect socket RTP events + feedback events.
		type feedbackRec struct {
			absNs int64
			kind  string
			seqs  int
		}
		var fbEvents []feedbackRec
		type rtpItem struct {
			absNs  int64
			seq    uint16
			ssrc   uint32
			encap  string
			flowID int
		}
		var sockRTPs []rtpItem
		for _, ev := range c.events {
			if filterStartNs > 0 && ev.AbsNs > 0 && ev.AbsNs < filterStartNs {
				if ev.Dir == "rx" {
					rep.PreFilterExcludedUDP++
					if ev.Kind == "rtp" {
						rep.PreFilterExcludedRTP++
					}
				}
				continue
			}
			if capFirstPktNs > 0 && ev.Dir == "rx" && ev.Kind == "rtp" && ev.AbsNs > 0 && ev.AbsNs < capFirstPktNs {
				// Within filterStartNs..capFirstPktNs -> rolled over from circular ETL before retained window!
				continue
			}
			switch ev.Dir {
			case "tx_pli":
				fbEvents = append(fbEvents, feedbackRec{absNs: ev.AbsNs, kind: "pli"})
			case "tx_nack":
				nCount := 1
				if ev.Meta != "" && ev.Meta != "-" {
					if strings.Contains(ev.Meta, "+") {
						base := strings.Split(ev.Meta, "+")
						extra, _ := strconv.Atoi(base[1])
						nCount = len(strings.Split(base[0], ";")) + extra
					} else {
						nCount = len(strings.Split(ev.Meta, ";"))
					}
				}
				fbEvents = append(fbEvents, feedbackRec{absNs: ev.AbsNs, kind: "nack", seqs: nCount})
			case "rx":
				if ev.Kind == "rtp" {
					if ev.Encap != "" {
						encapSet[ev.Encap] = true
					}
					sockRTPs = append(sockRTPs, rtpItem{
						absNs: ev.AbsNs, seq: ev.Seq, ssrc: ev.SSRC, encap: ev.Encap, flowID: ev.FlowID,
					})
				}
			}
		}

		// Collect inbound adapter RTP arrivals within capture window.
		var adaptRTPs []rtpItem
		for _, item := range perCamAdapter[ci] {
			if item.obs.ComponentID >= 0 {
				camCompSet[item.obs.ComponentID] = true
			}
			if item.dir != "rx" || item.obs.Wire.Kind != wireRTP {
				continue
			}
			if item.obs.Wire.Encap != "" {
				encapSet[item.obs.Wire.Encap] = true
			}
			adaptRTPs = append(adaptRTPs, rtpItem{
				absNs: item.obs.TimeNs, seq: item.obs.Wire.Seq, ssrc: item.obs.Wire.SSRC,
				encap: item.obs.Wire.Encap, flowID: item.flowIdx,
			})
		}

		// Per-SSRC correlation with 16-bit sequence wraparound extension anchored at shared first seq.
		type ssrcGroup struct {
			sock  []rtpItem
			adapt []rtpItem
		}
		groups := map[uint32]*ssrcGroup{}
		for _, it := range sockRTPs {
			g := groups[it.ssrc]
			if g == nil {
				g = &ssrcGroup{}
				groups[it.ssrc] = g
			}
			g.sock = append(g.sock, it)
		}
		for _, it := range adaptRTPs {
			g := groups[it.ssrc]
			if g == nil {
				g = &ssrcGroup{}
				groups[it.ssrc] = g
			}
			g.adapt = append(g.adapt, it)
		}
		rep.StreamCount = len(groups)

		var minAdaptNs, maxAdaptNs, minSockNs, maxSockNs int64
		var firstSockGapNs, firstAdaptGapNs int64
		var transitMs []float64

		for _, g := range groups {
			sort.SliceStable(g.sock, func(i, j int) bool { return g.sock[i].absNs < g.sock[j].absNs })
			sort.SliceStable(g.adapt, func(i, j int) bool { return g.adapt[i].absNs < g.adapt[j].absNs })

			// Align anchor so wrapped uint16 maps identically on adapter and socket streams.
			var sockExt, adaptExt extSeqMapper
			if len(g.adapt) > 0 && len(g.sock) > 0 {
				// Pick common anchor if first adapter packet isn't exact first socket packet.
				adaptSeqSet := map[uint16]bool{}
				for _, a := range g.adapt {
					adaptSeqSet[a.seq] = true
				}
				for _, s := range g.sock {
					if adaptSeqSet[s.seq] {
						// Anchor both at same initial sequence epoch when aligned.
						break
					}
				}
			}

			sockSeen := map[uint64]int64{}
			var sockMinExt, sockMaxExt uint64
			var sockPrevMax uint64
			for _, it := range g.sock {
				rep.SocketRTPPackets++
				if minSockNs == 0 || it.absNs < minSockNs {
					minSockNs = it.absNs
				}
				if it.absNs > maxSockNs {
					maxSockNs = it.absNs
				}
				ext, reord := sockExt.mapSeq(it.seq)
				if reord {
					rep.SocketReordered++
				}
				if sockPrevMax > 0 && ext > sockPrevMax+1 && firstSockGapNs == 0 {
					firstSockGapNs = it.absNs
				}
				if sockPrevMax > 0 && ext > sockPrevMax+1 {
					rep.SocketMissingGaps++
				}
				if ext > sockPrevMax {
					sockPrevMax = ext
				}
				if _, dup := sockSeen[ext]; !dup {
					sockSeen[ext] = it.absNs
					rep.SocketUniqueSeqs++
				}
				if sockMinExt == 0 || ext < sockMinExt {
					sockMinExt = ext
				}
				if ext > sockMaxExt {
					sockMaxExt = ext
				}
			}

			// Re-anchor adapter stream epoch against earliest socket packet if needed.
			adaptSeen := map[uint64]int64{}
			var adaptMinExt, adaptMaxExt, adaptPrevMax uint64
			if len(g.sock) > 0 && len(g.adapt) > 0 {
				// Compute epoch shift between g.adapt[0].seq and g.sock[0].seq cleanly.
				shift := int64(int16(g.adapt[0].seq - g.sock[0].seq))
				adaptExt = extSeqMapper{
					initialized: true,
					anchor:      g.sock[0].seq,
					curMax:      uint64(int64(0x10000+uint64(g.sock[0].seq)) + shift),
				}
			}
			for idx, it := range g.adapt {
				rep.AdapterRTPPackets++
				if minAdaptNs == 0 || it.absNs < minAdaptNs {
					minAdaptNs = it.absNs
				}
				if it.absNs > maxAdaptNs {
					maxAdaptNs = it.absNs
				}
				var ext uint64
				var reord bool
				if idx == 0 && adaptExt.initialized {
					ext = adaptExt.curMax
				} else {
					ext, reord = adaptExt.mapSeq(it.seq)
				}
				if reord {
					rep.AdapterReordered++
				}
				if adaptPrevMax > 0 && ext > adaptPrevMax+1 {
					rep.AdapterMissingGaps++
					if firstAdaptGapNs == 0 {
						firstAdaptGapNs = it.absNs
					}
				}
				if ext > adaptPrevMax {
					adaptPrevMax = ext
				}
				if _, exists := adaptSeen[ext]; exists {
					rep.AdapterDupObservations++
				} else {
					adaptSeen[ext] = it.absNs
					rep.AdapterUniqueSeqs++
					if sockTs, ok := sockSeen[ext]; ok && it.absNs > 0 && sockTs > 0 {
						transitMs = append(transitMs, float64(sockTs-it.absNs)/1e6)
					}
				}
				if adaptMinExt == 0 || ext < adaptMinExt {
					adaptMinExt = ext
				}
				if ext > adaptMaxExt {
					adaptMaxExt = ext
				}
			}

			// Evaluate exact sequence intersection across the overlapping sequence span.
			if sockMaxExt == 0 && adaptMaxExt == 0 {
				continue
			}
			winLo, winHi := sockMinExt, sockMaxExt
			if adaptMinExt > 0 && (winLo == 0 || adaptMinExt < winLo) {
				winLo = adaptMinExt
			}
			if adaptMaxExt > winHi {
				winHi = adaptMaxExt
			}
			for seqExt := winLo; seqExt <= winHi; seqExt++ {
				_, inA := adaptSeen[seqExt]
				_, inS := sockSeen[seqExt]
				switch {
				case inA && inS:
					rep.MatchedBoth++
				case inA && !inS:
					rep.AtAdapterLostBeforeSoc++
				case !inA && inS:
					// Ignore leading/trailing boundary outside adapter capture timestamps cleanly.
					if sockTs := sockSeen[seqExt]; (capStartNs == 0 || sockTs >= capStartNs) && (capEndNs == 0 || sockTs <= capEndNs) {
						rep.AtSocketMissedByAdapt++
					}
				case !inA && !inS:
					rep.MissingAtBoth++
				}
			}
		}

		// PLI & NACK relative to first socket gap
		for _, fb := range fbEvents {
			before := firstSockGapNs == 0 || fb.absNs < firstSockGapNs
			if fb.kind == "pli" {
				rep.PLIsTotal++
				if before {
					rep.PLIsBeforeFirstGap++
				} else {
					rep.PLIsAfterFirstGap++
				}
				if rep.FirstPLIRelMs == nil {
					rep.FirstPLIRelMs = relMs(fb.absNs)
				}
			} else if fb.kind == "nack" {
				rep.NACKsTotal++
				rep.NACKSeqsTotal += uint64(fb.seqs)
				if before {
					rep.NACKsBeforeFirstGap++
				} else {
					rep.NACKsAfterFirstGap++
				}
				if rep.FirstNACKRelMs == nil {
					rep.FirstNACKRelMs = relMs(fb.absNs)
				}
			}
		}

		if overallFirstGapNs > 0 && capFirstPktNs > 0 && capFirstPktNs > overallFirstGapNs {
			rep.RolloverMissedFirstGap = true
			anyRolloverRisk = true
			if firstSockGapNs == 0 {
				firstSockGapNs = overallFirstGapNs
			}
		}

		rep.AdapterFirstRelMs = relMs(minAdaptNs)
		rep.AdapterLastRelMs = relMs(maxAdaptNs)
		rep.SocketFirstRelMs = relMs(minSockNs)
		rep.SocketLastRelMs = relMs(maxSockNs)
		rep.FirstSocketGapRelMs = relMs(firstSockGapNs)
		rep.FirstAdapterGapRelMs = relMs(firstAdaptGapNs)
		if len(transitMs) > 0 {
			sort.Float64s(transitMs)
			med := math.Round(transitMs[len(transitMs)/2]*100) / 100
			mx := math.Round(transitMs[len(transitMs)-1]*100) / 100
			if med == 0 {
				med = 0
			}
			if mx == 0 {
				mx = 0
			}
			rep.MedianStackTransitMs = &med
			rep.MaxStackTransitMs = &mx
		}
		for k := range encapSet {
			rep.Encapsulations = append(rep.Encapsulations, k)
		}
		sort.Strings(rep.Encapsulations)
		for cid := range camCompSet {
			rep.ComponentIDs = append(rep.ComponentIDs, cid)
		}
		sort.Ints(rep.ComponentIDs)

		sumMissingAtBoth += rep.MissingAtBoth
		sumAdapterLostBeforeSocket += rep.AtAdapterLostBeforeSoc
		sumSocketMissedByAdapter += rep.AtSocketMissedByAdapt
		sumSocketMissing += maxUint64(rep.MissingAtBoth+rep.AtAdapterLostBeforeSoc, overallSockMissingAfterFilter)
		reports = append(reports, rep)
	}

	var allComps []int
	for cid := range compSet {
		allComps = append(allComps, cid)
	}
	sort.Ints(allComps)

	verdict, note := classifyCaptureBoundary(
		sumSocketMissing, sumMissingAtBoth, sumAdapterLostBeforeSocket, sumSocketMissedByAdapter,
		drops, anyRolloverRisk, allFilterOK, len(reports), opts.PCAPNG.Packets,
	)
	return captureCorrelateReport{
		OriginRelUTC:           time.Unix(0, originNs).UTC().Format(time.RFC3339Nano),
		CaptureStartRelMs:      relMs(capStartNs),
		CaptureEndRelMs:        relMs(capEndNs),
		MinPktSizeBytes:        opts.MinPktSizeBytes,
		CaptureDrops:           drops,
		AdapterComponentIDs:    allComps,
		TotalAdapterPacketsRaw: uint64(len(opts.PCAPNG.Packets)),
		MatchedMediaPackets:    matchedMedia,
		UnmatchedCaptureUDP:    unmatchedCaptureUDP,
		FilterCoverageComplete: allFilterOK,
		RolloverRisk:           anyRolloverRisk,
		FirstLossBoundary:      verdict,
		BoundaryNote:           note,
		Cameras:                reports,
	}, nil
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func classifyCaptureBoundary(
	socketMissing, missingAtBoth, atAdapterLostBeforeSoc, missedByAdapter uint64,
	drops captureDropStats, rolloverRisk, filterOK bool, camCount int, rawPkts []capturePacketObs,
) (string, string) {
	if camCount == 0 || socketMissing == 0 {
		return "inconclusive_no_reproduced_loss", "Zero missing RTP sequences were observed at the socket within the captured window."
	}
	if drops.TruncatedHeaders > 0 {
		return "inconclusive_truncated_capture", fmt.Sprintf(
			"%d UDP frames truncated before completing TURN/RTP headers.", drops.TruncatedHeaders)
	}
	if !filterOK || rolloverRisk || len(rawPkts) == 0 {
		return "inconclusive_capture_coverage", fmt.Sprintf(
			"Capture coverage incomplete (filterOK=%t rollover=%t rawPkts=%d).",
			filterOK, rolloverRisk, len(rawPkts))
	}
	if atAdapterLostBeforeSoc > 0 {
		return "lost_between_adapter_and_socket", fmt.Sprintf(
			"%d RTP sequence(s) arrived at the PktMon adapter tap but were lost before socket read (drops=%t comp=%v).",
			atAdapterLostBeforeSoc, drops.hasAnyDrop(), drops.DropComponents)
	}
	if drops.hasAnyDrop() {
		return "inconclusive_capture_drops", fmt.Sprintf(
			"Capture recorded drops while %d sequence(s) were absent at both adapter and socket (missedByAdapter=%d).",
			missingAtBoth, missedByAdapter)
	}
	if missedByAdapter > 0 {
		return "inconclusive_capture_coverage", fmt.Sprintf(
			"Capture missed %d sequence(s) observed at socket with zero recorded capture drops.", missedByAdapter)
	}
	return "absent_at_adapter_capture_point", fmt.Sprintf(
		"All %d missing RTP sequence(s) were already absent at the PktMon adapter tap (0 capture drops, 0 header truncations, 0 fragmentation). Camera TX, Wi-Fi, relay, and earlier adapter/driver path before PktMon remain candidates.",
		missingAtBoth)
}

var (
	correlatePCAPNGPath     string
	correlateDropPCAPNGPath string
	correlateCountersJSON   string
	correlateETLStatsPath   string
	correlateFilterListPath string
	correlateCompListPath   string
	correlateCameras        []string
	correlateAdapterIP      string
	correlateCaptureStart   string
	correlateFirstWake      string
)

var correlateCaptureCmd = &cobra.Command{
	Use:   "correlate-capture",
	Short: "Correlate Windows Packet Monitor PCAPNG capture against private socket RX traces",
	RunE: func(cmd *cobra.Command, args []string) error {
		compMap := map[string]int{}
		if correlateCompListPath != "" {
			if raw, err := os.ReadFile(correlateCompListPath); err == nil {
				var rows []map[string]any
				if json.Unmarshal(raw, &rows) == nil {
					for _, r := range rows {
						if name, ok := r["Name"].(string); ok {
							if id, ok := r["Id"].(float64); ok {
								compMap[name] = int(id)
							}
						}
					}
				}
			}
		}
		pcap, err := parsePCAPNGFile(correlatePCAPNGPath, compMap)
		if err != nil {
			return fmt.Errorf("parse pcapng: %w", err)
		}
		var dropPcap *parsedPCAPNG
		if correlateDropPCAPNGPath != "" {
			dropPcap, err = parsePCAPNGFile(correlateDropPCAPNGPath, compMap)
			if err != nil {
				return fmt.Errorf("parse drop pcapng: %w", err)
			}
		}
		var countersRaw []byte
		if correlateCountersJSON != "" {
			countersRaw, _ = os.ReadFile(correlateCountersJSON)
		}
		var etlStats string
		if correlateETLStatsPath != "" {
			if b, err := os.ReadFile(correlateETLStatsPath); err == nil {
				etlStats = string(b)
			}
		}
		var filterText string
		if correlateFilterListPath != "" {
			if b, err := os.ReadFile(correlateFilterListPath); err == nil {
				filterText = string(b)
			}
		}
		var camSpecs []cameraInputSpec
		for _, entry := range correlateCameras {
			eq := strings.IndexByte(entry, '=')
			if eq <= 0 {
				return fmt.Errorf("invalid --camera %q (expected alias=endpoints.json,flows.csv)", entry)
			}
			alias := entry[:eq]
			rest := strings.SplitN(entry[eq+1:], ",", 2)
			if len(rest) != 2 {
				return fmt.Errorf("invalid --camera %q (expected alias=endpoints.json,flows.csv)", entry)
			}
			camSpecs = append(camSpecs, cameraInputSpec{Alias: alias, EndpointsPath: rest[0], FlowsPath: rest[1]})
		}
		parseOptUTC := func(s string) int64 {
			if s == "" {
				return 0
			}
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return 0
			}
			return t.UnixNano()
		}
		report, err := runCaptureCorrelation(correlateOptions{
			PCAPNG:         pcap,
			DropPCAPNG:     dropPcap,
			FiltersText:    filterText,
			CountersJSON:   countersRaw,
			ETLStatsText:   etlStats,
			Cameras:        camSpecs,
			AdapterIP:      normalizeIP(net.ParseIP(correlateAdapterIP)),
			CaptureStartNs: parseOptUTC(correlateCaptureStart),
			FirstWakeNs:    parseOptUTC(correlateFirstWake),
		})
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	},
}

func init() {
	correlateCaptureCmd.Flags().StringVar(&correlatePCAPNGPath, "pcapng", "", "path to pktmon etl2pcap pcapng file")
	correlateCaptureCmd.Flags().StringVar(&correlateDropPCAPNGPath, "drop-pcapng", "", "optional path to pktmon etl2pcap --drop-only pcapng file")
	correlateCaptureCmd.Flags().StringVar(&correlateCountersJSON, "counters-json", "", "optional path to pktmon counters --json output")
	correlateCaptureCmd.Flags().StringVar(&correlateETLStatsPath, "etl-stats", "", "optional path to pktmon etl2txt --stats output")
	correlateCaptureCmd.Flags().StringVar(&correlateFilterListPath, "filter-list", "", "optional path to pktmon filter list output")
	correlateCaptureCmd.Flags().StringVar(&correlateCompListPath, "comp-list", "", "optional path to pktmon list --json output")
	correlateCaptureCmd.Flags().StringArrayVar(&correlateCameras, "camera", nil, "camera input alias=endpoints.json,flows.csv")
	correlateCaptureCmd.Flags().StringVar(&correlateAdapterIP, "adapter-ip", "", "optional resolved local adapter IP for wildcard flow resolution")
	correlateCaptureCmd.Flags().StringVar(&correlateCaptureStart, "capture-start-utc", "", "optional RFC3339Nano timestamp when filtered pktmon capture started")
	correlateCaptureCmd.Flags().StringVar(&correlateFirstWake, "first-wake-utc", "", "optional RFC3339Nano origin timestamp of first camera wake")
	_ = correlateCaptureCmd.MarkFlagRequired("pcapng")
}
