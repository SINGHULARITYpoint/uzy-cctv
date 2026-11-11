package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

const defaultStopLiveWait = 200 * time.Millisecond

type liveCommandAckPayload struct {
	Action      string `json:"action"`
	ReturnVal   *int   `json:"returnVal"`
	ReturnValue *int   `json:"returnValue"`
}

func parseStartLiveAck(data []byte) bool {
	var msg liveCommandAckPayload
	if json.Unmarshal(data, &msg) != nil || msg.Action != "startLive" {
		return false
	}
	if msg.ReturnValue != nil {
		return *msg.ReturnValue == 0
	}
	if msg.ReturnVal != nil {
		return *msg.ReturnVal == 0
	}
	return false
}

func parseStopLiveAck(data []byte) bool {
	var msg liveCommandAckPayload
	if json.Unmarshal(data, &msg) != nil || msg.Action != "stopLive" {
		return false
	}
	return true
}

func newLiveConnectionID() string {
	return fmt.Sprintf("%07x", rand.Uint32()&0xfffffff)
}

func buildLiveCommand(action, connectionID, targetID string, extra map[string]any) ([]byte, error) {
	stamp := time.Now()
	payload := map[string]any{
		"requestID":    fmt.Sprintf("%d-%d", stamp.UnixMilli(), rand.Intn(1000)),
		"connectionID": connectionID,
		"timeStamp":    stamp.Unix(),
		"action":       action,
	}
	if targetID != "" {
		payload["targetId"] = targetID
	}
	for k, v := range extra {
		payload[k] = v
	}
	return json.Marshal(payload)
}

type liveDataChannel interface {
	ReadyState() webrtc.DataChannelState
	SendText(string) error
	Close() error
}

// liveSessionControl serializes per-camera startLive / stopLive DataChannel commands
// and performs idempotent, bounded teardown before PeerConnection/WebSocket close.
type liveSessionControl struct {
	mu           sync.Mutex
	serial       string
	profile      streamProfileSpec
	connectionID string
	started      time.Time
	health       *log.Logger
	stopTimeout  time.Duration

	channel           liveDataChannel
	closing           bool
	startSent         atomic.Bool
	startAck          atomic.Bool
	stopSent          atomic.Bool
	stopAckCh         chan struct{}
	stopAckOnce       sync.Once
	channelClosedCh   chan struct{}
	channelClosedOnce sync.Once
	peerClosedCh      chan struct{}
	peerClosedOnce    sync.Once
	shutdownOnce      sync.Once
}

func newLiveSessionControl(serial string, profile streamProfileSpec, started time.Time, health *log.Logger) *liveSessionControl {
	return &liveSessionControl{
		serial:          serial,
		profile:         profile,
		connectionID:    newLiveConnectionID(),
		started:         started,
		health:          health,
		stopTimeout:     defaultStopLiveWait,
		stopAckCh:       make(chan struct{}),
		channelClosedCh: make(chan struct{}),
		peerClosedCh:    make(chan struct{}),
	}
}

func (c *liveSessionControl) AttachChannel(ch liveDataChannel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channel = ch
}

func (c *liveSessionControl) SetStopTimeout(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopTimeout = d
}

func (c *liveSessionControl) ConnectionID() string {
	return c.connectionID
}

func (c *liveSessionControl) StartLiveSent() bool {
	return c.startSent.Load()
}

func (c *liveSessionControl) StopLiveSent() bool {
	return c.stopSent.Load()
}

func (c *liveSessionControl) NotifyChannelClosed() {
	c.channelClosedOnce.Do(func() {
		close(c.channelClosedCh)
	})
}

func (c *liveSessionControl) NotifyPeerClosed() {
	c.peerClosedOnce.Do(func() {
		close(c.peerClosedCh)
	})
}

func (c *liveSessionControl) OnControlMessage(data []byte) bool {
	if parseStopLiveAck(data) {
		c.stopAckOnce.Do(func() {
			close(c.stopAckCh)
		})
		return false
	}
	if parseStartLiveAck(data) && c.startAck.CompareAndSwap(false, true) {
		return true
	}
	return false
}

func (c *liveSessionControl) SendStartLive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.channel == nil {
		return false
	}
	command, err := buildLiveCommand("startLive", c.connectionID, c.serial, map[string]any{
		"size":       c.profile.Size,
		"resolution": c.profile.Resolution,
	})
	if err != nil || c.channel.SendText(string(command)) != nil {
		return false
	}
	c.startSent.Store(true)
	if c.health != nil {
		c.health.Printf("event=start_live_sent profile=%s req_res=%s conn_id=%s elapsed_ms=%d ts_utc=%s",
			c.profile.Name, c.profile.Resolution, c.connectionID, time.Since(c.started).Milliseconds(), utcNowTag())
	}
	return true
}

func isChannelClosedSignaled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Shutdown cancels pending delayed starts, sends stopLive at most once when startLive
// was sent and the DataChannel is still usable, waits briefly for ack/flush, and closes resources.
func (c *liveSessionControl) Shutdown(holdCancel context.CancelFunc, closeResources func()) {
	c.shutdownOnce.Do(func() {
		if holdCancel != nil {
			holdCancel()
		}

		c.mu.Lock()
		c.closing = true
		ch := c.channel
		waitDur := c.stopTimeout
		needStop := c.startSent.Load() && !c.stopSent.Load()
		var sentNow bool
		if needStop {
			state := webrtc.DataChannelStateClosed
			if ch != nil {
				state = ch.ReadyState()
			}
			switch {
			case ch == nil || state != webrtc.DataChannelStateOpen || isChannelClosedSignaled(c.channelClosedCh):
				if c.health != nil {
					c.health.Printf("event=stop_live_skipped reason=channel_%s elapsed_ms=%d ts_utc=%s",
						state.String(), time.Since(c.started).Milliseconds(), utcNowTag())
				}
			case isChannelClosedSignaled(c.peerClosedCh):
				if c.health != nil {
					c.health.Printf("event=stop_live_skipped reason=peer_closed elapsed_ms=%d ts_utc=%s",
						time.Since(c.started).Milliseconds(), utcNowTag())
				}
			default:
				// VicoHome PlayControl.stopLive omits targetId when not using a station hub (!o00Oo00).
				cmd, err := buildLiveCommand("stopLive", c.connectionID, "", nil)
				if err != nil {
					if c.health != nil {
						c.health.Printf("event=stop_live_failed err=%q elapsed_ms=%d ts_utc=%s",
							err.Error(), time.Since(c.started).Milliseconds(), utcNowTag())
					}
				} else if sendErr := ch.SendText(string(cmd)); sendErr != nil {
					if c.health != nil {
						c.health.Printf("event=stop_live_failed err=%q elapsed_ms=%d ts_utc=%s",
							sendErr.Error(), time.Since(c.started).Milliseconds(), utcNowTag())
					}
				} else {
					c.stopSent.Store(true)
					sentNow = true
					if c.health != nil {
						c.health.Printf("event=stop_live_sent conn_id=%s elapsed_ms=%d ts_utc=%s",
							c.connectionID, time.Since(c.started).Milliseconds(), utcNowTag())
					}
				}
			}
		}
		c.mu.Unlock()

		if sentNow && waitDur > 0 {
			timer := time.NewTimer(waitDur)
			select {
			case <-c.stopAckCh:
				if c.health != nil {
					c.health.Printf("event=stop_live_ack elapsed_ms=%d ts_utc=%s",
						time.Since(c.started).Milliseconds(), utcNowTag())
				}
			case <-c.channelClosedCh:
			case <-c.peerClosedCh:
			case <-timer.C:
			}
			timer.Stop()
		}

		if ch != nil {
			_ = ch.Close()
		}
		if closeResources != nil {
			closeResources()
		}
	})
}
