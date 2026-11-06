package devices

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dydx/vico-cli/pkg/auth"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"github.com/spf13/cobra"
)

type mediaProbeSummary struct {
	WakeCode            int            `json:"wakeCode"`
	WebSocketConnected  bool           `json:"websocketConnected"`
	Authenticated       bool           `json:"authenticated"`
	JoinAcknowledged    bool           `json:"joinAcknowledged"`
	JoinCode            int            `json:"joinCode,omitempty"`
	JoinMessage         string         `json:"joinMessage,omitempty"`
	JoinResponseKeys    []string       `json:"joinResponseKeys,omitempty"`
	PeerOnline          bool           `json:"peerOnline"`
	PeerInKeys          []string       `json:"peerInKeys,omitempty"`
	PeerClientIsCamera  bool           `json:"peerClientIsCamera"`
	PeerRecipientIsID   bool           `json:"peerRecipientIsTicketId"`
	PeerSenderIsCamera  bool           `json:"peerSenderIsCamera"`
	OfferSent           bool           `json:"offerSent"`
	OfferAttempts       int            `json:"offerAttempts"`
	OfferBytes          int            `json:"offerBytes,omitempty"`
	CandidatesSent      uint64         `json:"candidatesSent"`
	SessionIDPresent    bool           `json:"sessionIdPresent"`
	SenderIDPresent     bool           `json:"senderIdPresent"`
	OfferResponseCode   int            `json:"offerResponseCode,omitempty"`
	ResponseCode        int            `json:"responseCode,omitempty"`
	ResponseMessage     string         `json:"responseMessage,omitempty"`
	TransmitCode        int            `json:"transmitResponseCode,omitempty"`
	TransmitMessage     string         `json:"transmitResponseMessage,omitempty"`
	AnswerAccepted      bool           `json:"answerAccepted"`
	DataChannelOpen     bool           `json:"dataChannelOpen"`
	StartLiveSent       bool           `json:"startLiveSent"`
	StartLiveAck        bool           `json:"startLiveAck"`
	VideoPackets        uint64         `json:"videoPackets"`
	VideoBytes          uint64         `json:"videoBytes"`
	RecordedBytes       uint64         `json:"recordedBytes"`
	FirstVideoMillis    int64          `json:"firstVideoMillis"`
	LastVideoMillis     int64          `json:"lastVideoMillis"`
	LargestVideoGapMs   int64          `json:"largestVideoGapMillis"`
	VideoActiveSeconds  uint64         `json:"videoSecondsWithPackets"`
	VideoSamples        uint64         `json:"videoSamples"`
	DroppedVideoPkts    uint64         `json:"droppedVideoPackets"`
	VideoIdleAtEndMs    int64          `json:"videoIdleAtEndMillis"`
	EndReason           string         `json:"endReason,omitempty"`
	AudioPackets        uint64         `json:"audioPackets"`
	ConnectionState     string         `json:"connectionState,omitempty"`
	LocalCandidateType  string         `json:"localCandidateType,omitempty"`
	RemoteCandidateType string         `json:"remoteCandidateType,omitempty"`
	SignalClosed        bool           `json:"signalClosed"`
	EventCounts         map[string]int `json:"eventCounts,omitempty"`
	DurationMillis      int64          `json:"durationMillis"`
}

type mediaSignalEnvelope struct {
	Method            string `json:"method"`
	MessageType       string `json:"messageType"`
	MessagePayload    string `json:"messagePayload"`
	SessionID         string `json:"sessionId"`
	RecipientClientID string `json:"recipientClientId"`
	SenderClientID    string `json:"senderClientId"`
	ClientID          string `json:"clientId"`
	Code              int    `json:"code"`
	Message           string `json:"message"`
}

var mediaProbeTimeout = 45 * time.Second
var mediaProbeNoTrickle bool
var mediaProbeOutput string
var mediaProbeRecordFor = 15 * time.Second
var mediaProbeObserveFor time.Duration
var mediaProbeProgressEvery time.Duration

var mediaProbeCmd = &cobra.Command{
	Use:   "media-probe SERIAL_NUMBER",
	Short: "Test independent WebRTC reception from an owned camera",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := auth.Authenticate()
		if err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
		wakeCode, err := wakeDevice(token, args[0])
		if err != nil {
			return err
		}
		if wakeCode != 0 {
			return fmt.Errorf("camera wake request returned code %d", wakeCode)
		}
		ticket, err := requestWebRTCTicket(token, args[0])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mediaProbeTimeout)
		defer cancel()
		summary, err := probeMedia(ctx, ticket, args[0])
		summary.WakeCode = wakeCode
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("format media summary: %w", err)
		}
		fmt.Println(string(encoded))
		return nil
	},
}

func init() {
	mediaProbeCmd.Flags().DurationVar(&mediaProbeTimeout, "timeout", 45*time.Second, "maximum media probe duration")
	mediaProbeCmd.Flags().BoolVar(&mediaProbeNoTrickle, "no-trickle", false, "diagnose SDP offer without sending local ICE candidates")
	mediaProbeCmd.Flags().StringVar(&mediaProbeOutput, "output", "", "write received H264 to this local file")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeRecordFor, "record-for", 15*time.Second, "time to record after startLive is sent")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeObserveFor, "observe-for", 0, "keep the live session open for this duration")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeProgressEvery, "progress-every", 0, "print non-sensitive connection progress at this interval")
}

func probeMedia(ctx context.Context, ticket webRTCTicketResponse, serial string) (mediaProbeSummary, error) {
	started := time.Now()
	summary := mediaProbeSummary{EventCounts: make(map[string]int)}
	var videoOutput *os.File
	if mediaProbeOutput != "" {
		var err error
		videoOutput, err = os.Create(mediaProbeOutput)
		if err != nil {
			return summary, fmt.Errorf("create H264 output: %w", err)
		}
		defer videoOutput.Close()
	}
	signalURL, err := buildSignalURL(ticket)
	if err != nil {
		return summary, err
	}
	connection, response, err := websocket.DefaultDialer.DialContext(ctx, signalURL, http.Header{})
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
	}

	var writeLock sync.Mutex
	writeSignal := func(message any) error {
		writeLock.Lock()
		defer writeLock.Unlock()
		return connection.WriteJSON(message)
	}
	pingEvery := time.Duration(ticket.Data.SignalPingInterval) * time.Second
	if pingEvery <= 0 {
		pingEvery = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeLock.Lock()
				err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(2*time.Second))
				writeLock.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	if err := writeSignal(buildAuthMessage(ticket)); err != nil {
		return summary, fmt.Errorf("send signal authentication: %w", err)
	}

	iceServers := make([]webrtc.ICEServer, 0, len(ticket.Data.IceServer))
	for _, server := range ticket.Data.IceServer {
		if server.URL != "" {
			iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{server.URL}, Username: server.Username, Credential: server.Credential})
		}
	}
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f",
		}, PayloadType: 121,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return summary, fmt.Errorf("register H264: %w", err)
	}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		}, PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return summary, fmt.Errorf("register PCMU: %w", err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		return summary, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors))
	peer, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers, BundlePolicy: webrtc.BundlePolicyMaxBundle})
	if err != nil {
		return summary, fmt.Errorf("create WebRTC peer: %w", err)
	}
	defer peer.Close()
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		return summary, fmt.Errorf("configure audio receiver: %w", err)
	}
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return summary, fmt.Errorf("configure video receiver: %w", err)
	}

	var sessionID, senderID string
	var sessionLock sync.RWMutex
	var candidateLock sync.Mutex
	var offerWritten bool
	var localCandidates []webrtc.ICECandidateInit
	var videoPackets, audioPackets atomic.Uint64
	var videoBytes atomic.Uint64
	var recordedBytes atomic.Uint64
	var candidatesSent atomic.Uint64
	var channelOpen atomic.Bool
	var startLiveSent atomic.Bool
	var startLiveAck atomic.Bool
	var observationCompleted atomic.Bool
	var authenticated, peerOnline, answerAccepted atomic.Bool
	var recordDeadline atomic.Int64
	var firstVideoMillis, lastVideoMillis, largestVideoGapMillis, lastVideoSecond atomic.Int64
	var videoActiveSeconds atomic.Uint64
	var videoSamples, droppedVideoPackets atomic.Uint64
	firstVideoMillis.Store(-1)
	lastVideoMillis.Store(-1)
	lastVideoSecond.Store(-1)
	var connectionState atomic.Value
	connectionState.Store("new")
	if mediaProbeProgressEvery > 0 {
		go func() {
			ticker := time.NewTicker(mediaProbeProgressEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					state, _ := connectionState.Load().(string)
					fmt.Fprintf(os.Stderr, "live probe: %.0fs auth=%t peer=%t answer=%t channel=%t start=%t video=%d state=%s\n",
						time.Since(started).Seconds(), authenticated.Load(), peerOnline.Load(),
						answerAccepted.Load(), channelOpen.Load(), startLiveSent.Load(), videoPackets.Load(), state)
				}
			}
		}()
	}
	shouldFinish := func() bool {
		if mediaProbeObserveFor > 0 {
			return false // the observation timer closes the signal connection
		}
		if videoPackets.Load() < 100 || !startLiveSent.Load() {
			return false
		}
		if videoOutput != nil && time.Now().UnixNano() < recordDeadline.Load() {
			return false
		}
		return true
	}
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) { connectionState.Store(state.String()) })
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			builder := samplebuilder.New(50, &codecs.H264Packet{}, 90000)
			writeSamples := func() {
				for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
					videoSamples.Add(1)
					droppedVideoPackets.Add(uint64(sample.PrevDroppedPackets))
					if videoOutput != nil && len(sample.Data) > 0 {
						if count, err := videoOutput.Write(sample.Data); err == nil {
							recordedBytes.Add(uint64(count))
						}
					}
				}
			}
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					if track.Kind() == webrtc.RTPCodecTypeVideo {
						builder.Flush()
						writeSamples()
					}
					return
				}
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					elapsed := time.Since(started).Milliseconds()
					firstVideoMillis.CompareAndSwap(-1, elapsed)
					previous := lastVideoMillis.Swap(elapsed)
					if previous >= 0 && elapsed-previous > largestVideoGapMillis.Load() {
						largestVideoGapMillis.Store(elapsed - previous)
					}
					if lastVideoSecond.Swap(elapsed/1000) != elapsed/1000 {
						videoActiveSeconds.Add(1)
					}
					videoPackets.Add(1)
					videoBytes.Add(uint64(len(packet.Payload)))
					builder.Push(packet)
					writeSamples()
					if shouldFinish() {
						_ = connection.Close()
					}
				} else {
					audioPackets.Add(1)
				}
			}
		}()
	})
	channel, err := peer.CreateDataChannel(serial, nil)
	if err != nil {
		return summary, fmt.Errorf("create camera data channel: %w", err)
	}
	channel.OnOpen(func() {
		channelOpen.Store(true)
		now := time.Now()
		if videoOutput != nil {
			recordDeadline.Store(now.Add(mediaProbeRecordFor).UnixNano())
		}
		command, err := json.Marshal(map[string]any{
			"requestID":    fmt.Sprintf("%d-%d", now.UnixMilli(), rand.Intn(1000)),
			"connectionID": "7893feb", "timeStamp": now.Unix(),
			"action": "startLive", "targetId": serial,
			"size": "1920x1080", "resolution": "2304x1296",
		})
		if err == nil && channel.SendText(string(command)) == nil {
			startLiveSent.Store(true)
			if mediaProbeObserveFor > 0 {
				go func() {
					select {
					case <-time.After(mediaProbeObserveFor):
						observationCompleted.Store(true)
						_ = connection.Close()
					case <-ctx.Done():
						return
					}
				}()
			}
		}
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		var result struct {
			Action    string `json:"action"`
			ReturnVal int    `json:"returnVal"`
		}
		if json.Unmarshal(message.Data, &result) == nil && result.Action == "startLive" && result.ReturnVal == 0 {
			startLiveAck.Store(true)
		}
	})
	sendCandidate := func(candidateJSON webrtc.ICECandidateInit) {
		sessionLock.RLock()
		currentSessionID, currentSenderID := sessionID, senderID
		sessionLock.RUnlock()
		if currentSessionID == "" {
			return
		}
		mid := "0"
		if candidateJSON.SDPMid != nil && *candidateJSON.SDPMid != "" {
			mid = *candidateJSON.SDPMid
		}
		lineIndex := uint16(0)
		if candidateJSON.SDPMLineIndex != nil {
			lineIndex = *candidateJSON.SDPMLineIndex
		}
		payload, err := json.Marshal(map[string]any{
			"candidate":     candidateJSON.Candidate,
			"sdpMid":        mid,
			"sdpMLineIndex": lineIndex,
		})
		if err != nil {
			return
		}
		if writeSignal(map[string]any{
			"method": "TRANSMIT", "messageType": "ICE_CANDIDATE",
			"messagePayload":    base64.StdEncoding.EncodeToString(payload),
			"recipientClientId": serial, "senderClientId": currentSenderID,
			"sessionId": currentSessionID, "version": "0.0.1",
		}) == nil {
			candidatesSent.Add(1)
		}
	}
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		if mediaProbeNoTrickle {
			return
		}
		candidateJSON := candidate.ToJSON()
		candidateLock.Lock()
		localCandidates = append(localCandidates, candidateJSON)
		ready := offerWritten
		candidateLock.Unlock()
		if ready {
			sendCandidate(candidateJSON)
		}
	})

	joinSent := false
	peerPresent := false
	var offerPayload []byte
	for {
		if ctx.Err() != nil {
			break
		}
		frameType, payload, readErr := connection.ReadMessage()
		if readErr != nil {
			summary.SignalClosed = true
			if ctx.Err() != nil || isTimeout(readErr) || videoPackets.Load() > 0 {
				break
			}
			break
		}
		if frameType != websocket.TextMessage {
			summary.EventCounts["BINARY_FRAME"]++
			continue
		}
		var event mediaSignalEnvelope
		if err := json.Unmarshal(payload, &event); err != nil {
			summary.EventCounts["UNPARSEABLE_TEXT"]++
			continue
		}
		kind := event.Method
		if kind == "TRANSMIT" && event.MessageType != "" {
			kind = event.MessageType
		}
		if kind == "" {
			kind = "UNNAMED_JSON"
		}
		summary.EventCounts[kind]++
		switch kind {
		case "AUTH_RESPONSE":
			if event.Code != 0 {
				return summary, fmt.Errorf("signal authentication rejected with code %d", event.Code)
			}
			summary.Authenticated = true
			authenticated.Store(true)
			if !joinSent {
				if err := writeSignal(buildJoinMessage(ticket, serial)); err != nil {
					return summary, fmt.Errorf("send JOIN_LIVE: %w", err)
				}
				joinSent = true
			}
		case "JOIN_LIVE_RESPONSE":
			summary.JoinCode = event.Code
			summary.JoinMessage = safeJoinMessage(event.Message, serial, ticket)
			var fields map[string]json.RawMessage
			if json.Unmarshal(payload, &fields) == nil {
				for key := range fields {
					summary.JoinResponseKeys = append(summary.JoinResponseKeys, key)
				}
				sort.Strings(summary.JoinResponseKeys)
			}
			// Codes 1 and 2 keep the viewer joined while the battery camera
			// wakes. The app waits for PEER_IN in these states.
			summary.JoinAcknowledged = event.Code >= 0 && event.Code <= 2
			if event.Code < 0 || event.Code > 2 {
				return finishMediaSummary(summary, started, &videoPackets, &audioPackets, &channelOpen, &startLiveSent, &connectionState), nil
			}
		case "PEER_IN":
			if peerPresent {
				break
			}
			peerPresent = true
			summary.PeerOnline = true
			peerOnline.Store(true)
			var peerFields map[string]json.RawMessage
			if json.Unmarshal(payload, &peerFields) == nil {
				for key := range peerFields {
					summary.PeerInKeys = append(summary.PeerInKeys, key)
				}
				sort.Strings(summary.PeerInKeys)
			}
			sessionLock.Lock()
			sessionID = fmt.Sprintf("Android-%s-%d", ticket.Data.ID, time.Now().UnixMilli())
			senderID = event.RecipientClientID
			if senderID == "" {
				senderID = ticket.Data.ID
			}
			sessionLock.Unlock()
			summary.SessionIDPresent = sessionID != ""
			summary.SenderIDPresent = senderID != ""
			summary.PeerRecipientIsID = event.RecipientClientID == ticket.Data.ID
			summary.PeerSenderIsCamera = event.SenderClientID == serial
			summary.PeerClientIsCamera = event.ClientID == serial
			if !summary.AnswerAccepted {
				if offerPayload == nil {
					offer, err := peer.CreateOffer(nil)
					if err != nil {
						return summary, fmt.Errorf("create SDP offer: %w", err)
					}
					if err := peer.SetLocalDescription(offer); err != nil {
						return summary, fmt.Errorf("set local SDP offer: %w", err)
					}
					offerPayload, err = json.Marshal(map[string]string{"type": "offer", "sdp": offer.SDP})
					if err != nil {
						return summary, fmt.Errorf("encode SDP offer: %w", err)
					}
					summary.OfferBytes = len(offer.SDP)
				}
				if err := writeSignal(map[string]any{
					"method": "TRANSMIT", "messageType": "SDP_OFFER",
					"messagePayload":    base64.StdEncoding.EncodeToString(offerPayload),
					"mode":              "vicoo",
					"recipientClientId": serial, "senderClientId": senderID,
					"sessionId": sessionID, "viewerType": "a4x_sdk",
					"version": "0.0.1", "resolution": "1280x720",
				}); err != nil {
					return summary, fmt.Errorf("send SDP offer: %w", err)
				}
				summary.OfferSent = true
				summary.OfferAttempts++
				candidateLock.Lock()
				offerWritten = true
				queued := append([]webrtc.ICECandidateInit(nil), localCandidates...)
				candidateLock.Unlock()
				for _, candidate := range queued {
					sendCandidate(candidate)
				}
			}
		case "PEER_OUT":
			peerPresent = false
			peerOnline.Store(false)
		case "SDP_ANSWER":
			answer, err := decodeSignalPayload(event.MessagePayload)
			if err != nil {
				return summary, fmt.Errorf("decode SDP answer: %w", err)
			}
			var description struct {
				SDP string `json:"sdp"`
			}
			if err := json.Unmarshal(answer, &description); err != nil || description.SDP == "" {
				return summary, fmt.Errorf("invalid SDP answer")
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: description.SDP}); err != nil {
				return summary, fmt.Errorf("set remote SDP answer: %w", err)
			}
			summary.AnswerAccepted = true
			answerAccepted.Store(true)
		case "SDP_OFFER_RESPONSE":
			summary.OfferResponseCode = event.Code
		case "RESPONSE":
			summary.ResponseCode = event.Code
			summary.ResponseMessage = safeJoinMessage(event.Message, serial, ticket)
		case "TRANSMIT_RESPONSE":
			summary.TransmitCode = event.Code
			summary.TransmitMessage = safeJoinMessage(event.Message, serial, ticket)
		case "ICE_CANDIDATE":
			candidate, err := decodeSignalPayload(event.MessagePayload)
			if err != nil {
				continue
			}
			var init webrtc.ICECandidateInit
			if json.Unmarshal(candidate, &init) == nil && init.Candidate != "" {
				_ = peer.AddICECandidate(init)
			}
		}
		if shouldFinish() {
			break
		}
	}
	summary.CandidatesSent = candidatesSent.Load()
	summary.VideoBytes = videoBytes.Load()
	summary.RecordedBytes = recordedBytes.Load()
	summary.FirstVideoMillis = firstVideoMillis.Load()
	summary.LastVideoMillis = lastVideoMillis.Load()
	summary.LargestVideoGapMs = largestVideoGapMillis.Load()
	summary.VideoActiveSeconds = videoActiveSeconds.Load()
	summary.VideoSamples = videoSamples.Load()
	summary.DroppedVideoPkts = droppedVideoPackets.Load()
	if summary.LastVideoMillis >= 0 {
		summary.VideoIdleAtEndMs = time.Since(started).Milliseconds() - summary.LastVideoMillis
	} else {
		summary.VideoIdleAtEndMs = time.Since(started).Milliseconds()
	}
	if ctx.Err() != nil {
		summary.EndReason = "timeout"
	} else if observationCompleted.Load() {
		summary.EndReason = "observation_complete"
	} else if summary.SignalClosed {
		summary.EndReason = "signal_closed"
	} else {
		summary.EndReason = "probe_complete"
	}
	summary.StartLiveAck = startLiveAck.Load()
	if peer.SCTP() != nil && peer.SCTP().Transport() != nil && peer.SCTP().Transport().ICETransport() != nil {
		if pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			summary.LocalCandidateType = pair.Local.Typ.String()
			summary.RemoteCandidateType = pair.Remote.Typ.String()
		}
	}
	return finishMediaSummary(summary, started, &videoPackets, &audioPackets, &channelOpen, &startLiveSent, &connectionState), nil
}

func safeJoinMessage(message, serial string, ticket webRTCTicketResponse) string {
	for _, secret := range []string{serial, ticket.Data.AccessToken, ticket.Data.ID, ticket.Data.GroupID, ticket.Data.Sign} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = regexp.MustCompile(`(?i)[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+`).ReplaceAllString(message, "[email]")
	message = regexp.MustCompile(`[A-Za-z0-9_\-]*[0-9][A-Za-z0-9_\-]{7,}`).ReplaceAllString(message, "[id]")
	if len(message) > 200 {
		message = message[:200]
	}
	return message
}

func decodeSignalPayload(payload string) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(payload), "{") {
		return []byte(payload), nil
	}
	return base64.StdEncoding.DecodeString(payload)
}

func finishMediaSummary(summary mediaProbeSummary, started time.Time, video, audio *atomic.Uint64, channel, startLive *atomic.Bool, state *atomic.Value) mediaProbeSummary {
	summary.VideoPackets = video.Load()
	summary.AudioPackets = audio.Load()
	summary.DataChannelOpen = channel.Load()
	summary.StartLiveSent = startLive.Load()
	summary.ConnectionState, _ = state.Load().(string)
	summary.DurationMillis = time.Since(started).Milliseconds()
	return summary
}
