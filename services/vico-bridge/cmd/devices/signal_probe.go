package devices

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dydx/vico-cli/pkg/auth"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

type signalProbeSummary struct {
	WebSocketConnected bool           `json:"websocketConnected"`
	Authenticated      bool           `json:"authenticated"`
	AuthCode           int            `json:"authCode,omitempty"`
	JoinAcknowledged   bool           `json:"joinAcknowledged"`
	JoinCode           int            `json:"joinCode,omitempty"`
	PeerOnline         bool           `json:"peerOnline"`
	TimedOut           bool           `json:"timedOut"`
	DurationMillis     int64          `json:"durationMillis"`
	EventCounts        map[string]int `json:"eventCounts,omitempty"`
}

type signalEnvelope struct {
	Method      string `json:"method"`
	MessageType string `json:"messageType"`
	Code        int    `json:"code"`
}

var signalProbeTimeout = 25 * time.Second

var signalProbeCmd = &cobra.Command{
	Use:   "signal-probe SERIAL_NUMBER",
	Short: "Safely test the owned camera's WebSocket signaling handshake",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := auth.Authenticate()
		if err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		ticket, err := requestWebRTCTicket(cmd.Context(), token, args[0])
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), signalProbeTimeout)
		defer cancel()
		summary, err := probeSignal(ctx, ticket, args[0])
		if err != nil {
			return err
		}

		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("format signal summary: %w", err)
		}
		fmt.Println(string(encoded))
		return nil
	},
}

func init() {
	signalProbeCmd.Flags().DurationVar(&signalProbeTimeout, "timeout", 25*time.Second, "maximum time to wait for signaling events")
}

func probeSignal(ctx context.Context, ticket webRTCTicketResponse, serialNumber string) (signalProbeSummary, error) {
	started := time.Now()
	summary := signalProbeSummary{EventCounts: make(map[string]int)}

	signalURL, err := buildSignalURL(ticket)
	if err != nil {
		return summary, err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	connection, response, err := dialer.DialContext(ctx, signalURL, http.Header{})
	if err != nil {
		if response != nil {
			return summary, fmt.Errorf("WebSocket handshake failed with HTTP %d", response.StatusCode)
		}
		return summary, fmt.Errorf("WebSocket handshake failed: %w", err)
	}
	defer connection.Close()
	summary.WebSocketConnected = true

	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetReadDeadline(deadline)
		_ = connection.SetWriteDeadline(deadline)
	}

	if err := connection.WriteJSON(buildAuthMessage(ticket)); err != nil {
		return summary, fmt.Errorf("send signal authentication: %w", err)
	}

	joinSent := false
	for {
		messageType, payload, readErr := connection.ReadMessage()
		if readErr != nil {
			if ctx.Err() != nil || isTimeout(readErr) {
				summary.TimedOut = true
				break
			}
			return summary, fmt.Errorf("read signal event: %w", readErr)
		}

		if messageType != websocket.TextMessage {
			summary.EventCounts["BINARY_FRAME"]++
			continue
		}

		var envelope signalEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			summary.EventCounts["UNPARSEABLE_TEXT"]++
			continue
		}
		eventName := envelope.Method
		if eventName == "TRANSMIT" && envelope.MessageType != "" {
			eventName = envelope.MessageType
		}
		if eventName == "" {
			eventName = "UNNAMED_JSON"
		}
		summary.EventCounts[eventName]++

		switch eventName {
		case "AUTH_RESPONSE":
			summary.AuthCode = envelope.Code
			if envelope.Code != 0 {
				return summary, fmt.Errorf("signal authentication rejected with code %d", envelope.Code)
			}
			summary.Authenticated = true
			if !joinSent {
				if err := connection.WriteJSON(buildJoinMessage(ticket, serialNumber)); err != nil {
					return summary, fmt.Errorf("send JOIN_LIVE: %w", err)
				}
				joinSent = true
			}
		case "JOIN_LIVE_RESPONSE":
			summary.JoinCode = envelope.Code
			// The battery camera can respond with a temporary wake state
			// (codes 1 or 2) before it joins the live session.
			summary.JoinAcknowledged = envelope.Code >= 0 && envelope.Code <= 2
			if envelope.Code < 0 || envelope.Code > 2 {
				summary.DurationMillis = time.Since(started).Milliseconds()
				return summary, nil
			}
		case "PEER_IN":
			summary.PeerOnline = true
			summary.DurationMillis = time.Since(started).Milliseconds()
			return summary, nil
		}
	}

	summary.DurationMillis = time.Since(started).Milliseconds()
	return summary, nil
}

func buildSignalURL(ticket webRTCTicketResponse) (string, error) {
	parsed, err := url.Parse(ticket.Data.SignalServer)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("ticket contains an invalid signal server")
	}
	if parsed.Scheme != "wss" && parsed.Scheme != "ws" {
		return "", fmt.Errorf("ticket contains unsupported signal scheme %q", parsed.Scheme)
	}

	if parsed.Port() == "" && ticket.Data.SignalPort > 0 {
		defaultPort := (parsed.Scheme == "wss" && ticket.Data.SignalPort == 443) || (parsed.Scheme == "ws" && ticket.Data.SignalPort == 80)
		if !defaultPort {
			parsed.Host = net.JoinHostPort(parsed.Hostname(), strconv.Itoa(ticket.Data.SignalPort))
		}
	}
	path := ticket.Data.WebsocketPath
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func buildAuthMessage(ticket webRTCTicketResponse) map[string]any {
	return map[string]any{
		"method":      "AUTH",
		"accessToken": ticket.Data.AccessToken,
		"version":     "0.0.1",
		"id":          ticket.Data.ID,
		"clientType":  "app",
		"status":      "normal",
	}
}

func buildJoinMessage(ticket webRTCTicketResponse, serialNumber string) map[string]any {
	group := ticket.Data.GroupID
	if group == "" {
		group = serialNumber
	}
	role := ticket.Data.Role
	if role == "" {
		role = "viewer"
	}
	traceID := ticket.Data.TraceID
	if traceID == "" {
		traceID = fmt.Sprintf("trace-%d", time.Now().UnixMilli())
	}

	return map[string]any{
		"method":                        "JOIN_LIVE",
		"group":                         group,
		"role":                          role,
		"name":                          ticket.Data.ID,
		"traceId":                       traceID,
		"traceparent":                   newTraceParent(),
		"recipientClientId":             serialNumber,
		"supportRtcConnectAcceleration": 0,
		"accessToken":                   ticket.Data.AccessToken,
	}
}

func newTraceParent() string {
	// W3C traceparent shape. These IDs are correlation values, not credentials.
	var traceID [16]byte
	var parentID [8]byte
	if _, err := rand.Read(traceID[:]); err != nil {
		fallback := time.Now().UnixNano()
		return fmt.Sprintf("00-%032x-%016x-01", uint64(fallback), uint64(fallback)^0xa4c0a4c0a4c0a4c0)
	}
	if _, err := rand.Read(parentID[:]); err != nil {
		fallback := time.Now().UnixNano()
		return fmt.Sprintf("00-%x-%016x-01", traceID, uint64(fallback)^0xa4c0a4c0a4c0a4c0)
	}
	return fmt.Sprintf("00-%x-%x-01", traceID, parentID)
}

func isTimeout(err error) bool {
	var networkError net.Error
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "timeout") || (errors.As(err, &networkError) && networkError.Timeout()))
}
