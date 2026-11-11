package devices

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type synthPCAPBuilder struct {
	buf bytes.Buffer
}

func newSynthPCAPBuilder() *synthPCAPBuilder {
	b := &synthPCAPBuilder{}
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0x1A2B3C4D))
	_ = binary.Write(&body, binary.LittleEndian, uint16(1))
	_ = binary.Write(&body, binary.LittleEndian, uint16(0))
	_ = binary.Write(&body, binary.LittleEndian, int64(-1))
	b.writeBlock(0x0A0D0D0A, body.Bytes())
	return b
}

func (b *synthPCAPBuilder) writeBlock(bType uint32, body []byte) {
	padded := (len(body) + 3) &^ 3
	total := uint32(12 + padded)
	_ = binary.Write(&b.buf, binary.LittleEndian, bType)
	_ = binary.Write(&b.buf, binary.LittleEndian, total)
	b.buf.Write(body)
	for i := len(body); i < padded; i++ {
		b.buf.WriteByte(0)
	}
	_ = binary.Write(&b.buf, binary.LittleEndian, total)
}

func writePCAPOpt(buf *bytes.Buffer, code uint16, val []byte) {
	_ = binary.Write(buf, binary.LittleEndian, code)
	_ = binary.Write(buf, binary.LittleEndian, uint16(len(val)))
	buf.Write(val)
	for i := len(val); i < ((len(val) + 3) &^ 3); i++ {
		buf.WriteByte(0)
	}
}

func (b *synthPCAPBuilder) addInterface(linkType uint16, name string, nsRes bool) int {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, linkType)
	_ = binary.Write(&body, binary.LittleEndian, uint16(0))
	_ = binary.Write(&body, binary.LittleEndian, uint32(160))
	if name != "" {
		writePCAPOpt(&body, 2, []byte(name))
	}
	res := byte(6)
	if nsRes {
		res = 9
	}
	writePCAPOpt(&body, 9, []byte{res})
	writePCAPOpt(&body, 0, nil)
	b.writeBlock(0x00000001, body.Bytes())
	return 0
}

func (b *synthPCAPBuilder) addEPB(ifID int, tsNs int64, origLen int, frame []byte, dir uint32, comment string, epbDrops uint64) {
	tsRaw := uint64(tsNs) // assumes if_tsresol=9
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(ifID))
	_ = binary.Write(&body, binary.LittleEndian, uint32(tsRaw>>32))
	_ = binary.Write(&body, binary.LittleEndian, uint32(tsRaw))
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(frame)))
	_ = binary.Write(&body, binary.LittleEndian, uint32(origLen))
	body.Write(frame)
	for i := len(frame); i < ((len(frame) + 3) &^ 3); i++ {
		body.WriteByte(0)
	}
	hasOpts := dir > 0 || comment != "" || epbDrops > 0
	if dir > 0 {
		var db [4]byte
		binary.LittleEndian.PutUint32(db[:], dir)
		writePCAPOpt(&body, 2, db[:])
	}
	if comment != "" {
		writePCAPOpt(&body, 1, []byte(comment))
	}
	if epbDrops > 0 {
		var dr [8]byte
		binary.LittleEndian.PutUint64(dr[:], epbDrops)
		writePCAPOpt(&body, 4, dr[:])
	}
	if hasOpts {
		writePCAPOpt(&body, 0, nil)
	}
	b.writeBlock(0x00000006, body.Bytes())
}

func (b *synthPCAPBuilder) addISB(ifID int, ifDrops, osDrops uint64) {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(ifID))
	_ = binary.Write(&body, binary.LittleEndian, uint32(0))
	_ = binary.Write(&body, binary.LittleEndian, uint32(0))
	var d [8]byte
	if ifDrops > 0 {
		binary.LittleEndian.PutUint64(d[:], ifDrops)
		writePCAPOpt(&body, 5, d[:])
	}
	if osDrops > 0 {
		binary.LittleEndian.PutUint64(d[:], osDrops)
		writePCAPOpt(&body, 7, d[:])
	}
	writePCAPOpt(&body, 0, nil)
	b.writeBlock(0x00000005, body.Bytes())
}

func makeEthIPv4UDP(srcIP, dstIP string, srcPort, dstPort int, udpPayload []byte, totalUDPOverride int) []byte {
	udpLen := 8 + len(udpPayload)
	if totalUDPOverride > 0 {
		udpLen = totalUDPOverride
	}
	ipLen := 20 + udpLen
	frame := make([]byte, 14+20+8+len(udpPayload))
	frame[12], frame[13] = 0x08, 0x00
	ip := frame[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipLen))
	ip[9] = 17
	copy(ip[12:16], net.ParseIP(srcIP).To4())
	copy(ip[16:20], net.ParseIP(dstIP).To4())
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:2], uint16(srcPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(dstPort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], udpPayload)
	return frame
}

func makeSynthRTP(ssrc uint32, seq uint16, marker bool, bodyLen int) []byte {
	p := make([]byte, 12+bodyLen)
	p[0] = 0x80
	p[1] = 121
	if marker {
		p[1] |= 0x80
	}
	binary.BigEndian.PutUint16(p[2:4], seq)
	binary.BigEndian.PutUint32(p[8:12], ssrc)
	return p
}

func wrapChannelData(rtpPkt []byte) []byte {
	out := make([]byte, 4+len(rtpPkt))
	out[0], out[1] = 0x40, 0x01
	binary.BigEndian.PutUint16(out[2:4], uint16(len(rtpPkt)))
	copy(out[4:], rtpPkt)
	return out
}

func wrapDataIndication(rtpPkt []byte) []byte {
	out := make([]byte, 20+4+len(rtpPkt))
	out[0], out[1] = 0x00, 0x17
	msgLen := 4 + len(rtpPkt)
	binary.BigEndian.PutUint16(out[2:4], uint16(msgLen))
	out[4], out[5], out[6], out[7] = 0x21, 0x12, 0xa4, 0x42
	out[20], out[21] = 0x00, 0x13
	binary.BigEndian.PutUint16(out[22:24], uint16(len(rtpPkt)))
	copy(out[24:], rtpPkt)
	return out
}

func writeCameraTraceFixtures(t *testing.T, dir, alias, localIP string, localPort int, remoteIP string, remotePort int, startTime time.Time, events []socketFlowEvent) cameraInputSpec {
	t.Helper()
	epPath := filepath.Join(dir, alias+".endpoints.json")
	flowsPath := filepath.Join(dir, alias+".flows.csv")

	ep := rxEndpointsFile{
		StartUTC:   startTime.UTC().Format(time.RFC3339Nano),
		ICEServers: []string{fmt.Sprintf("%s:%d", remoteIP, remotePort)},
		Flows: []traceFlow{
			{ID: 0, Dir: "rx", LocalIP: localIP, LocalPort: localPort, RemoteIP: remoteIP, RemotePort: remotePort, FirstUs: 1, LastUs: 1000000, Packets: 100, Bytes: 50000},
			{ID: 1, Dir: "tx", LocalIP: localIP, LocalPort: localPort, RemoteIP: remoteIP, RemotePort: remotePort, FirstUs: 1, LastUs: 1000000, Packets: 10, Bytes: 1200},
		},
	}
	rawEP, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(epPath, rawEP, 0o600); err != nil {
		t.Fatal(err)
	}

	var csvBuf bytes.Buffer
	fmt.Fprintf(&csvBuf, "# start_utc=%s fields=t_us,dir,flow_id,kind,encap,ssrc,seq,bytes,marker,fmt,meta\n", startTime.UTC().Format(time.RFC3339Nano))
	for _, ev := range events {
		m := 0
		if ev.Marker {
			m = 1
		}
		meta := ev.Meta
		if meta == "" {
			meta = "-"
		}
		fmt.Fprintf(&csvBuf, "%d,%s,%d,%s,%s,%d,%d,%d,%d,0,%s\n",
			ev.RelUs, ev.Dir, ev.FlowID, ev.Kind, ev.Encap, ev.SSRC, ev.Seq, ev.Bytes, m, meta)
	}
	if err := os.WriteFile(flowsPath, csvBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return cameraInputSpec{Alias: alias, EndpointsPath: epPath, FlowsPath: flowsPath}
}

func TestCorrelatorHandlesEncapsulationsWraparoundDuplicatesReordering(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	baseNs := t0.UnixNano()
	const ssrc = uint32(0x12345678)

	// Sequences wrap around 65532 -> 65535 -> 0 -> 4; includes pre-filter packet 65531, reordering, and duplicate comp observations.
	pb := newSynthPCAPBuilder()
	pb.addInterface(1, "Wi-Fi Component 5", true)
	pb.addInterface(1, "Wi-Fi Component 9", true)

	// Adapter arrivals starting at captureStart (+100ms) across plain SRTP, ChannelData, and STUN Data Indication.
	seqs := []struct {
		seq   uint16
		encap string
		dtMs  int64
		comp  int
	}{
		{65532, "raw", 100, 0},
		{65532, "raw", 101, 1}, // duplicate observation on second component ID
		{65534, "turn_ch", 120, 0},
		{65533, "turn_ch", 125, 0}, // reordered arrival
		{65535, "turn_ind", 140, 0},
		{0, "raw", 160, 0},
		{1, "turn_ch", 180, 0},
		{2, "turn_ind", 200, 0},
		{4, "turn_ind", 220, 0}, // seq 3 missing everywhere
	}
	var sockEvents []socketFlowEvent
	// Pre-filter packet before captureStart (+100ms) excluded while resolving endpoints
	sockEvents = append(sockEvents, socketFlowEvent{RelUs: 20_000, Dir: "rx", FlowID: 0, Kind: "rtp", Encap: "raw", SSRC: ssrc, Seq: 65531, Bytes: 200})

	for _, item := range seqs {
		rtp := makeSynthRTP(ssrc, item.seq, item.seq == 4, 240)
		var wire []byte
		switch item.encap {
		case "raw":
			wire = rtp
		case "turn_ch":
			wire = wrapChannelData(rtp)
		case "turn_ind":
			wire = wrapDataIndication(rtp)
		}
		// Simulate --pkt-size 160 truncating only payload tail beyond 160B Ethernet frame
		frame := makeEthIPv4UDP("203.0.113.10", "192.168.1.213", 3478, 51000, wire, 0)
		origLen := len(frame)
		if len(frame) > 160 {
			frame = frame[:160]
		}
		pb.addEPB(item.comp, baseNs+item.dtMs*1e6, origLen, frame, 1, "", 0)
		if item.comp == 0 {
			sockEvents = append(sockEvents, socketFlowEvent{
				RelUs: item.dtMs*1000 + 1500, Dir: "rx", FlowID: 0, Kind: "rtp", Encap: item.encap, SSRC: ssrc, Seq: item.seq, Bytes: len(rtp),
			})
		}
	}
	// NACK & PLI around the gap (seq 4 arrives at +221.5ms, feedback right after at +230ms & +240ms)
	sockEvents = append(sockEvents,
		socketFlowEvent{RelUs: 230_000, Dir: "tx_nack", FlowID: -1, Kind: "rtcp", Encap: "ctrl", SSRC: ssrc, Seq: 3, Meta: "3"},
		socketFlowEvent{RelUs: 240_000, Dir: "tx_pli", FlowID: -1, Kind: "rtcp", Encap: "ctrl", SSRC: ssrc},
	)

	spec := writeCameraTraceFixtures(t, dir, "dining_area", "0.0.0.0", 51000, "203.0.113.10", 3478, t0, sockEvents)
	pcapParsed, err := parsePCAPNGBytes(pb.buf.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	filterText := "#  Name    Protocol  IP                   Port\n1  uzy-d   UDP       192.168.1.213, 203.0.113.10   51000, 3478\n"

	report, err := runCaptureCorrelation(correlateOptions{
		PCAPNG:         pcapParsed,
		FiltersText:    filterText,
		Cameras:        []cameraInputSpec{spec},
		AdapterIP:      net.ParseIP("192.168.1.213").To4(),
		CaptureStartNs: baseNs + 90*1e6,
		FirstWakeNs:    baseNs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.FirstLossBoundary != "absent_at_adapter_capture_point" || !report.FilterCoverageComplete {
		t.Fatalf("unexpected boundary/coverage: %s filter=%t note=%s", report.FirstLossBoundary, report.FilterCoverageComplete, report.BoundaryNote)
	}
	cam := report.Cameras[0]
	if cam.PreFilterExcludedRTP != 1 || cam.AdapterDupObservations != 1 || cam.AdapterReordered != 1 {
		t.Fatalf("unexpected prefilter/dup/reorder counts: %+v", cam)
	}
	if cam.MissingAtBoth != 1 || cam.AtAdapterLostBeforeSoc != 0 || cam.AtSocketMissedByAdapt != 0 {
		t.Fatalf("unexpected sequence reconciliation: %+v", cam)
	}
	if cam.NACKsAfterFirstGap != 1 || cam.PLIsAfterFirstGap != 1 || len(cam.Encapsulations) != 3 {
		t.Fatalf("expected NACK/PLI after gap and all 3 encapsulations, got %+v", cam)
	}
	rawOut, _ := json.Marshal(report)
	outText := string(rawOut)
	for _, secret := range []string{"192.168.1.213", "203.0.113.10", "51000", "3478"} {
		if strings.Contains(outText, secret) {
			t.Fatalf("correlator output leaked endpoint secret %q:\n%s", secret, outText)
		}
	}
}

func TestCorrelatorIsolatesHostStackDropAfterAdapter(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	baseNs := t0.UnixNano()
	const ssrc = uint32(0x22334455)

	pb := newSynthPCAPBuilder()
	pb.addInterface(1, "Wi-Fi Component 5", true)
	dropPB := newSynthPCAPBuilder()
	dropPB.addInterface(1, "Component 12 NDIS Drop", true)

	var sockEvents []socketFlowEvent
	for seq := uint16(100); seq <= 110; seq++ {
		frame := makeEthIPv4UDP("203.0.113.20", "192.168.1.213", 3478, 52000, wrapChannelData(makeSynthRTP(ssrc, seq, seq == 100, 60)), 0)
		ts := baseNs + int64(seq-99)*20*1e6
		pb.addEPB(0, ts, len(frame), frame, 1, "Component 5", 0)
		if seq == 105 || seq == 106 {
			dropPB.addEPB(0, ts+500_000, len(frame), frame, 1, "Component 12 Drop", 0)
			continue
		}
		sockEvents = append(sockEvents, socketFlowEvent{
			RelUs: int64(seq-99)*20_000 + 2000, Dir: "rx", FlowID: 0, Kind: "rtp", Encap: "turn_ch", SSRC: ssrc, Seq: seq, Bytes: 72,
		})
	}
	spec := writeCameraTraceFixtures(t, dir, "cash_register", "192.168.1.213", 52000, "203.0.113.20", 3478, t0, sockEvents)
	pcapParsed, _ := parsePCAPNGBytes(pb.buf.Bytes(), nil)
	dropParsed, _ := parsePCAPNGBytes(dropPB.buf.Bytes(), nil)
	countersJSON := []byte(`[{"Name":"Component 12","Id":12,"Counters":{"Rx":{"DroppedPackets":2,"DropReason":"InsufficientResources"}}}]`)

	report, err := runCaptureCorrelation(correlateOptions{
		PCAPNG:       pcapParsed,
		DropPCAPNG:   dropParsed,
		CountersJSON: countersJSON,
		Cameras:      []cameraInputSpec{spec},
		FirstWakeNs:  baseNs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.FirstLossBoundary != "lost_between_adapter_and_socket" {
		t.Fatalf("got boundary %q, want lost_between_adapter_and_socket (%s)", report.FirstLossBoundary, report.BoundaryNote)
	}
	if report.Cameras[0].AtAdapterLostBeforeSoc != 2 || report.Cameras[0].DropOnlyMatchedPackets != 2 {
		t.Fatalf("unexpected drop counts: %+v", report.Cameras[0])
	}
}

func TestCorrelatorDetectsTruncationCaptureDropsAndRollover(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)
	baseNs := t0.UnixNano()
	const ssrc = uint32(0x33445566)

	buildCommon := func(truncHdr, captureDrop, rollover bool) (captureCorrelateReport, error) {
		dir := t.TempDir()
		pb := newSynthPCAPBuilder()
		pb.addInterface(1, "Wi-Fi Component 5", true)
		var sockEvents []socketFlowEvent
		for seq := uint16(10); seq <= 20; seq++ {
			ts := baseNs + int64(seq)*20*1e6
			if seq == 15 {
				continue // missing packet at seq 15
			}
			frame := makeEthIPv4UDP("203.0.113.30", "192.168.1.213", 3478, 53000, makeSynthRTP(ssrc, seq, false, 80), 0)
			origLen := len(frame)
			if truncHdr && seq == 16 {
				// Chop inside UDP payload before 12B RTP header completes
				frame = frame[:14+20+8+4]
			}
			dropFlag := uint64(0)
			if captureDrop && seq == 16 {
				dropFlag = 3
			}
			if !rollover || seq >= 17 {
				pb.addEPB(0, ts, origLen, frame, 1, "Component 5", dropFlag)
			}
			sockEvents = append(sockEvents, socketFlowEvent{
				RelUs: int64(seq)*20_000 + 1000, Dir: "rx", FlowID: 0, Kind: "rtp", Encap: "raw", SSRC: ssrc, Seq: seq, Bytes: 92,
			})
		}
		if captureDrop {
			pb.addISB(0, 2, 1)
		}
		spec := writeCameraTraceFixtures(t, dir, "dining_area", "192.168.1.213", 53000, "203.0.113.30", 3478, t0, sockEvents)
		parsed, err := parsePCAPNGBytes(pb.buf.Bytes(), nil)
		if err != nil {
			return captureCorrelateReport{}, err
		}
		return runCaptureCorrelation(correlateOptions{
			PCAPNG:      parsed,
			Cameras:     []cameraInputSpec{spec},
			FirstWakeNs: baseNs,
		})
	}

	if rep, err := buildCommon(true, false, false); err != nil || rep.FirstLossBoundary != "inconclusive_truncated_capture" {
		t.Fatalf("truncation check failed: err=%v boundary=%q note=%s", err, rep.FirstLossBoundary, rep.BoundaryNote)
	}
	if rep, err := buildCommon(false, true, false); err != nil || rep.FirstLossBoundary != "inconclusive_capture_drops" {
		t.Fatalf("capture-drop check failed: err=%v boundary=%q note=%s", err, rep.FirstLossBoundary, rep.BoundaryNote)
	}
	if rep, err := buildCommon(false, false, true); err != nil || !rep.RolloverRisk || rep.FirstLossBoundary != "inconclusive_capture_coverage" {
		t.Fatalf("rollover check failed: err=%v rollover=%t boundary=%q note=%s", err, rep.RolloverRisk, rep.FirstLossBoundary, rep.BoundaryNote)
	}

	// Unmatched filter rule check + CLI invocation check
	dir := t.TempDir()
	pb := newSynthPCAPBuilder()
	pb.addInterface(1, "Wi-Fi Component 5", true)
	var sockEvents []socketFlowEvent
	for seq := uint16(1); seq <= 5; seq++ {
		ts := baseNs + int64(seq)*20*1e6
		frame := makeEthIPv4UDP("203.0.113.40", "192.168.1.213", 3478, 54000, makeSynthRTP(ssrc, seq, false, 80), 0)
		pb.addEPB(0, ts, len(frame), frame, 1, "Component 5", 0)
		if seq == 3 {
			continue
		}
		sockEvents = append(sockEvents, socketFlowEvent{RelUs: int64(seq) * 20_000, Dir: "rx", FlowID: 0, Kind: "rtp", Encap: "raw", SSRC: ssrc, Seq: seq, Bytes: 92})
	}
	spec := writeCameraTraceFixtures(t, dir, "dining_area", "192.168.1.213", 54000, "203.0.113.40", 3478, t0, sockEvents)
	pcapPath := filepath.Join(dir, "capture.pcapng")
	if err := os.WriteFile(pcapPath, pb.buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	filterPath := filepath.Join(dir, "filters.txt")
	// Deliberately filter a DIFFERENT remote port 9999 so active flow fails filter coverage check
	if err := os.WriteFile(filterPath, []byte("1  wrong-filter  UDP  192.168.1.213, 203.0.113.40  54000, 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, _ := parsePCAPNGBytes(pb.buf.Bytes(), nil)
	repFilter, err := runCaptureCorrelation(correlateOptions{
		PCAPNG:      parsed,
		FiltersText: "1  wrong-filter  UDP  192.168.1.213, 203.0.113.40  54000, 9999\n",
		Cameras:     []cameraInputSpec{spec},
		FirstWakeNs: baseNs,
	})
	if err != nil || repFilter.FilterCoverageComplete || repFilter.FirstLossBoundary != "inconclusive_capture_coverage" {
		t.Fatalf("filter mismatch check failed: err=%v complete=%t boundary=%q", err, repFilter.FilterCoverageComplete, repFilter.FirstLossBoundary)
	}

	// Verify CLI correlateCaptureCmd runs cleanly on matching filter
	correlatePCAPNGPath = pcapPath
	correlateFilterListPath = ""
	correlateCameras = []string{fmt.Sprintf("dining_area=%s,%s", spec.EndpointsPath, spec.FlowsPath)}
	t.Cleanup(func() {
		correlatePCAPNGPath = ""
		correlateCameras = nil
	})
	if err := correlateCaptureCmd.RunE(correlateCaptureCmd, nil); err != nil {
		t.Fatalf("correlateCaptureCmd failed: %v", err)
	}
}
