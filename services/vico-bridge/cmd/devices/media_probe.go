package devices

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dydx/vico-cli/pkg/auth"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"github.com/spf13/cobra"
)

type mediaProbeSummary struct {
	WakeCode              int              `json:"wakeCode"`
	TicketMillis          int64            `json:"ticketMillis"`
	WebSocketConnected    bool             `json:"websocketConnected"`
	Authenticated         bool             `json:"authenticated"`
	JoinAcknowledged      bool             `json:"joinAcknowledged"`
	JoinCode              int              `json:"joinCode,omitempty"`
	JoinMessage           string           `json:"joinMessage,omitempty"`
	JoinResponseKeys      []string         `json:"joinResponseKeys,omitempty"`
	PeerOnline            bool             `json:"peerOnline"`
	PeerInKeys            []string         `json:"peerInKeys,omitempty"`
	PeerClientIsCamera    bool             `json:"peerClientIsCamera"`
	PeerRecipientIsID     bool             `json:"peerRecipientIsTicketId"`
	PeerSenderIsCamera    bool             `json:"peerSenderIsCamera"`
	OfferSent             bool             `json:"offerSent"`
	OfferAttempts         int              `json:"offerAttempts"`
	OfferBytes            int              `json:"offerBytes,omitempty"`
	CandidatesSent        uint64           `json:"candidatesSent"`
	SessionIDPresent      bool             `json:"sessionIdPresent"`
	SenderIDPresent       bool             `json:"senderIdPresent"`
	OfferResponseCode     int              `json:"offerResponseCode,omitempty"`
	ResponseCode          int              `json:"responseCode,omitempty"`
	ResponseMessage       string           `json:"responseMessage,omitempty"`
	TransmitCode          int              `json:"transmitResponseCode,omitempty"`
	TransmitMessage       string           `json:"transmitResponseMessage,omitempty"`
	AnswerAccepted        bool             `json:"answerAccepted"`
	DataChannelOpen       bool             `json:"dataChannelOpen"`
	StartLiveSent         bool             `json:"startLiveSent"`
	StartLiveAck          bool             `json:"startLiveAck"`
	VideoPackets          uint64           `json:"videoPackets"`
	VideoBytes            uint64           `json:"videoBytes"`
	RecordedBytes         uint64           `json:"recordedBytes"`
	FirstVideoMillis      int64            `json:"firstVideoMillis"`
	FirstFrameMillis      int64            `json:"firstFrameMillis"`
	LastVideoMillis       int64            `json:"lastVideoMillis"`
	LargestVideoGapMs     int64            `json:"largestVideoGapMillis"`
	VideoActiveSeconds    uint64           `json:"videoSecondsWithPackets"`
	VideoSamples          uint64           `json:"videoSamples"`
	DroppedVideoPkts      uint64           `json:"droppedVideoPackets"`
	RTPSeqGaps            uint64           `json:"rtpSeqGaps"`
	RTPSeqMissing         uint64           `json:"rtpSeqMissing"`
	RTPSeqLate            uint64           `json:"rtpSeqLate"`
	AssemblyDroppedPkts   uint64           `json:"assemblyDroppedPackets"`
	AssemblyLatePkts      uint64           `json:"assemblyLatePackets"`
	PictureLossNegotiated bool             `json:"pictureLossNegotiated,omitempty"`
	LabDropMillis         int64            `json:"labDropMillis,omitempty"`
	LabPLIMillis          int64            `json:"labPliMillis,omitempty"`
	LabIDRMillis          int64            `json:"labIdrMillis,omitempty"`
	LabPreviousIDRMillis  int64            `json:"labPreviousIdrMillis,omitempty"`
	LabIDRIntervalMillis  int64            `json:"labIdrIntervalMillis,omitempty"`
	OutputFrames          uint64           `json:"outputFrames,omitempty"`
	LargestFrameGapMs     int64            `json:"largestFrameGapMillis,omitempty"`
	StreamProfile         string           `json:"streamProfile,omitempty"`
	VerifiedWidth         int              `json:"verifiedWidth,omitempty"`
	VerifiedHeight        int              `json:"verifiedHeight,omitempty"`
	PostStartBytes        uint64           `json:"postStartBytes,omitempty"`
	PostStartBitrateKbps  float64          `json:"postStartBitrateKbps,omitempty"`
	RTPNALTypes           map[uint8]uint64 `json:"rtpNalTypes,omitempty"`
	OutputNALTypes        map[uint8]uint64 `json:"outputNalTypes,omitempty"`
	CountersOnly          bool             `json:"countersOnly,omitempty"`
	RXErrors              uint64           `json:"rxErrors,omitempty"`
	RXTimeouts            uint64           `json:"rxTimeouts,omitempty"`
	RTPErrors             uint64           `json:"rtpErrors,omitempty"`
	RXLastError           string           `json:"rxLastError,omitempty"`
	VideoIdleAtEndMs      int64            `json:"videoIdleAtEndMillis"`
	EndReason             string           `json:"endReason,omitempty"`
	AudioPackets          uint64           `json:"audioPackets"`
	ConnectionState       string           `json:"connectionState,omitempty"`
	LocalCandidateType    string           `json:"localCandidateType,omitempty"`
	RemoteCandidateType   string           `json:"remoteCandidateType,omitempty"`
	SignalClosed          bool             `json:"signalClosed"`
	LockWaitMillis        int64            `json:"lockWaitMillis,omitempty"`
	WakeUTC               string           `json:"wakeUtc,omitempty"`
	ICEConnectedUTC       string           `json:"iceConnectedUtc,omitempty"`
	DataChannelOpenUTC    string           `json:"dataChannelOpenUtc,omitempty"`
	FirstFrameUTC         string           `json:"firstFrameUtc,omitempty"`
	EventCounts           map[string]int   `json:"eventCounts,omitempty"`
	DurationMillis        int64            `json:"durationMillis"`
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
var mediaProbeContinuous bool
var mediaProbeHealthLog string
var mediaProbeLabDropAfter time.Duration
var mediaProbeHoldFile string
var mediaProbeForceDirect bool
var mediaProbeRetryLimit int
var mediaProbeStartupLockFile string
var mediaProbeStartupLockTimeout time.Duration
var mediaProbeStreamProfile = "2k"
var mediaProbeCountersOnly bool
var mediaProbeMaxDuration time.Duration
var errOutputClosed = errors.New("output pipe closed")

func utcNowTag() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func sanitizeLockReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

// acquireStartupLock acquires an OS-backed exclusive cross-process file lock for
// the full startup window without premature expiry. A crashed holder releases
// ownership automatically via OS handle teardown; a cancelled waiter returns
// an error and never proceeds without ownership.
func acquireStartupLock(ctx context.Context, path string, waitTimeout time.Duration, health *log.Logger) (func(string), error) {
	if path == "" {
		return func(string) {}, nil
	}
	waitCtx := ctx
	if waitTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, waitTimeout)
		defer cancel()
	}
	waitStarted := time.Now()
	if waitCtx.Err() != nil {
		if health != nil {
			health.Printf("event=lock_cancelled wait_ms=0 ts_utc=%s reason=%s", utcNowTag(), sanitizeLockReason(waitCtx.Err()))
		}
		return nil, waitCtx.Err()
	}
	lockFile, err := openOSLockFile(path)
	if err != nil {
		if health != nil {
			health.Printf("event=lock_error wait_ms=%d ts_utc=%s", time.Since(waitStarted).Milliseconds(), utcNowTag())
		}
		return nil, err
	}
	loggedWaiting := false
	for {
		if waitCtx.Err() != nil {
			lockFile.closeOnly()
			if health != nil {
				health.Printf("event=lock_cancelled wait_ms=%d ts_utc=%s reason=%s",
					time.Since(waitStarted).Milliseconds(), utcNowTag(), sanitizeLockReason(waitCtx.Err()))
			}
			return nil, waitCtx.Err()
		}
		locked, lockErr := lockFile.tryLock()
		if lockErr != nil {
			lockFile.closeOnly()
			if health != nil {
				health.Printf("event=lock_error wait_ms=%d ts_utc=%s", time.Since(waitStarted).Milliseconds(), utcNowTag())
			}
			return nil, lockErr
		}
		if locked {
			acquiredAt := time.Now().UTC()
			waitMs := acquiredAt.Sub(waitStarted).Milliseconds()
			if health != nil {
				health.Printf("event=lock_acquired wait_ms=%d ts_utc=%s", waitMs, acquiredAt.Format(time.RFC3339Nano))
			}
			var once sync.Once
			return func(reason string) {
				once.Do(func() {
					lockFile.unlockAndClose()
					if health != nil {
						health.Printf("event=lock_released reason=%s held_ms=%d ts_utc=%s",
							reason, time.Since(acquiredAt).Milliseconds(), utcNowTag())
					}
				})
			}, nil
		}
		if !loggedWaiting && health != nil {
			loggedWaiting = true
			health.Printf("event=lock_waiting ts_utc=%s", utcNowTag())
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			lockFile.closeOnly()
			if health != nil {
				health.Printf("event=lock_cancelled wait_ms=%d ts_utc=%s reason=%s",
					time.Since(waitStarted).Milliseconds(), utcNowTag(), sanitizeLockReason(waitCtx.Err()))
			}
			return nil, waitCtx.Err()
		case <-timer.C:
		}
	}
}

func runWithStartupLock(ctx context.Context, lockPath string, waitTimeout time.Duration, health *log.Logger, fn func(release func(string)) error) error {
	release, err := acquireStartupLock(ctx, lockPath, waitTimeout, health)
	if err != nil {
		return err
	}
	defer release("attempt_exit")
	return fn(release)
}

func continuousRetryBackoff(failedStarts int) time.Duration {
	switch {
	case failedStarts <= 0:
		return 1500 * time.Millisecond
	case failedStarts == 1:
		return 3 * time.Second
	case failedStarts == 2:
		return 5 * time.Second
	case failedStarts == 3:
		return 8 * time.Second
	default:
		return 10 * time.Second
	}
}

func shouldAbortDeadSession(nowMs, lastRTPMs, lastFrameMs, lastPeerOutMs, iceDisconnectedMs int64, peerOnline, answerAccepted, iceFailed bool, packets uint64) (string, bool) {
	if iceFailed {
		return "ice_failed", true
	}
	rtpIdle := lastRTPMs < 0 || nowMs-lastRTPMs >= 5000
	if lastFrameMs >= 0 && ((nowMs-lastFrameMs >= 8000 && rtpIdle) || nowMs-lastFrameMs >= 20000) {
		return "output_stall", true
	}
	if lastPeerOutMs >= 0 && !peerOnline && answerAccepted {
		if packets == 0 && nowMs-lastPeerOutMs >= 3500 {
			return "peer_lost", true
		}
		if packets > 0 && (lastRTPMs < 0 || nowMs-lastRTPMs >= 2500) && nowMs-lastPeerOutMs >= 2000 {
			return "peer_lost", true
		}
	}
	if iceDisconnectedMs >= 0 && nowMs-iceDisconnectedMs >= 6000 && (lastRTPMs < 0 || nowMs-lastRTPMs >= 4000) {
		return "ice_disconnected", true
	}
	return "", false
}

var mediaProbeCmd = &cobra.Command{
	Use:   "media-probe SERIAL_NUMBER",
	Short: "Test independent WebRTC reception from an owned camera",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		baseCtx := cmd.Context()
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		runCtx, stop := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		if mediaProbeMaxDuration > 0 {
			var cancelMax context.CancelFunc
			runCtx, cancelMax = context.WithTimeout(runCtx, mediaProbeMaxDuration)
			defer cancelMax()
		}
		if _, err := parseStreamProfile(mediaProbeStreamProfile); err != nil {
			return err
		}
		if cmd.Flags().Changed("lab-drop-after") && (mediaProbeLabDropAfter <= 0 || mediaProbeContinuous || mediaProbeCountersOnly ||
			mediaProbeObserveFor <= mediaProbeLabDropAfter || mediaProbeOutput == "" || mediaProbeOutput == "-") {
			return fmt.Errorf("lab loss injection requires standalone file output and --observe-for longer than --lab-drop-after")
		}
		if mediaProbeCountersOnly && mediaProbeOutput != "" {
			return fmt.Errorf("--counters-only cannot be combined with --output")
		}
		if !mediaProbeContinuous {
			var health *log.Logger
			if mediaProbeHealthLog != "" {
				var closeLog func()
				var err error
				health, closeLog, err = openProbeHealthLog(mediaProbeHealthLog)
				if err != nil {
					return err
				}
				defer closeLog()
				health.Printf("event=attempt attempt=1 ts_utc=%s", utcNowTag())
			}
			_, err := runMediaProbe(runCtx, args[0], health)
			return err
		}
		if (!mediaProbeCountersOnly && mediaProbeOutput != "-") || mediaProbeTimeout <= 0 {
			return fmt.Errorf("continuous mode requires --output - and a positive --timeout")
		}
		health, closeLog, err := openProbeHealthLog(mediaProbeHealthLog)
		if err != nil {
			return err
		}
		defer closeLog()
		failedStarts := 0
		for attempt := 1; runCtx.Err() == nil; attempt++ {
			health.Printf("event=attempt attempt=%d ts_utc=%s", attempt, utcNowTag())
			produced, err := runMediaProbe(runCtx, args[0], health)
			if errors.Is(err, errOutputClosed) {
				return nil
			}
			if runCtx.Err() != nil {
				return nil
			}
			if mediaProbeObserveFor > 0 {
				select {
				case <-runCtx.Done():
				case <-time.After(5 * time.Second):
				}
				return err
			}
			if produced {
				failedStarts = 0
			} else {
				failedStarts++
				if mediaProbeRetryLimit > 0 && failedStarts >= mediaProbeRetryLimit {
					return fmt.Errorf("live source failed to start after %d attempts", failedStarts)
				}
			}
			backoff := continuousRetryBackoff(failedStarts)
			health.Printf("event=retry backoff_ms=%d failed_starts=%d ts_utc=%s", backoff.Milliseconds(), failedStarts, utcNowTag())
			select {
			case <-runCtx.Done():
				return nil
			case <-time.After(backoff):
			}
		}
		return nil
	},
}

func runMediaProbe(ctx context.Context, serial string, health *log.Logger) (produced bool, err error) {
	lockWaitStart := time.Now()
	err = runWithStartupLock(ctx, mediaProbeStartupLockFile, mediaProbeStartupLockTimeout, health, func(releaseStartupLock func(string)) error {
		lockWaitMs := time.Since(lockWaitStart).Milliseconds()
		started := time.Now()
		startup, cancel := context.WithTimeout(ctx, mediaProbeTimeout)
		defer cancel()
		stage := "account_auth"
		var attemptErr error
		if health != nil {
			defer func() {
				if attemptErr != nil {
					health.Printf("event=error stage=%s deadline=%t elapsed_ms=%d ts_utc=%s", stage, startup.Err() != nil, time.Since(started).Milliseconds(), utcNowTag())
				}
			}()
		}
		var token string
		token, attemptErr = auth.AuthenticateContext(startup)
		if attemptErr != nil {
			return fmt.Errorf("authentication failed: %w", attemptErr)
		}
		stage = "wake"
		wakeStart := time.Now()
		if health != nil {
			health.Printf("event=wake_start elapsed_ms=%d ts_utc=%s", wakeStart.Sub(started).Milliseconds(), wakeStart.UTC().Format(time.RFC3339Nano))
		}
		var wakeCode int
		wakeCode, attemptErr = wakeDevice(startup, token, serial)
		if attemptErr != nil {
			return attemptErr
		}
		wakeDone := time.Now()
		wakeUTC := wakeDone.UTC().Format(time.RFC3339Nano)
		if health != nil {
			health.Printf("event=wake wake_code=%d wake_ms=%d elapsed_ms=%d ts_utc=%s",
				wakeCode, wakeDone.Sub(wakeStart).Milliseconds(), wakeDone.Sub(started).Milliseconds(), wakeUTC)
		}
		if wakeCode != 0 {
			attemptErr = fmt.Errorf("camera wake request returned code %d", wakeCode)
			return attemptErr
		}
		stage = "ticket"
		var ticket webRTCTicketResponse
		ticket, attemptErr = requestWebRTCTicket(startup, token, serial)
		if attemptErr != nil {
			return attemptErr
		}
		ticketMillis := time.Since(started).Milliseconds()
		stage = "signal_media"
		if health != nil {
			health.Printf("event=ticket wake_code=%d elapsed_ms=%d ts_utc=%s", wakeCode, ticketMillis, utcNowTag())
		}
		var summary mediaProbeSummary
		summary, attemptErr = probeMedia(ctx, startup, started, ticket, serial, health, func() {
			releaseStartupLock("first_frame")
		})
		summary.LockWaitMillis = lockWaitMs
		summary.WakeCode = wakeCode
		summary.WakeUTC = wakeUTC
		summary.TicketMillis = ticketMillis
		if mediaProbeCountersOnly {
			produced = summary.StartLiveAck && summary.VideoPackets > 0
		} else {
			produced = summary.RecordedBytes > 0
		}
		if attemptErr != nil || mediaProbeContinuous {
			return attemptErr
		}
		encoded, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("format media summary: %w", err)
		}
		fmt.Println(string(encoded))
		return nil
	})
	return produced, err
}

func init() {
	mediaProbeCmd.Flags().DurationVar(&mediaProbeTimeout, "timeout", 45*time.Second, "maximum media probe duration")
	mediaProbeCmd.Flags().BoolVar(&mediaProbeNoTrickle, "no-trickle", false, "diagnose SDP offer without sending local ICE candidates")
	mediaProbeCmd.Flags().StringVar(&mediaProbeOutput, "output", "", "write received H264 to this local file")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeRecordFor, "record-for", 15*time.Second, "time to record after startLive is sent")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeObserveFor, "observe-for", 0, "keep the live session open for this duration")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeProgressEvery, "progress-every", 0, "print non-sensitive connection progress at this interval")
	mediaProbeCmd.Flags().BoolVar(&mediaProbeContinuous, "continuous", false, "stream H264 to stdout and reconnect after signal or media loss")
	mediaProbeCmd.Flags().StringVar(&mediaProbeHealthLog, "health-log", "", "append continuous health to a private log file")
	mediaProbeCmd.Flags().StringVar(&mediaProbeHoldFile, "hold-start-live-file", "", "after the data channel opens, wait for this file to exist before sending startLive (test harness)")
	mediaProbeCmd.Flags().BoolVar(&mediaProbeForceDirect, "force-direct", false, "only use direct host/srflx candidates without relay (skips TURN servers)")
	mediaProbeCmd.Flags().IntVar(&mediaProbeRetryLimit, "retry-limit", 0, "stop continuous mode after this many consecutive failed startups (0 keeps retrying)")
	mediaProbeCmd.Flags().StringVar(&mediaProbeStartupLockFile, "startup-lock-file", "", "optional lock file path used to serialize camera startup and retries across processes")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeStartupLockTimeout, "startup-lock-timeout", 0, "optional maximum time to wait for startup lock acquisition before aborting without handshake (0 waits until owned or cancelled)")
	mediaProbeCmd.Flags().StringVar(&mediaProbeStreamProfile, "stream-profile", "2k", "requested live stream profile: 2k (2304x1296) or 360p (640x360 low-bandwidth)")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeLabDropAfter, "lab-drop-after", 0, "discard one non-IDR RTP packet just after a keyframe past this main-stream warmup")
	mediaProbeCmd.Flags().BoolVar(&mediaProbeCountersOnly, "counters-only", false, "receive RTP directly into telemetry and error counters without H.264 assembly or output writes")
	mediaProbeCmd.Flags().DurationVar(&mediaProbeMaxDuration, "max-duration", 0, "maximum total runtime across retries before orderly shutdown")
}

func buildICEServers(servers []iceServer, forceDirect bool, maxTurn int) []webrtc.ICEServer {
	iceServers := make([]webrtc.ICEServer, 0, len(servers))
	turnCount := 0
	for _, server := range servers {
		if server.URL == "" {
			continue
		}
		isTurn := strings.HasPrefix(server.URL, "turn:") || strings.HasPrefix(server.URL, "turns:")
		if forceDirect && isTurn {
			continue
		}
		if isTurn && maxTurn > 0 {
			if turnCount >= maxTurn {
				continue
			}
			turnCount++
		}
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{server.URL}, Username: server.Username, Credential: server.Credential})
	}
	return iceServers
}

func probeMedia(ctx, startup context.Context, started time.Time, ticket webRTCTicketResponse, serial string, health *log.Logger, onFirstFrame func()) (mediaProbeSummary, error) {
	ctx, cancel := context.WithCancel(ctx)
	holdCtx, holdCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var teardownMu sync.Mutex
	var tornDown bool
	spawn := func(fn func()) bool {
		teardownMu.Lock()
		if tornDown {
			teardownMu.Unlock()
			return false
		}
		wg.Add(1)
		teardownMu.Unlock()
		go func() {
			defer wg.Done()
			fn()
		}()
		return true
	}
	defer func() {
		teardownMu.Lock()
		tornDown = true
		teardownMu.Unlock()
		holdCancel()
		cancel()
		wg.Wait()
	}()
	profileSpec, err := parseStreamProfile(mediaProbeStreamProfile)
	if err != nil {
		return mediaProbeSummary{}, err
	}
	summary := mediaProbeSummary{EventCounts: make(map[string]int), StreamProfile: profileSpec.Name}
	var videoOutput *os.File
	if mediaProbeOutput != "" {
		var err error
		if mediaProbeOutput == "-" {
			videoOutput = os.Stdout
		} else {
			videoOutput, err = os.Create(mediaProbeOutput)
			if err != nil {
				return summary, fmt.Errorf("create H264 output: %w", err)
			}
			defer videoOutput.Close()
		}
	}
	signalURL, err := buildSignalURL(ticket)
	if err != nil {
		return summary, err
	}
	connection, response, err := websocket.DefaultDialer.DialContext(startup, signalURL, http.Header{})
	if err != nil {
		if response != nil {
			return summary, fmt.Errorf("WebSocket handshake failed with HTTP %d", response.StatusCode)
		}
		return summary, fmt.Errorf("WebSocket handshake failed: %w", err)
	}
	defer connection.Close()
	var startupTimedOut atomic.Bool
	stopStartup := context.AfterFunc(startup, func() {
		startupTimedOut.Store(true)
		if health != nil {
			health.Print("event=startup_timeout")
		}
		_ = connection.Close()
	})
	defer stopStartup()
	stopCtxClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCtxClose()
	summary.WebSocketConnected = true
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetReadDeadline(deadline)
	}

	var writeLock sync.Mutex
	writeSignal := func(message any) error {
		writeLock.Lock()
		defer writeLock.Unlock()
		_ = connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
		return connection.WriteJSON(message)
	}
	pingEvery := time.Duration(ticket.Data.SignalPingInterval) * time.Second
	if pingEvery <= 0 {
		pingEvery = 2 * time.Second
	}
	spawn(func() {
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
	})
	if err := writeSignal(buildAuthMessage(ticket)); err != nil {
		return summary, fmt.Errorf("send signal authentication: %w", err)
	}

	forceDirect := mediaProbeForceDirect || os.Getenv("VICO_ICE_FORCE_DIRECT") == "1"
	maxTurn := 2
	if envMax := os.Getenv("VICO_MAX_TURN_SERVERS"); envMax != "" {
		if v, err := strconv.Atoi(envMax); err == nil && v >= 0 {
			maxTurn = v
		}
	}
	iceServers := buildICEServers(ticket.Data.IceServer, forceDirect, maxTurn)
	if health != nil {
		health.Printf("event=ice_config force_direct=%t ice_servers=%d", forceDirect, len(iceServers))
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
	// Private header-only receive trace under the ICE agent; VICO_RX_TRACE=1 also writes it beside --health-log.
	rxPath := ""
	if os.Getenv("VICO_RX_TRACE") != "" && mediaProbeHealthLog != "" {
		rxPath = mediaProbeHealthLog + ".rx.csv"
	}
	rx := newRxTrace(rxPath)
	defer rx.close()
	var turnSpecs []string
	for _, s := range iceServers {
		for _, u := range s.URLs {
			u = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(u, "turns:"), "turn:"), "stun:")
			if idx := strings.IndexByte(u, '?'); idx >= 0 {
				u = u[:idx]
			}
			if u != "" {
				turnSpecs = append(turnSpecs, u)
			}
		}
	}
	rx.recordICEServers(turnSpecs)

	var lastNackLog atomic.Int64
	interceptors := &interceptor.Registry{}
	if err := configureLiveInterceptorsWithObserver(mediaEngine, interceptors, func(pkts []rtcp.Packet) {
		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.TransportLayerNack:
				var seqs []uint16
				for _, np := range p.Nacks {
					seqs = append(seqs, np.PacketList()...)
				}
				rx.observeFeedback("tx_nack", p.MediaSSRC, seqs)
				if health != nil && len(seqs) > 0 {
					nowNs := time.Now().UnixNano()
					prev := lastNackLog.Load()
					if prev == 0 || (nowNs-prev) >= int64(500*time.Millisecond) {
						if lastNackLog.CompareAndSwap(prev, nowNs) {
							health.Printf("event=nack_sent ssrc=%d count=%d first_seq=%d elapsed_ms=%d ts_utc=%s",
								p.MediaSSRC, len(seqs), seqs[0], time.Since(started).Milliseconds(), utcNowTag())
						}
					}
				}
			case *rtcp.PictureLossIndication:
				rx.observeFeedback("tx_pli", p.MediaSSRC, nil)
			}
		}
	}); err != nil {
		return summary, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetSRTPReplayProtectionWindow(liveRTPMaxLate)
	settingEngine.SetICEMaxBindingRequests(25)
	settingEngine.SetIPFilter(func(ip net.IP) bool {
		return !ip.IsLinkLocalUnicast() && !ip.IsLoopback()
	})
	if tn, err := newTracedNet(rx); err == nil {
		settingEngine.SetNet(tn)
	}
	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptors),
	)
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
	var pendingRemoteCandidates []webrtc.ICECandidateInit
	var videoPackets, audioPackets atomic.Uint64
	var videoBytes atomic.Uint64
	var recordedBytes atomic.Uint64
	var candidatesSent atomic.Uint64
	var channelOpen atomic.Bool
	sessionCtrl := newLiveSessionControl(serial, profileSpec, started, health)
	var postStartReady atomic.Bool
	var postStartBytes atomic.Uint64
	var postStartFirstVideoNs atomic.Int64
	var verifiedWidth, verifiedHeight atomic.Int64
	var streamVerifiedLogged atomic.Bool
	var iceConnectedUTC, dataChannelOpenUTC, firstFrameUTC atomic.Value
	calcPostKbps := func(now time.Time) float64 {
		t0 := postStartFirstVideoNs.Load()
		if t0 <= 0 {
			return 0
		}
		secs := now.Sub(time.Unix(0, t0)).Seconds()
		if secs < 0.25 {
			return 0
		}
		return float64(postStartBytes.Load()) * 8.0 / (secs * 1000.0)
	}
	dimLabel := func() string {
		if w, h := verifiedWidth.Load(), verifiedHeight.Load(); w > 0 && h > 0 {
			return fmt.Sprintf("%dx%d", w, h)
		}
		return "-"
	}
	var observationCompleted atomic.Bool
	var authenticated, peerOnline, answerAccepted atomic.Bool
	var recordDeadline atomic.Int64
	var firstVideoMillis, lastVideoMillis, largestVideoGapMillis, lastVideoSecond atomic.Int64
	var videoActiveSeconds atomic.Uint64
	var videoSamples, droppedVideoPackets, rtpErrs atomic.Uint64
	summary.CountersOnly = mediaProbeCountersOnly
	var diag rtpDiagTracker
	var loopStats loopTimings
	var nalMu sync.Mutex
	rtpNALTypes := make(map[uint8]uint64)
	outputNALTypes := make(map[uint8]uint64)
	firstVideoMillis.Store(-1)
	lastVideoMillis.Store(-1)
	lastVideoSecond.Store(-1)
	var connectionState atomic.Value
	var firstFrameMillis, lastFrameMillis, largestFrameGapMillis atomic.Int64
	var lastPeerOutMillis, iceDisconnectedMillis atomic.Int64
	var iceFailed, outputErrored atomic.Bool
	var outputFrames atomic.Uint64
	var pictureLossNegotiated atomic.Bool
	var labDropMillis, labPLIMillis, labIDRMillis atomic.Int64
	var labPreviousIDRMillis, labIDRIntervalMillis atomic.Int64
	firstFrameMillis.Store(-1)
	lastFrameMillis.Store(-1)
	lastPeerOutMillis.Store(-1)
	iceDisconnectedMillis.Store(-1)
	connectionState.Store("new")
	if health != nil {
		spawn(func() {
			tickInterval := time.Second
			if mediaProbeProgressEvery > 0 && mediaProbeProgressEvery < tickInterval {
				tickInterval = mediaProbeProgressEvery
			}
			ticker := time.NewTicker(tickInterval)
			defer ticker.Stop()
			lastProgress := time.Now()
			type rateSample struct {
				at    time.Time
				bytes uint64
			}
			var rateRing []rateSample
			var lastRateAt time.Time
			recentKbps := func(now time.Time) float64 {
				cur := postStartBytes.Load()
				if lastRateAt.IsZero() || now.Sub(lastRateAt) >= time.Second {
					lastRateAt = now
					rateRing = append(rateRing, rateSample{at: now, bytes: cur})
					cutoff := now.Add(-10 * time.Second)
					for len(rateRing) > 2 && rateRing[0].at.Before(cutoff) {
						rateRing = rateRing[1:]
					}
				}
				if len(rateRing) < 2 {
					return calcPostKbps(now)
				}
				span := now.Sub(rateRing[0].at).Seconds()
				if span < 1.0 || cur < rateRing[0].bytes {
					return calcPostKbps(now)
				}
				return float64(cur-rateRing[0].bytes) * 8.0 / (span * 1000.0)
			}
			for {
				select {
				case <-ctx.Done():
					_ = connection.Close()
					return
				case now := <-ticker.C:
					lastFrame := lastFrameMillis.Load()
					lastFrameUTC := "-"
					if lastFrame >= 0 {
						lastFrameUTC = started.Add(time.Duration(lastFrame) * time.Millisecond).UTC().Format(time.RFC3339Nano)
					}
					if mediaProbeProgressEvery > 0 && now.Sub(lastProgress) >= mediaProbeProgressEvery {
						lastProgress = now
						health.Printf("event=progress auth=%t peer=%t answer=%t channel=%t start=%t ack=%t packets=%d samples=%d drops=%d seq_gaps=%d seq_missing=%d seq_late=%d asm_drops=%d asm_late=%d frames=%d last_rtp_ms=%d last_frame_ms=%d last_frame_utc=%s max_rtp_gap_ms=%d max_frame_gap_ms=%d state=%s seq_dup=%d seq_unknown=%d profile=%s dim=%s post_kbps=%.1f inst_kbps=%.1f rtp_errs=%d %s %s",
							authenticated.Load(), peerOnline.Load(), answerAccepted.Load(), channelOpen.Load(), sessionCtrl.StartLiveSent(), sessionCtrl.startAck.Load(), videoPackets.Load(), videoSamples.Load(), droppedVideoPackets.Load(), diag.seqGaps.Load(), diag.seqMissing.Load(), diag.seqLate.Load(), diag.asmDrops.Load(), diag.asmLate.Load(), outputFrames.Load(), lastVideoMillis.Load(), lastFrame, lastFrameUTC, largestVideoGapMillis.Load(), largestFrameGapMillis.Load(), connectionState.Load(), diag.seqDup.Load(), diag.seqUnknown.Load(), profileSpec.Name, dimLabel(), calcPostKbps(now), recentKbps(now), rtpErrs.Load(), loopStats.snapshot(), rx.fields())
					}
					nowMs := now.Sub(started).Milliseconds()
					if mediaProbeCountersOnly && postStartReady.Load() && lastVideoMillis.Load() >= 0 && nowMs-lastVideoMillis.Load() >= 8000 {
						health.Printf("event=rtp_stall last_rtp_ms=%d packets=%d", lastVideoMillis.Load(), videoPackets.Load())
						_ = connection.Close()
						return
					}
					if reason, abort := shouldAbortDeadSession(nowMs, lastVideoMillis.Load(), lastFrame, lastPeerOutMillis.Load(), iceDisconnectedMillis.Load(), peerOnline.Load(), answerAccepted.Load(), iceFailed.Load(), videoPackets.Load()); abort {
						if reason == "output_stall" {
							rtpIdle := lastVideoMillis.Load() < 0 || nowMs-lastVideoMillis.Load() >= 5000
							health.Printf("event=output_stall last_rtp_ms=%d last_frame_ms=%d rtp_idle=%t", lastVideoMillis.Load(), lastFrame, rtpIdle)
						} else {
							health.Printf("event=%s last_rtp_ms=%d last_frame_ms=%d packets=%d frames=%d", reason, lastVideoMillis.Load(), lastFrame, videoPackets.Load(), outputFrames.Load())
						}
						_ = connection.Close()
						return
					}
				}
			}
		})
	}
	if health == nil && mediaProbeProgressEvery > 0 {
		spawn(func() {
			ticker := time.NewTicker(mediaProbeProgressEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					state, _ := connectionState.Load().(string)
					fmt.Fprintf(os.Stderr, "live probe: %.0fs elapsed_ms=%d auth=%t peer=%t answer=%t channel=%t start=%t ack=%t video=%d frames=%d state=%s\n",
						time.Since(started).Seconds(), time.Since(started).Milliseconds(), authenticated.Load(), peerOnline.Load(),
						answerAccepted.Load(), channelOpen.Load(), sessionCtrl.StartLiveSent(), sessionCtrl.startAck.Load(), videoPackets.Load(), outputFrames.Load(), state)
				}
			}
		})
	}
	shouldFinish := func() bool {
		if mediaProbeContinuous {
			return false
		}
		if mediaProbeObserveFor > 0 {
			return false // the observation timer closes the signal connection
		}
		if videoPackets.Load() < 100 || !sessionCtrl.StartLiveSent() {
			return false
		}
		if mediaProbeCountersOnly && !postStartReady.Load() {
			return false
		}
		if videoOutput != nil && time.Now().UnixNano() < recordDeadline.Load() {
			return false
		}
		return true
	}
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		connectionState.Store(state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			sessionCtrl.NotifyPeerClosed()
		}
	})
	peer.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		iceNow := utcNowTag()
		switch state {
		case webrtc.ICEConnectionStateFailed:
			iceFailed.Store(true)
			sessionCtrl.NotifyPeerClosed()
		case webrtc.ICEConnectionStateClosed:
			sessionCtrl.NotifyPeerClosed()
		case webrtc.ICEConnectionStateDisconnected:
			iceDisconnectedMillis.CompareAndSwap(-1, time.Since(started).Milliseconds())
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			iceDisconnectedMillis.Store(-1)
			iceFailed.Store(false)
			if v, _ := iceConnectedUTC.Load().(string); v == "" {
				iceConnectedUTC.Store(iceNow)
			}
		}
		if health != nil {
			var localICE, remoteICE string
			if peer.SCTP() != nil && peer.SCTP().Transport() != nil && peer.SCTP().Transport().ICETransport() != nil {
				if pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
					localICE, remoteICE = pair.Local.Typ.String(), pair.Remote.Typ.String()
				}
			}
			health.Printf("event=ice_state state=%s ice=%s/%s ts_utc=%s", state, localICE, remoteICE, iceNow)
		}
	})
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		spawn(func() {
			if track.Kind() != webrtc.RTPCodecTypeVideo {
				for {
					if _, _, err := track.ReadRTP(); err != nil {
						return
					}
					audioPackets.Add(1)
				}
			}
			if mediaProbeCountersOnly {
				drainVideoTrackCounters(&countersTrackDeps{
					ctx:                   ctx,
					track:                 track,
					peer:                  peer,
					closeSignal:           func() { _ = connection.Close() },
					started:               started,
					profileSpec:           profileSpec,
					postStartReady:        &postStartReady,
					postStartFirstVideoNs: &postStartFirstVideoNs,
					postStartBytes:        &postStartBytes,
					verifiedWidth:         &verifiedWidth,
					verifiedHeight:        &verifiedHeight,
					streamVerifiedLogged:  &streamVerifiedLogged,
					pictureLossNegotiated: &pictureLossNegotiated,
					observationCompleted:  &observationCompleted,
					videoPackets:          &videoPackets,
					videoBytes:            &videoBytes,
					firstVideoMillis:      &firstVideoMillis,
					lastVideoMillis:       &lastVideoMillis,
					largestVideoGapMillis: &largestVideoGapMillis,
					lastVideoSecond:       &lastVideoSecond,
					videoActiveSeconds:    &videoActiveSeconds,
					rtpErrs:               &rtpErrs,
					diag:                  &diag,
					loopStats:             &loopStats,
					rx:                    rx,
					nalMu:                 &nalMu,
					rtpNALTypes:           rtpNALTypes,
					stopStartup:           func() { stopStartup() },
					onFirstFrame:          onFirstFrame,
					spawn:                 spawn,
					health:                health,
					observeFor:            mediaProbeObserveFor,
				})
				return
			}
			liveRecovery := mediaProbeContinuous || mediaProbeLabDropAfter > 0 || mediaProbeObserveFor > 0 || videoOutput != nil
			// Main-stream IDR frames can span far more than 50 RTP packets.
			// Use the frontier-guarded assembler on live paths so stale duplicates cannot
			// rewind the window and complete IDRs are not trapped behind obsolete gaps.
			var builder *samplebuilder.SampleBuilder
			var asm *liveH264Assembler
			if liveRecovery {
				asm = newLiveH264Assembler(&diag, liveEnvMillis("VICO_REASSEMBLY_WINDOW_MS", liveRTPReorderWindow))
			} else {
				builder = samplebuilder.New(liveRTPMaxLate, &codecs.H264Packet{}, 90000,
					samplebuilder.WithRTPHeaders(true),
					samplebuilder.WithPacketReleaseHandler(diag.onBuilderRelease))
			}
			var recovery h264LiveRecovery
			recovery.gate.configureProfile(profileSpec, &postStartReady)
			recovery.stall = liveEnvMillis("VICO_FRAME_STALL_MS", 0)
			stallMs := recovery.stall.Milliseconds()
			if stallMs == 0 {
				stallMs = liveFrameStall.Milliseconds()
			}
			if health != nil {
				health.Printf("event=reassembly window_ms=%d stall_ms=%d", liveEnvMillis("VICO_REASSEMBLY_WINDOW_MS", liveRTPReorderWindow).Milliseconds(), stallMs)
			}
			var firstFrame, previousIDR, lastPLI time.Time
			var idrInterval time.Duration
			var injectedSequence uint16
			var injectedTimestamp uint32
			pliNegotiated := false
			for _, feedback := range track.Codec().RTCPFeedback {
				pliNegotiated = pliNegotiated || (feedback.Type == "nack" && feedback.Parameter == "pli")
			}
			pictureLossNegotiated.Store(pliNegotiated)
			sendPLI := func(now time.Time, eventName string) {
				if !pliNegotiated {
					return
				}
				if mediaProbeLabDropAfter == 0 && !lastPLI.IsZero() && now.Sub(lastPLI) < 2500*time.Millisecond {
					if health != nil && eventName == "keyframe_request" {
						health.Printf("event=keyframe_request_throttled elapsed_since_last_ms=%d", now.Sub(lastPLI).Milliseconds())
					}
					return
				}
				lastPLI = now
				err := peer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if health != nil {
					health.Printf("event=%s sent=%t", eventName, err == nil)
				}
				if err == nil && labDropMillis.Load() > 0 && labIDRMillis.Load() == 0 {
					labPLIMillis.CompareAndSwap(0, now.Sub(started).Milliseconds())
				}
			}
			reportGap := func(now time.Time, reason string) {
				if health != nil {
					health.Printf("event=video_gap reason=%s last_frame_ms=%d detected_ms=%d", reason, lastFrameMillis.Load(), now.Sub(started).Milliseconds())
				}
				sendPLI(now, "keyframe_request")
			}
			writeSamples := func() {
				nextSample := func() *media.Sample {
					if asm != nil {
						return asm.Pop(time.Now(), recovery.recovering || !recovery.gate.started)
					}
					return builder.Pop()
				}
				for sample := nextSample(); sample != nil; sample = nextSample() {
					videoSamples.Add(1)
					droppedVideoPackets.Add(uint64(sample.PrevDroppedPackets))
					lateSample := recovery.hasTimestamp && int32(sample.PacketTimestamp-recovery.lastRTP) < 0
					completeSample := len(sample.RTPHeaders) > 0 && sample.RTPHeaders[len(sample.RTPHeaders)-1].Marker && len(sample.Data) > 0
					diag.onSamplePopped(sample, lateSample || !completeSample, lateSample)
					nalMu.Lock()
					countAnnexBNALTypes(sample.Data, outputNALTypes)
					nalMu.Unlock()
					data := sample.Data
					var frame, recovered bool
					if liveRecovery {
						now := time.Now()
						var gapStarted bool
						data, frame, gapStarted, recovered = recovery.push(sample)
						if gapStarted {
							reportGap(now, "incomplete_rtp")
						}
					} else if sample.PrevDroppedPackets == 0 && len(sample.Data) > 0 {
						_, frame = recovery.gate.pushFrame(sample.Data)
					}
					if videoOutput != nil && len(data) > 0 {
						tWrite := time.Now()
						count, err := videoOutput.Write(data)
						wd := time.Since(tWrite)
						loopStats.observe(&loopStats.write, wd)
						if wd > 50*time.Millisecond {
							loopStats.slowWrite.Add(1)
						}
						if err == nil {
							recordedBytes.Add(uint64(count))
						} else {
							outputErrored.Store(true)
							if health != nil {
								health.Print("event=output_error")
								_ = connection.Close()
							}
							return
						}
					}
					if frame && (videoOutput == nil || len(data) > 0) {
						now := time.Now()
						recovery.lastFrame = now
						if recovery.gate.lastSPS.HasDimensions {
							verifiedWidth.Store(int64(recovery.gate.lastSPS.Width))
							verifiedHeight.Store(int64(recovery.gate.lastSPS.Height))
							if health != nil && streamVerifiedLogged.CompareAndSwap(false, true) {
								health.Printf("event=stream_verified profile=%s dim=%dx%d sps_profile=%d sps_level=%d post_start=%t",
									profileSpec.Name, recovery.gate.lastSPS.Width, recovery.gate.lastSPS.Height,
									recovery.gate.lastSPS.ProfileIDC, recovery.gate.lastSPS.LevelIDC, postStartReady.Load())
							}
						}
						if len(sample.RTPHeaders) > 0 {
							if age, ok := rx.lag(sample.RTPHeaders[0].SSRC, sample.RTPHeaders[0].SequenceNumber, now); ok {
								loopStats.observe(&loopStats.frameAge, age)
							}
						}
						elapsed := now.Sub(started).Milliseconds()
						previous := lastFrameMillis.Swap(elapsed)
						outputFrames.Add(1)
						if previous < 0 {
							firstFrame = now
							ffUTC := now.UTC().Format(time.RFC3339Nano)
							firstFrameUTC.Store(ffUTC)
							firstFrameMillis.Store(elapsed)
							stopStartup()
							if onFirstFrame != nil {
								onFirstFrame()
							}
							if health != nil {
								var localICE, remoteICE string
								if peer.SCTP() != nil && peer.SCTP().Transport() != nil && peer.SCTP().Transport().ICETransport() != nil {
									if pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
										localICE, remoteICE = pair.Local.Typ.String(), pair.Remote.Typ.String()
									}
								}
								health.Printf("event=first_frame elapsed_ms=%d first_rtp_ms=%d ice=%s/%s profile=%s dim=%s ts_utc=%s",
									elapsed, firstVideoMillis.Load(), localICE, remoteICE, profileSpec.Name, dimLabel(), ffUTC)
							}
							if mediaProbeObserveFor > 0 {
								spawn(func() {
									select {
									case <-time.After(mediaProbeObserveFor):
										observationCompleted.Store(true)
										_ = connection.Close()
									case <-ctx.Done():
										return
									}
								})
							}
						} else if elapsed-previous > largestFrameGapMillis.Load() {
							largestFrameGapMillis.Store(elapsed - previous)
						}
						if mediaProbeLabDropAfter > 0 && labDropMillis.Load() == 0 {
							for _, nalu := range annexBNALUs(data) {
								if nalu[0]&0x1f == 5 {
									if !previousIDR.IsZero() {
										idrInterval = now.Sub(previousIDR)
									}
									previousIDR = now
									break
								}
							}
						}
						if recovered {
							if health != nil {
								health.Printf("event=video_recovered frame_ms=%d gap_ms=%d", elapsed, elapsed-previous)
							}
							if labDropMillis.Load() > 0 {
								labIDRMillis.CompareAndSwap(0, elapsed)
							}
						}
					}
				}
				diag.afterPopDrain()
			}
			var prevReturn time.Time
			for {
				tCall := time.Now()
				if !prevReturn.IsZero() {
					loopStats.observe(&loopStats.busy, tCall.Sub(prevReturn))
				}
				packet, _, err := track.ReadRTP()
				tReturn := time.Now()
				prevReturn = tReturn
				if err != nil {
					recordReadRTPError(err, ctx, &observationCompleted, &rtpErrs, health)
					if !liveRecovery {
						builder.Flush()
						writeSamples()
					}
					return
				}
				loopStats.observe(&loopStats.readWait, tReturn.Sub(tCall))
				if lag, ok := rx.lag(packet.SSRC, packet.SequenceNumber, tReturn); ok {
					loopStats.observe(&loopStats.readLag, lag)
				}
				if len(packet.Payload) > 0 {
					nalMu.Lock()
					rtpNALTypes[packet.Payload[0]&0x1f]++
					nalMu.Unlock()
				}
				now := time.Now()
				elapsed := now.Sub(started).Milliseconds()
				firstVideoMillis.CompareAndSwap(-1, elapsed)
				previous := lastVideoMillis.Swap(elapsed)
				if previous >= 0 && elapsed-previous > largestVideoGapMillis.Load() {
					largestVideoGapMillis.Store(elapsed - previous)
				}
				if lastVideoSecond.Swap(elapsed/1000) != elapsed/1000 {
					videoActiveSeconds.Add(1)
				}
				videoPackets.Add(1)
				pktBytes := uint64(len(packet.Payload))
				videoBytes.Add(pktBytes)
				if postStartReady.Load() {
					postStartFirstVideoNs.CompareAndSwap(0, now.UnixNano())
					postStartBytes.Add(pktBytes)
				}
				if liveRecovery && recovery.stalled(now) {
					if asm != nil {
						asm.PruneOnStall(now)
					} else {
						diag.onBuilderReset()
						builder = newLiveH264BuilderWithDiag(&diag)
					}
					reportGap(now, "frame_stall")
				} else if liveRecovery && recovery.recovering && pliNegotiated && !lastPLI.IsZero() && now.Sub(lastPLI) >= 2500*time.Millisecond {
					sendPLI(now, "keyframe_retry")
				}
				if mediaProbeLabDropAfter > 0 && labDropMillis.Load() == 0 && recovery.gate.started &&
					idrInterval > 0 && previousIDR.Sub(firstFrame) >= mediaProbeLabDropAfter && now.Sub(previousIDR) <= 300*time.Millisecond && len(packet.Payload) >= 2 &&
					(packet.Payload[0]&0x1f == 1 || (packet.Payload[0]&0x1f == 28 && packet.Payload[1]&0x1f == 1)) {
					injectedSequence, injectedTimestamp = packet.SequenceNumber, packet.Timestamp
					labPreviousIDRMillis.Store(previousIDR.Sub(started).Milliseconds())
					labIDRIntervalMillis.Store(idrInterval.Milliseconds())
					labDropMillis.Store(elapsed)
				}
				if labDropMillis.Load() > 0 && packet.SequenceNumber == injectedSequence && packet.Timestamp == injectedTimestamp {
					continue
				}
				tPush := time.Now()
				diag.onInputPacket(packet)
				if asm != nil {
					asm.Push(packet, now)
				} else {
					builder.Push(packet)
				}
				loopStats.observe(&loopStats.push, time.Since(tPush))
				tPop := time.Now()
				writeSamples()
				loopStats.observe(&loopStats.pop, time.Since(tPop))
				if shouldFinish() {
					_ = connection.Close()
				}
			}
		})
	})
	channel, err := peer.CreateDataChannel(serial, nil)
	if err != nil {
		return summary, fmt.Errorf("create camera data channel: %w", err)
	}
	sessionCtrl.AttachChannel(channel)
	defer sessionCtrl.Shutdown(holdCancel, func() {
		teardownMu.Lock()
		tornDown = true
		teardownMu.Unlock()
		cancel()
		_ = peer.Close()
		_ = connection.Close()
		wg.Wait()
	})
	channel.OnClose(func() {
		sessionCtrl.NotifyChannelClosed()
	})
	channel.OnOpen(func() {
		channelOpen.Store(true)
		dcUTC := utcNowTag()
		dataChannelOpenUTC.Store(dcUTC)
		if health != nil {
			health.Printf("event=channel_open elapsed_ms=%d ts_utc=%s", time.Since(started).Milliseconds(), dcUTC)
		}
		sendStartLive := func() {
			if sessionCtrl.SendStartLive() && videoOutput != nil {
				recordDeadline.Store(time.Now().Add(mediaProbeRecordFor).UnixNano())
			}
		}
		if mediaProbeHoldFile == "" {
			sendStartLive()
			return
		}
		// Harness hold: stay connected without requesting video until released.
		rx.flushEndpoints()
		if health != nil {
			health.Printf("event=hold_ready elapsed_ms=%d", time.Since(started).Milliseconds())
		}
		spawn(func() {
			for {
				if _, err := os.Stat(mediaProbeHoldFile); err == nil {
					break
				}
				select {
				case <-holdCtx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
			if health != nil {
				health.Printf("event=hold_released elapsed_ms=%d", time.Since(started).Milliseconds())
			}
			sendStartLive()
		})
	})
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if sessionCtrl.OnControlMessage(message.Data) {
			postStartReady.Store(true)
			if health != nil {
				health.Printf("event=start_live_ack profile=%s elapsed_ms=%d ts_utc=%s", profileSpec.Name, time.Since(started).Milliseconds(), utcNowTag())
			}
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
		if health != nil {
			health.Printf("event=local_candidate proto=%s typ=%s port=%d", candidate.Protocol, candidate.Typ, candidate.Port)
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
				return finishMediaSummary(summary, started, &videoPackets, &audioPackets, &channelOpen, &sessionCtrl.startSent, &connectionState), nil
			}
		case "PEER_IN":
			lastPeerOutMillis.Store(-1)
			if peerPresent {
				break
			}
			peerPresent = true
			summary.PeerOnline = true
			peerOnline.Store(true)
			if health != nil {
				health.Printf("event=peer_in elapsed_ms=%d", time.Since(started).Milliseconds())
			}
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
			lastPeerOutMillis.CompareAndSwap(-1, time.Since(started).Milliseconds())
			if health != nil {
				health.Printf("event=peer_out elapsed_ms=%d answer=%t packets=%d frames=%d", time.Since(started).Milliseconds(), answerAccepted.Load(), videoPackets.Load(), outputFrames.Load())
			}
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
			sdp := description.SDP
			if forceDirect {
				var filtered []string
				for _, line := range strings.Split(sdp, "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "a=candidate:") && strings.Contains(trimmed, " typ relay") {
						if health != nil {
							health.Printf("event=remote_candidate_skipped typ=relay reason=force_direct")
						}
						continue
					}
					filtered = append(filtered, line)
				}
				sdp = strings.Join(filtered, "\n")
			}
			if health != nil {
				for _, line := range strings.Split(sdp, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "a=candidate:") {
						fields := strings.Fields(line)
						typ := ""
						for i, f := range fields {
							if f == "typ" && i+1 < len(fields) {
								typ = fields[i+1]
								break
							}
						}
						health.Printf("event=remote_candidate typ=%s", typ)
					}
				}
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
				return summary, fmt.Errorf("set remote SDP answer: %w", err)
			}
			summary.AnswerAccepted = true
			answerAccepted.Store(true)
			if health != nil {
				health.Printf("event=answer_received elapsed_ms=%d", time.Since(started).Milliseconds())
			}
			candidateLock.Lock()
			for _, pending := range pendingRemoteCandidates {
				_ = peer.AddICECandidate(pending)
			}
			pendingRemoteCandidates = nil
			candidateLock.Unlock()
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
				if forceDirect && strings.Contains(init.Candidate, " typ relay") {
					if health != nil {
						health.Printf("event=remote_candidate_skipped typ=relay reason=force_direct")
					}
					continue
				}
				candidateLock.Lock()
				if answerAccepted.Load() {
					_ = peer.AddICECandidate(init)
				} else {
					pendingRemoteCandidates = append(pendingRemoteCandidates, init)
				}
				candidateLock.Unlock()
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
	summary.FirstFrameMillis = firstFrameMillis.Load()
	summary.LastVideoMillis = lastVideoMillis.Load()
	summary.LargestVideoGapMs = largestVideoGapMillis.Load()
	summary.VideoActiveSeconds = videoActiveSeconds.Load()
	summary.VideoSamples = videoSamples.Load()
	summary.DroppedVideoPkts = droppedVideoPackets.Load()
	summary.RTPSeqGaps = diag.seqGaps.Load()
	summary.RTPSeqMissing = diag.seqMissing.Load()
	summary.RTPSeqLate = diag.seqLate.Load()
	summary.AssemblyDroppedPkts = diag.asmDrops.Load()
	summary.AssemblyLatePkts = diag.asmLate.Load()
	summary.PictureLossNegotiated = pictureLossNegotiated.Load()
	summary.LabDropMillis = labDropMillis.Load()
	summary.LabPLIMillis = labPLIMillis.Load()
	summary.LabIDRMillis = labIDRMillis.Load()
	summary.LabPreviousIDRMillis = labPreviousIDRMillis.Load()
	summary.LabIDRIntervalMillis = labIDRIntervalMillis.Load()
	summary.OutputFrames = outputFrames.Load()
	summary.LargestFrameGapMs = largestFrameGapMillis.Load()
	nalMu.Lock()
	summary.RTPNALTypes = maps.Clone(rtpNALTypes)
	summary.OutputNALTypes = maps.Clone(outputNALTypes)
	nalMu.Unlock()
	if summary.LastVideoMillis >= 0 {
		summary.VideoIdleAtEndMs = time.Since(started).Milliseconds() - summary.LastVideoMillis
	} else {
		summary.VideoIdleAtEndMs = time.Since(started).Milliseconds()
	}
	if ctx.Err() != nil && mediaProbeMaxDuration > 0 && !startupTimedOut.Load() {
		summary.EndReason = "max_duration"
	} else if ctx.Err() != nil || startupTimedOut.Load() {
		summary.EndReason = "timeout"
	} else if observationCompleted.Load() {
		summary.EndReason = "observation_complete"
	} else if summary.SignalClosed {
		summary.EndReason = "signal_closed"
	} else {
		summary.EndReason = "probe_complete"
	}
	summary.StartLiveAck = sessionCtrl.startAck.Load()
	summary.VerifiedWidth = int(verifiedWidth.Load())
	summary.VerifiedHeight = int(verifiedHeight.Load())
	summary.PostStartBytes = postStartBytes.Load()
	summary.PostStartBitrateKbps = math.Round(calcPostKbps(time.Now())*10) / 10
	summary.RXErrors = rx.readErrs.Load()
	summary.RXTimeouts = rx.readTimeouts.Load()
	summary.RTPErrors = rtpErrs.Load()
	if lastRXErr := rx.lastErr(); lastRXErr != "-" {
		summary.RXLastError = lastRXErr
	}
	summary.ICEConnectedUTC, _ = iceConnectedUTC.Load().(string)
	summary.DataChannelOpenUTC, _ = dataChannelOpenUTC.Load().(string)
	summary.FirstFrameUTC, _ = firstFrameUTC.Load().(string)
	if peer.SCTP() != nil && peer.SCTP().Transport() != nil && peer.SCTP().Transport().ICETransport() != nil {
		if pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			summary.LocalCandidateType = pair.Local.Typ.String()
			summary.RemoteCandidateType = pair.Remote.Typ.String()
		}
	}
	if health != nil {
		health.Printf("event=session_end reason=%s offers=%d join_code=%d answer=%t start_ack=%t packets=%d samples=%d drops=%d seq_gaps=%d seq_missing=%d seq_late=%d asm_drops=%d asm_late=%d frames=%d last_frame_ms=%d max_rtp_gap_ms=%d max_frame_gap_ms=%d profile=%s dim=%s post_kbps=%.1f ice=%s/%s rtp_errs=%d %s",
			summary.EndReason, summary.OfferAttempts, summary.JoinCode, summary.AnswerAccepted, summary.StartLiveAck, videoPackets.Load(), summary.VideoSamples, summary.DroppedVideoPkts, summary.RTPSeqGaps, summary.RTPSeqMissing, summary.RTPSeqLate, summary.AssemblyDroppedPkts, summary.AssemblyLatePkts, outputFrames.Load(), lastFrameMillis.Load(), summary.LargestVideoGapMs, largestFrameGapMillis.Load(), profileSpec.Name, dimLabel(), calcPostKbps(time.Now()), summary.LocalCandidateType, summary.RemoteCandidateType, summary.RTPErrors, rx.fields())
	}
	if outputErrored.Load() {
		return finishMediaSummary(summary, started, &videoPackets, &audioPackets, &channelOpen, &sessionCtrl.startSent, &connectionState), errOutputClosed
	}
	return finishMediaSummary(summary, started, &videoPackets, &audioPackets, &channelOpen, &sessionCtrl.startSent, &connectionState), nil
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

var (
	ipv4ErrRegexp = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	ipv6ErrRegexp = regexp.MustCompile(`\[[0-9A-Fa-f:]+\](?::\d+)?`)
)

func openProbeHealthLog(path string) (*log.Logger, func(), error) {
	health := log.New(os.Stderr, "", log.LstdFlags|log.LUTC|log.Lmicroseconds)
	if path == "" {
		return health, func() {}, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open private health log: %w", err)
	}
	health.SetOutput(file)
	return health, func() { _ = file.Close() }, nil
}

func sanitizeErrTag(text string) string {
	text = ipv4ErrRegexp.ReplaceAllString(text, "[ip]")
	text = ipv6ErrRegexp.ReplaceAllString(text, "[ip]")
	text = strings.TrimSpace(text)
	text = strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			return '_'
		case r < 0x20 || r > 0x7e:
			return -1
		default:
			return r
		}
	}, text)
	if text == "" {
		return "unknown"
	}
	if len(text) > 80 {
		text = text[:80]
	}
	return text
}

func classifySocketReadErr(err error) (closed, timeout bool, tag string) {
	if err == nil {
		return false, false, ""
	}
	if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
		return true, false, ""
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, true, "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			return false, false, sanitizeErrTag(fmt.Sprintf("%s_%s:%v", opErr.Op, sysErr.Syscall, sysErr.Err))
		}
		return false, false, sanitizeErrTag(fmt.Sprintf("%s:%v", opErr.Op, opErr.Err))
	}
	return false, false, sanitizeErrTag(err.Error())
}

func classifySocketErrTag(err error) string {
	closed, timeout, tag := classifySocketReadErr(err)
	if closed {
		return "closed"
	}
	if timeout {
		return "timeout"
	}
	return tag
}

func recordReadRTPError(err error, ctx context.Context, observationCompleted *atomic.Bool, rtpErrs *atomic.Uint64, health *log.Logger) {
	if err == nil || ctx.Err() != nil || observationCompleted.Load() || errors.Is(err, io.EOF) {
		return
	}
	closed, _, tag := classifySocketReadErr(err)
	if closed {
		return
	}
	rtpErrs.Add(1)
	if health != nil {
		health.Printf("event=rtp_err err=%s", tag)
	}
}

func selectedICEPairLabels(peer *webrtc.PeerConnection) (string, string) {
	if peer == nil || peer.SCTP() == nil || peer.SCTP().Transport() == nil || peer.SCTP().Transport().ICETransport() == nil {
		return "", ""
	}
	pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "", ""
	}
	return pair.Local.Typ.String(), pair.Remote.Typ.String()
}

func trackHasPLI(track *webrtc.TrackRemote) bool {
	if track == nil {
		return false
	}
	for _, feedback := range track.Codec().RTCPFeedback {
		if feedback.Type == "nack" && feedback.Parameter == "pli" {
			return true
		}
	}
	return false
}

func inspectRTPVideoPayload(payload []byte, counts map[uint8]uint64, onSPS func([]byte)) {
	if len(payload) == 0 {
		return
	}
	nalType := payload[0] & 0x1f
	switch {
	case nalType >= 1 && nalType <= 23:
		counts[nalType]++
		if nalType == 7 {
			onSPS(payload)
		}
	case nalType == 24:
		for offset := 1; offset+2 <= len(payload); {
			size := int(payload[offset])<<8 | int(payload[offset+1])
			offset += 2
			if size <= 0 || offset+size > len(payload) {
				break
			}
			nalu := payload[offset : offset+size]
			typ := nalu[0] & 0x1f
			counts[typ]++
			if typ == 7 {
				onSPS(nalu)
			}
			offset += size
		}
	case nalType == 28 && len(payload) >= 2:
		if payload[1]&0x80 != 0 {
			counts[payload[1]&0x1f]++
		}
	}
}

type countersTrackDeps struct {
	ctx                   context.Context
	track                 *webrtc.TrackRemote
	peer                  *webrtc.PeerConnection
	closeSignal           func()
	started               time.Time
	profileSpec           streamProfileSpec
	postStartReady        *atomic.Bool
	postStartFirstVideoNs *atomic.Int64
	postStartBytes        *atomic.Uint64
	verifiedWidth         *atomic.Int64
	verifiedHeight        *atomic.Int64
	streamVerifiedLogged  *atomic.Bool
	pictureLossNegotiated *atomic.Bool
	observationCompleted  *atomic.Bool
	videoPackets          *atomic.Uint64
	videoBytes            *atomic.Uint64
	firstVideoMillis      *atomic.Int64
	lastVideoMillis       *atomic.Int64
	largestVideoGapMillis *atomic.Int64
	lastVideoSecond       *atomic.Int64
	videoActiveSeconds    *atomic.Uint64
	rtpErrs               *atomic.Uint64
	diag                  *rtpDiagTracker
	loopStats             *loopTimings
	rx                    *rxTrace
	nalMu                 *sync.Mutex
	rtpNALTypes           map[uint8]uint64
	stopStartup           func()
	onFirstFrame          func()
	spawn                 func(func()) bool
	health                *log.Logger
	observeFor            time.Duration
}

func drainVideoTrackCounters(d *countersTrackDeps) {
	pliNegotiated := trackHasPLI(d.track)
	d.pictureLossNegotiated.Store(pliNegotiated)
	if d.health != nil {
		d.health.Printf("event=counters_only pli=%t profile=%s", pliNegotiated, d.profileSpec.Name)
	}
	var gate h264LiveGate
	gate.configureProfile(d.profileSpec, d.postStartReady)
	var matchedSPS atomic.Bool
	var readyOnce sync.Once
	var lastPLI time.Time
	var prevReturn time.Time

	for {
		tCall := time.Now()
		if !prevReturn.IsZero() {
			d.loopStats.observe(&d.loopStats.busy, tCall.Sub(prevReturn))
		}
		packet, _, err := d.track.ReadRTP()
		tReturn := time.Now()
		prevReturn = tReturn
		if err != nil {
			recordReadRTPError(err, d.ctx, d.observationCompleted, d.rtpErrs, d.health)
			return
		}
		d.loopStats.observe(&d.loopStats.readWait, tReturn.Sub(tCall))
		if lag, ok := d.rx.lag(packet.SSRC, packet.SequenceNumber, tReturn); ok {
			d.loopStats.observe(&d.loopStats.readLag, lag)
		}

		prevGaps := d.diag.seqGaps.Load()
		d.diag.onInputPacket(packet)
		if d.diag.seqGaps.Load() > prevGaps && pliNegotiated && d.postStartReady.Load() {
			if lastPLI.IsZero() || tReturn.Sub(lastPLI) >= 2500*time.Millisecond {
				lastPLI = tReturn
				err := d.peer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(d.track.SSRC())}})
				if d.health != nil {
					if err != nil {
						d.health.Printf("event=keyframe_request ssrc=%d err=write", uint32(d.track.SSRC()))
					} else {
						d.health.Printf("event=keyframe_request ssrc=%d", uint32(d.track.SSRC()))
					}
				}
			}
		}

		elapsed := tReturn.Sub(d.started).Milliseconds()
		d.videoPackets.Add(1)
		payloadLen := uint64(len(packet.Payload))
		d.videoBytes.Add(payloadLen)
		d.firstVideoMillis.CompareAndSwap(-1, elapsed)
		if prev := d.lastVideoMillis.Swap(elapsed); prev >= 0 {
			gap := elapsed - prev
			for {
				cur := d.largestVideoGapMillis.Load()
				if gap <= cur || d.largestVideoGapMillis.CompareAndSwap(cur, gap) {
					break
				}
			}
		}
		sec := elapsed / 1000
		for {
			prevSec := d.lastVideoSecond.Load()
			if sec <= prevSec {
				break
			}
			if d.lastVideoSecond.CompareAndSwap(prevSec, sec) {
				d.videoActiveSeconds.Add(1)
				break
			}
		}

		postReady := d.postStartReady.Load()
		if postReady {
			d.postStartFirstVideoNs.CompareAndSwap(0, tReturn.UnixNano())
			d.postStartBytes.Add(payloadLen)
		}

		d.nalMu.Lock()
		inspectRTPVideoPayload(packet.Payload, d.rtpNALTypes, func(nalu []byte) {
			if sps, ok := parseH264SPS(nalu); ok && gate.matchesSPS(sps) && (!gate.requirePostStart || postReady) {
				matchedSPS.Store(true)
				if sps.HasDimensions {
					d.verifiedWidth.Store(int64(sps.Width))
					d.verifiedHeight.Store(int64(sps.Height))
					if d.health != nil && d.streamVerifiedLogged.CompareAndSwap(false, true) {
						d.health.Printf("event=stream_verified profile=%s dim=%dx%d sps_profile=%d sps_level=%d post_start=%t",
							d.profileSpec.Name, sps.Width, sps.Height, sps.ProfileIDC, sps.LevelIDC, postReady)
					}
				}
			}
		})
		d.nalMu.Unlock()

		if postReady && matchedSPS.Load() {
			readyOnce.Do(func() {
				d.stopStartup()
				if d.onFirstFrame != nil {
					d.onFirstFrame()
				}
				if d.health != nil {
					localICE, remoteICE := selectedICEPairLabels(d.peer)
					dim := "-"
					if w, h := d.verifiedWidth.Load(), d.verifiedHeight.Load(); w > 0 && h > 0 {
						dim = fmt.Sprintf("%dx%d", w, h)
					}
					d.health.Printf("event=counters_ready elapsed_ms=%d first_rtp_ms=%d ice=%s/%s profile=%s dim=%s ts_utc=%s",
						elapsed, d.firstVideoMillis.Load(), localICE, remoteICE, d.profileSpec.Name, dim, tReturn.UTC().Format(time.RFC3339Nano))
				}
				if d.observeFor > 0 {
					d.spawn(func() {
						timer := time.NewTimer(d.observeFor)
						defer timer.Stop()
						select {
						case <-d.ctx.Done():
						case <-timer.C:
							d.observationCompleted.Store(true)
							if d.health != nil {
								d.health.Printf("event=observe_complete elapsed_ms=%d packets=%d post_bytes=%d ts_utc=%s",
									time.Since(d.started).Milliseconds(), d.videoPackets.Load(), d.postStartBytes.Load(), utcNowTag())
							}
							d.closeSignal()
						}
					})
				}
			})
		}
	}
}
