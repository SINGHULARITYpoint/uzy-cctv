package devices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dydx/vico-cli/pkg/auth"
	"github.com/spf13/cobra"
)

const webRTCTicketEndpoint = "https://api-us.vicohome.io/device/getWebrtcTicket"
const wakeDeviceEndpoint = "https://api-us.vicohome.io/device/wakeupDevice"

type webRTCTicketRequest struct {
	Language                  string `json:"language"`
	CountryNo                 string `json:"countryNo"`
	SerialNumber              string `json:"serialNumber"`
	SupportUnlimitedWebsocket bool   `json:"supportUnlimitedWebsocket"`
	VerifyDormancyStatus      bool   `json:"verifyDormancyStatus"`
	Verify4GVIPStatus         bool   `json:"verify4GVIPStatus"`
	NeedWakeupDevice          bool   `json:"needWakeupDevice"`
}

type iceServer struct {
	URL        string `json:"url"`
	Credential string `json:"credential"`
	Username   string `json:"username"`
	IPAddress  string `json:"ipAddress"`
}

type webRTCTicketResponse struct {
	Result int    `json:"result"`
	Msg    string `json:"msg"`
	IOT    string `json:"iot"`
	Data   struct {
		SignalServer          string      `json:"signalServer"`
		SignalServerIPAddress string      `json:"signalServerIpAddress"`
		SignalPort            int         `json:"signalPort"`
		WebsocketPath         string      `json:"websocketPath"`
		AccessToken           string      `json:"accessToken"`
		Sign                  string      `json:"sign"`
		ID                    string      `json:"id"`
		GroupID               string      `json:"groupId"`
		Role                  string      `json:"role"`
		Time                  int64       `json:"time"`
		ExpirationTime        int64       `json:"expirationTime"`
		SignalPingInterval    int         `json:"signalPingInterval"`
		AppStopLiveTimeout    int         `json:"appStopLiveTimeout"`
		KissTarget            any         `json:"kissTarget"`
		TraceID               string      `json:"traceId"`
		IceServer             []iceServer `json:"iceServer"`
	} `json:"data"`
}

type webRTCTicketSummary struct {
	Result                int      `json:"result"`
	Message               string   `json:"message,omitempty"`
	IOTMode               string   `json:"iotMode,omitempty"`
	SignalServer          string   `json:"signalServer,omitempty"`
	SignalPort            int      `json:"signalPort,omitempty"`
	WebsocketPath         string   `json:"websocketPath,omitempty"`
	Role                  string   `json:"role,omitempty"`
	IssuedAt              int64    `json:"issuedAt,omitempty"`
	ExpiresAt             int64    `json:"expiresAt,omitempty"`
	SignalPingInterval    int      `json:"signalPingInterval,omitempty"`
	AppStopLiveTimeout    int      `json:"appStopLiveTimeout,omitempty"`
	KissTarget            any      `json:"kissTarget,omitempty"`
	ICEServerCount        int      `json:"iceServerCount"`
	ICEServerEndpoints    []string `json:"iceServerEndpoints,omitempty"`
	HasAccessToken        bool     `json:"hasAccessToken"`
	HasSignature          bool     `json:"hasSignature"`
	HasSessionIdentifiers bool     `json:"hasSessionIdentifiers"`
}

var ticketCmd = &cobra.Command{
	Use:   "ticket SERIAL_NUMBER",
	Short: "Request redacted WebRTC ticket metadata for an owned camera",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := auth.Authenticate()
		if err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		response, err := requestWebRTCTicket(cmd.Context(), token, args[0])
		if err != nil {
			return err
		}

		summary := summarizeWebRTCTicket(response)
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("format ticket summary: %w", err)
		}
		fmt.Println(string(encoded))
		return nil
	},
}

func requestWebRTCTicket(ctx context.Context, token, serialNumber string) (webRTCTicketResponse, error) {
	requestBody := webRTCTicketRequest{
		Language:                  "en",
		CountryNo:                 "US",
		SerialNumber:              serialNumber,
		SupportUnlimitedWebsocket: true,
		VerifyDormancyStatus:      true,
		Verify4GVIPStatus:         true,
		NeedWakeupDevice:          false,
	}

	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return webRTCTicketResponse{}, fmt.Errorf("encode ticket request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webRTCTicketEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return webRTCTicketResponse{}, fmt.Errorf("create ticket request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", token)

	responseBody, err := auth.ExecuteWithRetry(request)
	if err != nil {
		return webRTCTicketResponse{}, fmt.Errorf("request WebRTC ticket: %w", err)
	}

	var response webRTCTicketResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return webRTCTicketResponse{}, fmt.Errorf("decode WebRTC ticket response: %w", err)
	}
	if response.Result != 0 {
		return webRTCTicketResponse{}, fmt.Errorf("WebRTC ticket API error %d: %s", response.Result, response.Msg)
	}
	return response, nil
}

func wakeDevice(ctx context.Context, token, serialNumber string) (int, error) {
	body, err := json.Marshal(map[string]string{
		"countryNo": "US", "language": "en", "serialNumber": serialNumber,
	})
	if err != nil {
		return 0, fmt.Errorf("encode wake request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, wakeDeviceEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create wake request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", token)
	responseBody, err := auth.ExecuteWithRetry(request)
	if err != nil {
		return 0, fmt.Errorf("wake camera: %w", err)
	}
	var response struct {
		Result int `json:"result"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return 0, fmt.Errorf("decode wake response: %w", err)
	}
	return response.Result, nil
}

func summarizeWebRTCTicket(response webRTCTicketResponse) webRTCTicketSummary {
	endpoints := make([]string, 0, len(response.Data.IceServer))
	for _, server := range response.Data.IceServer {
		endpoints = append(endpoints, redactEndpoint(server.URL))
	}

	return webRTCTicketSummary{
		Result:                response.Result,
		Message:               response.Msg,
		IOTMode:               response.IOT,
		SignalServer:          redactEndpoint(response.Data.SignalServer),
		SignalPort:            response.Data.SignalPort,
		WebsocketPath:         response.Data.WebsocketPath,
		Role:                  response.Data.Role,
		IssuedAt:              response.Data.Time,
		ExpiresAt:             response.Data.ExpirationTime,
		SignalPingInterval:    response.Data.SignalPingInterval,
		AppStopLiveTimeout:    response.Data.AppStopLiveTimeout,
		KissTarget:            response.Data.KissTarget,
		ICEServerCount:        len(response.Data.IceServer),
		ICEServerEndpoints:    endpoints,
		HasAccessToken:        response.Data.AccessToken != "",
		HasSignature:          response.Data.Sign != "",
		HasSessionIdentifiers: response.Data.ID != "" || response.Data.GroupID != "",
	}
}

func redactEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}

	// TURN URLs are commonly formatted as turn:host:port. Strip query strings,
	// which can contain short-lived credentials.
	if index := strings.IndexByte(raw, '?'); index >= 0 {
		raw = raw[:index]
	}
	return raw
}
