package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type mockLiveChannel struct {
	mu     sync.Mutex
	state  webrtc.DataChannelState
	events []string
	sent   []map[string]any
	sendFn func(string) error
	onSend func(map[string]any)
}

func (m *mockLiveChannel) ReadyState() webrtc.DataChannelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *mockLiveChannel) SendText(s string) error {
	m.mu.Lock()
	if m.sendFn != nil {
		fn := m.sendFn
		m.mu.Unlock()
		return fn(s)
	}
	if m.state != webrtc.DataChannelStateOpen {
		m.mu.Unlock()
		return fmt.Errorf("channel not open: %s", m.state)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(s), &parsed)
	action, _ := parsed["action"].(string)
	m.events = append(m.events, "send:"+action)
	m.sent = append(m.sent, parsed)
	cb := m.onSend
	m.mu.Unlock()
	if cb != nil {
		cb(parsed)
	}
	return nil
}

func (m *mockLiveChannel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = webrtc.DataChannelStateClosed
	m.events = append(m.events, "dc_close")
	return nil
}

func (m *mockLiveChannel) setState(s webrtc.DataChannelState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s
}

func TestSessionCleanupStopLiveOrderAndWireFormat(t *testing.T) {
	prof, err := parseStreamProfile("2k")
	if err != nil {
		t.Fatal(err)
	}
	var logBuf strings.Builder
	health := log.New(&logBuf, "", 0)
	ctrl := newLiveSessionControl("SERIAL123", prof, time.Now(), health)
	ctrl.SetStopTimeout(3 * time.Second) // proves ack wakes Shutdown early without sleeping 3s

	ch := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ch.onSend = func(msg map[string]any) {
		if msg["action"] == "stopLive" {
			go func() {
				ctrl.OnControlMessage([]byte(`{"action":"stopLive","returnValue":0}`))
			}()
		}
	}
	ctrl.AttachChannel(ch)

	if !ctrl.SendStartLive() {
		t.Fatal("expected SendStartLive to succeed")
	}

	var closeSeqMu sync.Mutex
	var resourcesClosed bool
	startShutdown := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctrl.Shutdown(nil, func() {
				closeSeqMu.Lock()
				resourcesClosed = true
				ch.mu.Lock()
				ch.events = append(ch.events, "resources_close")
				ch.mu.Unlock()
				closeSeqMu.Unlock()
			})
		}()
	}
	wg.Wait()
	elapsed := time.Since(startShutdown)

	if elapsed >= 500*time.Millisecond {
		t.Fatalf("expected early wakeup on stopLive ack, took %v", elapsed)
	}
	if !resourcesClosed || !ctrl.StopLiveSent() {
		t.Fatalf("expected StopLiveSent=true and resourcesClosed=true, got stop=%t closed=%t", ctrl.StopLiveSent(), resourcesClosed)
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()
	wantOrder := []string{"send:startLive", "send:stopLive", "dc_close", "resources_close"}
	if len(ch.events) != len(wantOrder) {
		t.Fatalf("event order mismatch: got %v, want %v", ch.events, wantOrder)
	}
	for i, w := range wantOrder {
		if ch.events[i] != w {
			t.Fatalf("event[%d] = %q, want %q (all=%v)", i, ch.events[i], w, ch.events)
		}
	}

	stopMsg := ch.sent[1]
	startMsg := ch.sent[0]
	if stopMsg["action"] != "stopLive" {
		t.Fatalf("unexpected stopLive action: %v", stopMsg["action"])
	}
	if stopMsg["connectionID"] != startMsg["connectionID"] {
		t.Fatalf("connectionID mismatch between startLive (%v) and stopLive (%v)", startMsg["connectionID"], stopMsg["connectionID"])
	}
	if _, hasTarget := stopMsg["targetId"]; hasTarget {
		t.Fatalf("expected standalone CG6 stopLive to omit targetId (matches PlayControl.stopLive !o00Oo00), got %v", stopMsg)
	}
	if _, hasParams := stopMsg["parameters"]; hasParams {
		t.Fatalf("unexpected parameters wrapper in stopLive: %v", stopMsg)
	}
	if stopMsg["requestID"] == "" || stopMsg["timeStamp"] == nil {
		t.Fatalf("missing requestID or timeStamp in stopLive: %v", stopMsg)
	}
}

func TestSessionCleanupSkipsStopLiveBeforeStartAndCancelsDelayedStart(t *testing.T) {
	prof, _ := parseStreamProfile("2k")

	// Case 1: Shutdown before startLive was sent must not send stopLive, and late SendStartLive is suppressed.
	ctrl1 := newLiveSessionControl("SERIAL1", prof, time.Now(), nil)
	ch1 := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ctrl1.AttachChannel(ch1)
	ctrl1.Shutdown(nil, func() {})
	if ctrl1.SendStartLive() {
		t.Fatal("expected late SendStartLive after Shutdown to be rejected")
	}
	if ctrl1.StopLiveSent() || ctrl1.StartLiveSent() || len(ch1.sent) != 0 {
		t.Fatalf("expected zero messages when torn down before startLive, sent=%v", ch1.sent)
	}

	// Case 2: Hold goroutine blocked on holdCtx must exit when Shutdown cancels holdCtx, never sending startLive after close.
	for iter := 0; iter < 50; iter++ {
		ctrl := newLiveSessionControl("SERIAL2", prof, time.Now(), nil)
		ctrl.SetStopTimeout(5 * time.Millisecond)
		ch := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
		ctrl.AttachChannel(ch)
		holdCtx, holdCancel := context.WithCancel(context.Background())

		var workerWg sync.WaitGroup
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			if iter%2 == 0 {
				<-holdCtx.Done()
				return
			}
			// Concurrent race: try sending right as Shutdown starts.
			ctrl.SendStartLive()
		}()

		ctrl.Shutdown(holdCancel, func() {
			workerWg.Wait()
		})

		ch.mu.Lock()
		if len(ch.sent) > 2 {
			t.Fatalf("iter %d: unexpected message count %v", iter, ch.events)
		}
		// Valid outcomes: [] or [send:startLive, send:stopLive]. Never [send:startLive] alone, never [send:stopLive] alone.
		if len(ch.sent) == 1 {
			t.Fatalf("iter %d: unpaired command %v (events=%v)", iter, ch.sent[0]["action"], ch.events)
		}
		if len(ch.sent) == 2 && (ch.sent[0]["action"] != "startLive" || ch.sent[1]["action"] != "stopLive") {
			t.Fatalf("iter %d: invalid command order %v", iter, ch.events)
		}
		// Guarantee no send occurred after dc_close.
		closedSeen := false
		for _, ev := range ch.events {
			if ev == "dc_close" {
				closedSeen = true
			} else if closedSeen && strings.HasPrefix(ev, "send:") {
				t.Fatalf("iter %d: send after dc_close in %v", iter, ch.events)
			}
		}
		ch.mu.Unlock()
	}
}

func TestSessionCleanupHandlesClosedOrFailedChannelsWithoutBlocking(t *testing.T) {
	prof, _ := parseStreamProfile("2k")

	// Already-closed DataChannel
	ctrlClosed := newLiveSessionControl("SN_CLOSED", prof, time.Now(), nil)
	ctrlClosed.SetStopTimeout(2 * time.Second)
	chClosed := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ctrlClosed.AttachChannel(chClosed)
	ctrlClosed.SendStartLive()
	chClosed.setState(webrtc.DataChannelStateClosed)
	ctrlClosed.NotifyChannelClosed()
	t0 := time.Now()
	ctrlClosed.Shutdown(nil, func() {})
	if time.Since(t0) > 100*time.Millisecond || ctrlClosed.StopLiveSent() {
		t.Fatalf("closed channel cleanup blocked (%v) or sent stopLive=%t", time.Since(t0), ctrlClosed.StopLiveSent())
	}

	// Peer already failed
	ctrlPeer := newLiveSessionControl("SN_PEER", prof, time.Now(), nil)
	ctrlPeer.SetStopTimeout(2 * time.Second)
	chPeer := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ctrlPeer.AttachChannel(chPeer)
	ctrlPeer.SendStartLive()
	ctrlPeer.NotifyPeerClosed()
	t1 := time.Now()
	ctrlPeer.Shutdown(nil, func() {})
	if time.Since(t1) > 100*time.Millisecond || ctrlPeer.StopLiveSent() {
		t.Fatalf("failed peer cleanup blocked (%v) or sent stopLive=%t", time.Since(t1), ctrlPeer.StopLiveSent())
	}

	// SendText failure
	ctrlErr := newLiveSessionControl("SN_ERR", prof, time.Now(), nil)
	ctrlErr.SetStopTimeout(2 * time.Second)
	chErr := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ctrlErr.AttachChannel(chErr)
	ctrlErr.SendStartLive()
	chErr.mu.Lock()
	chErr.sendFn = func(string) error { return errors.New("sctp closed") }
	chErr.mu.Unlock()
	t2 := time.Now()
	ctrlErr.Shutdown(nil, func() {})
	if time.Since(t2) > 100*time.Millisecond || ctrlErr.StopLiveSent() {
		t.Fatalf("send error cleanup blocked (%v) or claimed StopLiveSent=%t", time.Since(t2), ctrlErr.StopLiveSent())
	}

	// Open channel with silent peer must stay bounded by stopTimeout.
	ctrlTimeout := newLiveSessionControl("SN_SILENT", prof, time.Now(), nil)
	ctrlTimeout.SetStopTimeout(35 * time.Millisecond)
	chTimeout := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	ctrlTimeout.AttachChannel(chTimeout)
	ctrlTimeout.SendStartLive()
	t3 := time.Now()
	ctrlTimeout.Shutdown(nil, func() {})
	if d := time.Since(t3); d < 25*time.Millisecond || d > 400*time.Millisecond || !ctrlTimeout.StopLiveSent() {
		t.Fatalf("unexpected silent timeout cleanup duration=%v stopSent=%t", d, ctrlTimeout.StopLiveSent())
	}
}

func TestSessionCleanupIsolatesConcurrentCameras(t *testing.T) {
	prof, _ := parseStreamProfile("2k")
	camA := newLiveSessionControl("CAM_A", prof, time.Now(), nil)
	camB := newLiveSessionControl("CAM_B", prof, time.Now(), nil)
	camA.SetStopTimeout(20 * time.Millisecond)
	camB.SetStopTimeout(20 * time.Millisecond)

	chA := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	chB := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
	camA.AttachChannel(chA)
	camB.AttachChannel(chB)

	if !camA.SendStartLive() || !camB.SendStartLive() {
		t.Fatal("failed to start both cameras")
	}
	if camA.ConnectionID() == camB.ConnectionID() {
		t.Fatalf("expected distinct connection IDs, both got %s", camA.ConnectionID())
	}

	camA.Shutdown(nil, func() {})

	if !camA.StopLiveSent() {
		t.Fatal("expected Camera A to send stopLive")
	}
	if camB.StopLiveSent() || !camB.StartLiveSent() || chB.ReadyState() != webrtc.DataChannelStateOpen {
		t.Fatalf("Camera A teardown impacted Camera B: start=%t stop=%t state=%v", camB.StartLiveSent(), camB.StopLiveSent(), chB.ReadyState())
	}
	camB.Shutdown(nil, func() {})
}

func TestSessionCleanupRealPionWebRTCDeliversStopLiveBeforeClose(t *testing.T) {
	// End-to-end Pion SCTP/DTLS/ICE check proving stopLive arrives over the wire
	// before PeerConnection/DataChannel close completes.
	prof, _ := parseStreamProfile("2k")

	viewerPeer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer viewerPeer.Close()

	cameraPeer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer cameraPeer.Close()

	ctrl := newLiveSessionControl("LOOPBACK_CAM", prof, time.Now(), nil)
	dc, err := viewerPeer.CreateDataChannel("LOOPBACK_CAM", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.AttachChannel(dc)
	dc.OnClose(func() {
		ctrl.NotifyChannelClosed()
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		ctrl.OnControlMessage(msg.Data)
	})

	startReceived := make(chan string, 1)
	stopBeforeClose := make(chan bool, 1)
	cameraPeer.OnDataChannel(func(camDC *webrtc.DataChannel) {
		var closed atomic.Bool
		camDC.OnClose(func() {
			closed.Store(true)
		})
		camDC.OnMessage(func(msg webrtc.DataChannelMessage) {
			var cmd map[string]any
			if json.Unmarshal(msg.Data, &cmd) != nil {
				return
			}
			switch cmd["action"] {
			case "startLive":
				_ = camDC.SendText(`{"action":"startLive","returnValue":0}`)
				select {
				case startReceived <- string(msg.Data):
				default:
				}
			case "stopLive":
				wasOpen := !closed.Load()
				_ = camDC.SendText(`{"action":"stopLive","returnValue":0}`)
				select {
				case stopBeforeClose <- wasOpen:
				default:
				}
			}
		})
	})

	viewerOpen := make(chan struct{})
	dc.OnOpen(func() {
		if ctrl.SendStartLive() {
			close(viewerOpen)
		}
	})

	offer, err := viewerPeer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherViewer := webrtc.GatheringCompletePromise(viewerPeer)
	if err := viewerPeer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherViewer
	if err := cameraPeer.SetRemoteDescription(*viewerPeer.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := cameraPeer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherCam := webrtc.GatheringCompletePromise(cameraPeer)
	if err := cameraPeer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gatherCam
	if err := viewerPeer.SetRemoteDescription(*cameraPeer.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-viewerOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for viewer DataChannel open")
	}
	select {
	case <-startReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for camera startLive")
	}

	var peerClosed bool
	ctrl.Shutdown(nil, func() {
		_ = viewerPeer.Close()
		peerClosed = true
	})
	if !peerClosed {
		t.Fatal("expected Shutdown to close viewerPeer")
	}
	select {
	case wasOpen := <-stopBeforeClose:
		if !wasOpen {
			t.Fatal("camera received stopLive after DataChannel closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("camera never received stopLive before PeerConnection closed")
	}
}

func TestRunWithStartupLockWaitsForFullCleanupBeforeNextAttempt(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "start.lock")
	prof, _ := parseStreamProfile("2k")

	cleanupDone := make(chan struct{})
	attempt2Acquired := make(chan time.Time, 1)

	errA := runWithStartupLock(context.Background(), lockPath, 0, nil, func(release func(string)) error {
		ctrl := newLiveSessionControl("CAM_RETRY", prof, time.Now(), nil)
		ctrl.SetStopTimeout(120 * time.Millisecond)
		ch := &mockLiveChannel{state: webrtc.DataChannelStateOpen}
		ctrl.AttachChannel(ch)
		ctrl.SendStartLive()

		// Spawn Attempt 2 waiter right while Attempt 1 is still active (before first_frame).
		go func() {
			_ = runWithStartupLock(context.Background(), lockPath, 0, nil, func(rel2 func(string)) error {
				attempt2Acquired <- time.Now()
				rel2("attempt2_exit")
				return nil
			})
		}()
		time.Sleep(40 * time.Millisecond)

		// Attempt 1 fails before first_frame and runs orderly Shutdown before returning.
		ctrl.Shutdown(nil, func() {
			close(cleanupDone)
		})
		return errors.New("simulated failure before first_frame")
	})
	if errA == nil {
		t.Fatal("expected simulated error")
	}
	select {
	case <-cleanupDone:
	default:
		t.Fatal("runWithStartupLock returned before cleanup finished")
	}
	select {
	case tAcq := <-attempt2Acquired:
		_ = tAcq
	case <-time.After(2 * time.Second):
		t.Fatal("attempt 2 never acquired startup lock after attempt 1 cleaned up")
	}
}

func TestStartupLockProbeMediaSignalsCloseCleanlyOnCancel(t *testing.T) {
	// Ensures probeMedia cleanly exits and returns (releasing startup lock) when ctx cancels early during signal wait.
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ticket := webRTCTicketResponse{}
	ticket.Data.SignalServer = "ws" + strings.TrimPrefix(server.URL, "http")
	ticket.Data.WebsocketPath = "/ws"
	ticket.Data.AccessToken = "tok"
	ticket.Data.ID = "id1"
	ticket.Data.GroupID = "grp"
	ticket.Data.Role = "viewer"

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, _ = probeMedia(ctx, ctx, started, ticket, "SN_CANCEL", nil, nil)
	if d := time.Since(started); d > 1500*time.Millisecond {
		t.Fatalf("probeMedia did not exit promptly after ctx cancel: %v", d)
	}
}
