package devices

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarizeWebRTCTicketDoesNotLeakSecrets(t *testing.T) {
	var response webRTCTicketResponse
	response.Data.SignalServer = "wss://signal.example.test:443/private?token=signal-secret"
	response.Data.AccessToken = "access-secret"
	response.Data.Sign = "signature-secret"
	response.Data.ID = "session-secret"
	response.Data.IceServer = append(response.Data.IceServer, iceServer{
		URL:        "turn:turn.example.test:3478?transport=udp&token=turn-secret",
		Credential: "ice-secret",
		Username:   "ice-user-secret",
	})

	summary := summarizeWebRTCTicket(response)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	output := string(encoded)
	for _, secret := range []string{"signal-secret", "access-secret", "signature-secret", "session-secret", "turn-secret", "ice-secret", "ice-user-secret"} {
		if strings.Contains(output, secret) {
			t.Fatalf("summary leaked %q: %s", secret, output)
		}
	}
	if !summary.HasAccessToken || !summary.HasSignature || !summary.HasSessionIdentifiers {
		t.Fatalf("summary should report secret presence without reporting values: %+v", summary)
	}
}

func TestRedactEndpointKeepsOnlyWebSocketOrigin(t *testing.T) {
	got := redactEndpoint("wss://signal.example.test:443/private/path?token=secret")
	if got != "wss://signal.example.test:443" {
		t.Fatalf("unexpected redacted endpoint: %q", got)
	}
}
