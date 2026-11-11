package devices

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestMissingSequenceClearsOnlyOnExactArrival(t *testing.T) {
	var d rtpDiagTracker
	feed := func(seq uint16) {
		d.onInputPacket(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: 1000}})
	}
	feed(100)
	feed(104) // skips 101, 102, 103
	if got := d.seqMissing.Load(); got != 3 {
		t.Fatalf("missing = %d, want 3", got)
	}
	feed(50) // unrelated old packet must not clear a gap
	if got := d.seqMissing.Load(); got != 3 {
		t.Fatalf("unrelated old packet changed missing to %d", got)
	}
	feed(104) // duplicate must not clear a gap
	if got := d.seqMissing.Load(); got != 3 {
		t.Fatalf("duplicate changed missing to %d", got)
	}
	feed(102)
	if got := d.seqMissing.Load(); got != 2 {
		t.Fatalf("exact arrival: missing = %d, want 2", got)
	}
	feed(102) // second copy of the same late packet
	if got := d.seqMissing.Load(); got != 2 {
		t.Fatalf("repeat late arrival changed missing to %d", got)
	}
	if d.seqUnknown.Load() != 1 || d.seqDup.Load() != 2 {
		t.Fatalf("unknown=%d dup=%d, want 1 and 2", d.seqUnknown.Load(), d.seqDup.Load())
	}
}

func TestRxTraceSeesRelayedChannelData(t *testing.T) {
	rx := newRxTrace("")
	rtpPkt := make([]byte, 20)
	rtpPkt[0], rtpPkt[1], rtpPkt[3], rtpPkt[11] = 0x80, 121, 5, 9
	framed := append([]byte{0x40, 0x01, 0x00, byte(len(rtpPkt))}, rtpPkt...)
	rx.observe(framed)
	if rx.rtpPackets.Load() != 1 || rx.streams[0] == nil || rx.streams[0].packets != 1 {
		t.Fatal("RTP carried in TURN ChannelData was not traced")
	}
}

func TestRxTraceSeparatesSocketGapFromLoopLag(t *testing.T) {
	rx := newRxTrace("")
	pkt := func(seq uint16) []byte {
		b := make([]byte, 20)
		b[0], b[1] = 0x80, 121
		b[2], b[3] = byte(seq>>8), byte(seq)
		b[11] = 7
		return b
	}
	rx.observe(pkt(10))
	rx.observe(pkt(13)) // 11 and 12 never reach the socket
	rx.observe(pkt(11)) // exact late arrival
	rx.observe([]byte{0x00, 0x01, 0, 0})
	rx.observe([]byte{22, 0xfe, 0xfd})
	if s := rx.streams[0]; s.outstand != 1 || s.skipped != 2 || s.late != 1 {
		t.Fatalf("outstanding=%d skipped=%d late=%d", s.outstand, s.skipped, s.late)
	}
	if rx.stunPackets.Load() != 1 || rx.dtlsPackets.Load() != 1 {
		t.Fatal("STUN/DTLS were not classified separately from RTP")
	}
	time.Sleep(5 * time.Millisecond)
	if lag, ok := rx.lag(7, 13, time.Now()); !ok || lag < 4*time.Millisecond {
		t.Fatalf("lag = %v ok=%v", lag, ok)
	}
	if _, ok := rx.lag(7, 12, time.Now()); ok {
		t.Fatal("socket never saw sequence 12")
	}
}

func TestRxTraceTracksUDPErrorsAndRedactsAddresses(t *testing.T) {
	rx := newRxTrace("")

	opErr := &net.OpError{
		Op:     "read",
		Net:    "udp",
		Source: &net.UDPAddr{IP: net.ParseIP("192.168.1.99"), Port: 54321},
		Addr:   &net.UDPAddr{IP: net.ParseIP("10.20.30.40"), Port: 3478},
		Err:    fmt.Errorf("recv from 10.20.30.40:3478: %w", syscall.ECONNRESET),
	}
	rx.observeErr(opErr)
	if rx.readErrs.Load() != 1 {
		t.Fatalf("readErrs = %d, want 1", rx.readErrs.Load())
	}
	if got := rx.lastErr(); got == "-" || strings.Contains(got, "10.20.30.40") || strings.Contains(got, "192.168.1.99") || strings.Contains(got, " ") {
		t.Fatalf("unsafe or missing rx_last_err: %q", got)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &tracedPacketConn{PacketConn: pc, rx: rx}
	_ = wrapped.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
	buf := make([]byte, 64)
	if _, _, readErr := wrapped.ReadFrom(buf); readErr == nil {
		t.Fatal("expected read deadline timeout")
	}
	_ = wrapped.Close()
	if _, _, readErr := wrapped.ReadFrom(buf); readErr == nil {
		t.Fatal("expected closed socket error")
	}

	if rx.readTimeouts.Load() < 1 {
		t.Fatalf("readTimeouts = %d, want >= 1", rx.readTimeouts.Load())
	}
	if rx.readClosed.Load() < 1 {
		t.Fatalf("readClosed = %d, want >= 1", rx.readClosed.Load())
	}
	if rx.readErrs.Load() != 1 {
		t.Fatalf("timeout or close inflated readErrs to %d", rx.readErrs.Load())
	}
	fields := rx.fields()
	if !strings.Contains(fields, "rx_errs=1 rx_timeouts=") || !strings.Contains(fields, "rx_last_err=read:") {
		t.Fatalf("unexpected rx fields: %s", fields)
	}
}

func TestCountersOnlyProbeDrainsRTPWithSignalingAndFeedback(t *testing.T) {
	origCountersOnly := mediaProbeCountersOnly
	origObserveFor := mediaProbeObserveFor
	origProgressEvery := mediaProbeProgressEvery
	origHealthLog := mediaProbeHealthLog
	origStreamProfile := mediaProbeStreamProfile
	t.Cleanup(func() {
		mediaProbeCountersOnly = origCountersOnly
		mediaProbeObserveFor = origObserveFor
		mediaProbeProgressEvery = origProgressEvery
		mediaProbeHealthLog = origHealthLog
		mediaProbeStreamProfile = origStreamProfile
	})

	t.Setenv("VICO_RX_TRACE", "1")
	mediaProbeCountersOnly = true
	mediaProbeObserveFor = 800 * time.Millisecond
	mediaProbeProgressEvery = 150 * time.Millisecond
	mediaProbeStreamProfile = "2k"
	healthPath := filepath.Join(t.TempDir(), "counters.bridge.log")
	mediaProbeHealthLog = healthPath

	health, closeLog, err := openProbeHealthLog(healthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()

	camME := &webrtc.MediaEngine{}
	if err := camME.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	cameraPeer, err := webrtc.NewAPI(webrtc.WithMediaEngine(camME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer cameraPeer.Close()

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeH264,
		ClockRate: 90000,
	}, "video", "cam")
	if err != nil {
		t.Fatal(err)
	}
	rtpSender, err := cameraPeer.AddTrack(videoTrack)
	if err != nil {
		t.Fatal(err)
	}

	var sawNACK atomic.Bool
	var sawPLI atomic.Bool
	go func() {
		for {
			pkts, _, rtcpErr := rtpSender.ReadRTCP()
			if rtcpErr != nil {
				return
			}
			for _, pkt := range pkts {
				switch pkt.(type) {
				case *rtcp.TransportLayerNack:
					sawNACK.Store(true)
				case *rtcp.PictureLossIndication:
					sawPLI.Store(true)
				}
			}
		}
	}()

	startLiveCh := make(chan string, 1)
	stopLiveBeforeClose := make(chan bool, 1)
	cameraPeer.OnDataChannel(func(camDC *webrtc.DataChannel) {
		var dcClosed atomic.Bool
		camDC.OnClose(func() { dcClosed.Store(true) })
		camDC.OnMessage(func(msg webrtc.DataChannelMessage) {
			var cmd map[string]any
			if json.Unmarshal(msg.Data, &cmd) != nil {
				return
			}
			switch cmd["action"] {
			case "startLive":
				_ = camDC.SendText(`{"action":"startLive","returnValue":0}`)
				select {
				case startLiveCh <- string(msg.Data):
				default:
				}
			case "stopLive":
				wasOpen := !dcClosed.Load()
				_ = camDC.SendText(`{"action":"stopLive","returnValue":0}`)
				select {
				case stopLiveBeforeClose <- wasOpen:
				default:
				}
			}
		})
	})

	var wsMu sync.Mutex
	var wsConn *websocket.Conn
	sendSignal := func(msg any) {
		wsMu.Lock()
		defer wsMu.Unlock()
		if wsConn != nil {
			_ = wsConn.WriteJSON(msg)
		}
	}

	cameraPeer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		payload, _ := json.Marshal(c.ToJSON())
		sendSignal(map[string]any{
			"method":         "TRANSMIT",
			"messageType":    "ICE_CANDIDATE",
			"messagePayload": base64.StdEncoding.EncodeToString(payload),
		})
	})

	upgrader := websocket.Upgrader{}
	const camSerial = "TEST_CAM_SERIAL"
	const viewerID = "viewer-test-id"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, upErr := upgrader.Upgrade(w, r, nil)
		if upErr != nil {
			return
		}
		wsMu.Lock()
		wsConn = c
		wsMu.Unlock()
		defer c.Close()

		for {
			_, raw, readErr := c.ReadMessage()
			if readErr != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			switch msg["method"] {
			case "AUTH":
				sendSignal(map[string]any{"method": "AUTH_RESPONSE", "code": 0})
			case "JOIN_LIVE":
				sendSignal(map[string]any{"method": "JOIN_LIVE_RESPONSE", "code": 0})
				sendSignal(map[string]any{
					"method":            "PEER_IN",
					"recipientClientId": viewerID,
					"senderClientId":    camSerial,
					"clientId":          camSerial,
				})
			case "TRANSMIT":
				msgType, _ := msg["messageType"].(string)
				b64, _ := msg["messagePayload"].(string)
				decoded, decErr := base64.StdEncoding.DecodeString(b64)
				if decErr != nil {
					continue
				}
				switch msgType {
				case "SDP_OFFER":
					var offer webrtc.SessionDescription
					if json.Unmarshal(decoded, &offer) == nil {
						_ = cameraPeer.SetRemoteDescription(offer)
						ans, ansErr := cameraPeer.CreateAnswer(nil)
						if ansErr == nil {
							_ = cameraPeer.SetLocalDescription(ans)
							ansJSON, _ := json.Marshal(ans)
							sendSignal(map[string]any{
								"method":         "TRANSMIT",
								"messageType":    "SDP_ANSWER",
								"messagePayload": base64.StdEncoding.EncodeToString(ansJSON),
							})
						}
					}
				case "ICE_CANDIDATE":
					var cand webrtc.ICECandidateInit
					if json.Unmarshal(decoded, &cand) == nil {
						_ = cameraPeer.AddICECandidate(cand)
					}
				}
			}
		}
	}))
	defer srv.Close()

	// Drive synthetic preview (640x360) then 2K (2304x1296) RTP packets with an explicit sequence gap.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		sps360, _ := hex.DecodeString("67640016ac3b505017fcb080000003008000000f42")
		sps2k, _ := hex.DecodeString("67640032ac3b50120051d080000003008000000f42")
		seq := uint16(100)
		ts := uint32(90000)
		writePkt := func(s uint16, t uint32, marker bool, payload []byte) {
			_ = videoTrack.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    102,
					SequenceNumber: s,
					Timestamp:      t,
					SSRC:           0x11223344,
					Marker:         marker,
				},
				Payload: payload,
			})
		}

		// Wait until startLive arrives, sending pre-start 360p preview while waiting.
		select {
		case rawStart := <-startLiveCh:
			if !strings.Contains(rawStart, "2304x1296") {
				t.Errorf("expected 2304x1296 resolution in startLive, got %s", rawStart)
			}
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for startLive")
			return
		}
		writePkt(seq, ts, true, sps360)
		seq++
		ts += 3000
		writePkt(seq, ts, true, sps2k)
		seq++
		writePkt(seq, ts, false, []byte{0x68, 0x01})
		seq++
		// Intentionally skip 2 sequence numbers cleanly so NACK + PLI feedback fires.
		seq += 2
		for i := 0; i < 60 && ctx.Err() == nil; i++ {
			ts += 3000
			payload := []byte{0x41, 0x9a, byte(i)}
			if i%10 == 0 {
				writePkt(seq, ts, false, sps2k)
				seq++
				payload = []byte{0x65, 0x88, byte(i)}
			}
			writePkt(seq, ts, true, payload)
			seq++
			select {
			case <-ctx.Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ticket := webRTCTicketResponse{}
	ticket.Data.SignalServer = wsURL
	ticket.Data.WebsocketPath = "/"
	ticket.Data.AccessToken = "token-test"
	ticket.Data.ID = viewerID
	ticket.Data.GroupID = "group-test"
	ticket.Data.Sign = "sign-test"

	startup, startupCancel := context.WithTimeout(ctx, 5*time.Second)
	defer startupCancel()

	var lockReleased atomic.Bool
	summary, err := probeMedia(ctx, startup, time.Now(), ticket, camSerial, health, func() {
		lockReleased.Store(true)
	})
	<-streamDone
	closeLog()
	if err != nil {
		t.Fatalf("probeMedia failed: %v", err)
	}
	if !summary.CountersOnly || summary.EndReason != "observation_complete" {
		t.Fatalf("unexpected summary flags/reason: countersOnly=%t reason=%q", summary.CountersOnly, summary.EndReason)
	}
	if !summary.StartLiveSent || !summary.StartLiveAck || summary.VerifiedWidth != 2304 || summary.VerifiedHeight != 1296 {
		t.Fatalf("expected verified 2304x1296 startLive session, got %+v", summary)
	}
	if summary.OutputFrames != 0 || summary.RecordedBytes != 0 || summary.AssemblyDroppedPkts != 0 {
		t.Fatalf("counters-only must skip assembly/output, got frames=%d bytes=%d asmDrops=%d",
			summary.OutputFrames, summary.RecordedBytes, summary.AssemblyDroppedPkts)
	}
	if summary.VideoPackets < 20 || summary.RTPSeqGaps < 1 || summary.RXErrors != 0 || summary.RTPErrors != 0 {
		t.Fatalf("unexpected packet/error stats: pkts=%d gaps=%d rxErrs=%d rtpErrs=%d",
			summary.VideoPackets, summary.RTPSeqGaps, summary.RXErrors, summary.RTPErrors)
	}
	if !lockReleased.Load() {
		t.Fatal("expected onFirstFrame callback to release startup lock in counters-only mode")
	}
	select {
	case ok := <-stopLiveBeforeClose:
		if !ok {
			t.Fatal("expected stopLive to arrive before channel closed")
		}
	case <-time.After(time.Second):
		t.Fatal("expected stopLive command on orderly teardown")
	}
	if !sawNACK.Load() || !sawPLI.Load() {
		t.Fatalf("expected active feedback in counters-only mode: nack=%t pli=%t", sawNACK.Load(), sawPLI.Load())
	}

	rawLog, err := os.ReadFile(healthPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(rawLog)
	for _, needle := range []string{"event=counters_only", "event=stream_verified", "dim=2304x1296", "event=counters_ready", "event=observe_complete", "event=session_end", "event=nack_sent", "nacks=", "plis=", "rx_errs=0", "rx_last_err=-", fmt.Sprintf("seq_gaps=%d", summary.RTPSeqGaps), fmt.Sprintf("rx_gaps=%d", summary.RTPSeqGaps)} {
		if !strings.Contains(logText, needle) {
			t.Fatalf("health log missing %q:\n%s", needle, logText)
		}
	}
	for _, path := range []string{healthPath + ".rx.csv", healthPath + ".rx.csv.flows.csv", healthPath + ".rx.csv.endpoints.json"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected private trace file %s: %v", path, err)
		}
	}

	origOutput := mediaProbeOutput
	mediaProbeOutput = filepath.Join(t.TempDir(), "out.h264")
	mediaProbeCountersOnly = true
	t.Cleanup(func() { mediaProbeOutput = origOutput })
	if err := mediaProbeCmd.RunE(mediaProbeCmd, []string{camSerial}); err == nil || !strings.Contains(err.Error(), "--counters-only") {
		t.Fatalf("expected --counters-only with --output rejection, got %v", err)
	}
	mediaProbeOutput = origOutput
}

func TestRxTraceUnwrapsTURNDataIndicationAndBidirectionalFlows(t *testing.T) {
	rxPath := filepath.Join(t.TempDir(), "trace.rx.csv")
	rx := newRxTrace(rxPath)
	rx.recordICEServers([]string{"203.0.113.10:3478"})

	// Build a TURN STUN Data Indication (0x0017) wrapping an RTP packet (seq 77, ssrc 0x01020304).
	innerRTP := make([]byte, 20)
	innerRTP[0], innerRTP[1] = 0x80, 121|0x80
	innerRTP[2], innerRTP[3] = 0, 77
	innerRTP[8], innerRTP[9], innerRTP[10], innerRTP[11] = 1, 2, 3, 4
	stunInd := make([]byte, 20+8+4+len(innerRTP))
	stunInd[0], stunInd[1] = 0x00, 0x17
	msgLen := len(stunInd) - 20
	stunInd[2], stunInd[3] = byte(msgLen>>8), byte(msgLen)
	stunInd[4], stunInd[5], stunInd[6], stunInd[7] = 0x21, 0x12, 0xa4, 0x42
	// Dummy XOR-PEER-ADDRESS attr (0x0012, len 4 dummy) before DATA attr (0x0013)
	stunInd[20], stunInd[21], stunInd[22], stunInd[23] = 0x00, 0x12, 0x00, 0x04
	stunInd[28], stunInd[29], stunInd[30], stunInd[31] = 0x00, 0x13, 0x00, byte(len(innerRTP))
	copy(stunInd[32:], innerRTP)

	rx.observeWithAddr("rx", "192.168.1.213:51000", "203.0.113.10:3478", stunInd)
	if rx.rtpPackets.Load() != 1 || rx.stunPackets.Load() != 0 {
		t.Fatalf("expected Data Indication unwrapped to RTP (got rtp=%d stun=%d)", rx.rtpPackets.Load(), rx.stunPackets.Load())
	}

	// Outbound SRTCP feedback NACK header (V=2, FMT=1, PT=205) wrapped in ChannelData
	rtcpNack := []byte{0x40, 0x01, 0x00, 0x0c, 0x81, 205, 0x00, 0x02, 1, 2, 3, 4, 5, 6, 7, 8}
	rx.observeWithAddr("tx", "192.168.1.213:51000", "203.0.113.10:3478", rtcpNack)
	rx.observeFeedback("tx_nack", 0x01020304, []uint16{75, 76})
	rx.observeFeedback("tx_pli", 0x01020304, nil)
	fields := rx.fields()
	rx.close()

	for _, want := range []string{"nacks=1", "nack_seqs=2", "plis=1"} {
		if !strings.Contains(fields, want) {
			t.Fatalf("missing %q in fields: %s", want, fields)
		}
	}
	flowsRaw, err := os.ReadFile(rxPath + ".flows.csv")
	if err != nil {
		t.Fatal(err)
	}
	flowText := string(flowsRaw)
	if !strings.Contains(flowText, ",rx,0,rtp,turn_ind,16909060,77,20,1,0,-") ||
		!strings.Contains(flowText, ",tx,1,rtcp,turn_ch,") ||
		!strings.Contains(flowText, ",tx_nack,-1,rtcp,ctrl,16909060,75,0,0,0,75;76") {
		t.Fatalf("unexpected flows.csv contents:\n%s", flowText)
	}
	epRaw, err := os.ReadFile(rxPath + ".endpoints.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(epRaw), `"203.0.113.10"`) || !strings.Contains(string(epRaw), `"local_port": 51000`) {
		t.Fatalf("missing endpoints in endpoints.json:\n%s", epRaw)
	}
}
