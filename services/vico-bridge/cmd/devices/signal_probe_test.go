package devices

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestProbeSignalCompletesAuthJoinAndPeerInWithoutLeakingSecrets(t *testing.T) {
	const (
		accessToken = "secret-access-token"
		serial      = "secret-camera-serial"
		ticketID    = "secret-ticket-id"
		groupID     = "secret-group-id"
	)

	serverError := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			serverError <- err
			return
		}
		defer connection.Close()

		var authMessage map[string]any
		if err := connection.ReadJSON(&authMessage); err != nil {
			serverError <- err
			return
		}
		if authMessage["method"] != "AUTH" || authMessage["accessToken"] != accessToken || authMessage["id"] != ticketID {
			serverError <- &testError{"unexpected AUTH message"}
			return
		}
		if err := connection.WriteJSON(map[string]any{"method": "AUTH_RESPONSE", "code": 0}); err != nil {
			serverError <- err
			return
		}

		var joinMessage map[string]any
		if err := connection.ReadJSON(&joinMessage); err != nil {
			serverError <- err
			return
		}
		if joinMessage["method"] != "JOIN_LIVE" || joinMessage["accessToken"] != accessToken || joinMessage["recipientClientId"] != serial || joinMessage["group"] != groupID {
			serverError <- &testError{"unexpected JOIN_LIVE message"}
			return
		}
		if err := connection.WriteJSON(map[string]any{"method": "JOIN_LIVE_RESPONSE", "code": 0}); err != nil {
			serverError <- err
			return
		}
		if err := connection.WriteJSON(map[string]any{"method": "PEER_IN", "code": 0}); err != nil {
			serverError <- err
			return
		}
		serverError <- nil
	}))
	defer server.Close()

	ticket := webRTCTicketResponse{}
	ticket.Data.SignalServer = "ws" + strings.TrimPrefix(server.URL, "http")
	ticket.Data.WebsocketPath = "/iot-channel/connect"
	ticket.Data.AccessToken = accessToken
	ticket.Data.ID = ticketID
	ticket.Data.GroupID = groupID
	ticket.Data.Role = "viewer"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	summary, err := probeSignal(ctx, ticket, serial)
	if err != nil {
		t.Fatalf("probeSignal returned error: %v", err)
	}
	if err := <-serverError; err != nil {
		t.Fatalf("test server failed: %v", err)
	}
	if !summary.WebSocketConnected || !summary.Authenticated || !summary.JoinAcknowledged || !summary.PeerOnline {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{accessToken, serial, ticketID, groupID} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("summary leaked secret %q", secret)
		}
	}
}

func TestBuildSignalURLUsesTicketPath(t *testing.T) {
	ticket := webRTCTicketResponse{}
	ticket.Data.SignalServer = "wss://signal.example.test"
	ticket.Data.SignalPort = 5349
	ticket.Data.WebsocketPath = "iot-channel/connect"

	got, err := buildSignalURL(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://signal.example.test:5349/iot-channel/connect" {
		t.Fatalf("unexpected signal URL: %s", got)
	}
}

type testError struct{ message string }

func (err *testError) Error() string { return err.message }
