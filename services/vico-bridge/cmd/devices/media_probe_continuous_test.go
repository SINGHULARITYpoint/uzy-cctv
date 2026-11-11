package devices

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestContinuousRetryBackoff(t *testing.T) {
	cases := []struct {
		failed int
		want   time.Duration
	}{
		{0, 1500 * time.Millisecond},
		{1, 3 * time.Second},
		{2, 5 * time.Second},
		{3, 8 * time.Second},
		{4, 10 * time.Second},
		{10, 10 * time.Second},
	}
	for _, tc := range cases {
		if got := continuousRetryBackoff(tc.failed); got != tc.want {
			t.Errorf("continuousRetryBackoff(%d) = %v, want %v", tc.failed, got, tc.want)
		}
	}
}

func TestShouldAbortDeadSession(t *testing.T) {
	// Healthy streaming session does not abort.
	if r, abort := shouldAbortDeadSession(10000, 9950, 9900, -1, -1, true, true, false, 500); abort {
		t.Fatalf("healthy session aborted: %s", r)
	}
	// PEER_OUT before any RTP arrives aborts once >= 3.5s after peer_out.
	if _, abort := shouldAbortDeadSession(7000, -1, -1, 4000, -1, false, true, false, 0); abort {
		t.Fatal("aborted too soon on peer_out without packets")
	}
	if r, abort := shouldAbortDeadSession(7600, -1, -1, 4000, -1, false, true, false, 0); !abort || r != "peer_lost" {
		t.Fatalf("expected peer_lost at 3.6s without packets, got %q/%t", r, abort)
	}
	// PEER_OUT during active stream does not abort while RTP keeps arriving.
	if r, abort := shouldAbortDeadSession(20000, 19500, 19000, 15000, -1, false, true, false, 1000); abort {
		t.Fatalf("aborted on peer_out even though RTP was still flowing: %s", r)
	}
	// PEER_OUT + >= 2.5s RTP silence aborts cleanly as peer_lost.
	if r, abort := shouldAbortDeadSession(20000, 17400, 17000, 17900, -1, false, true, false, 1000); !abort || r != "peer_lost" {
		t.Fatalf("expected peer_lost after RTP stopped + peer_out, got %q/%t", r, abort)
	}
	// Output stall threshold preserves existing 8s+5s rtp_idle and 20s continuous thresholds.
	if r, abort := shouldAbortDeadSession(25000, 19900, 16900, -1, -1, true, true, false, 1000); !abort || r != "output_stall" {
		t.Fatalf("expected output_stall with idle RTP, got %q/%t", r, abort)
	}
	// ICE failure aborts immediately.
	if r, abort := shouldAbortDeadSession(6000, -1, -1, -1, -1, true, true, true, 0); !abort || r != "ice_failed" {
		t.Fatalf("expected ice_failed, got %q/%t", r, abort)
	}
}

func TestParseStartLiveAck(t *testing.T) {
	if !parseStartLiveAck([]byte(`{"action":"startLive","returnValue":0}`)) {
		t.Fatal("expected returnValue=0 to ack")
	}
	if parseStartLiveAck([]byte(`{"action":"startLive","returnValue":-1}`)) {
		t.Fatal("expected non-zero returnValue to be rejected")
	}
	if !parseStartLiveAck([]byte(`{"action":"startLive","returnVal":0}`)) {
		t.Fatal("expected returnVal=0 to ack")
	}
	if parseStartLiveAck([]byte(`{"action":"startLive"}`)) {
		t.Fatal("expected missing return field to be rejected")
	}
	if parseStartLiveAck([]byte(`{"action":"other","returnValue":0}`)) {
		t.Fatal("expected non-startLive action to be rejected")
	}
}

func TestAcquireStartupLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "start.lock")
	releaseA, err := acquireStartupLock(context.Background(), lockPath, 0, nil)
	if err != nil {
		t.Fatalf("acquireA failed: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file to exist while held: %v", err)
	}

	// Second waiter blocks until releaseA frees the lock, then acquires cleanly.
	done := make(chan error, 1)
	var releaseB func(string)
	go func() {
		var acquireErr error
		releaseB, acquireErr = acquireStartupLock(context.Background(), lockPath, 0, nil)
		done <- acquireErr
	}()
	select {
	case err := <-done:
		t.Fatalf("second waiter returned while lock still held: %v", err)
	case <-time.After(220 * time.Millisecond):
	}
	releaseA("first_frame")
	if err := <-done; err != nil {
		t.Fatalf("waiter failed after holder released lock: %v", err)
	}
	defer releaseB("attempt_exit")

	// Cancelled waiter returns an error immediately and never gains ownership while holder B holds lock.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelWait()
	if relC, err := acquireStartupLock(waitCtx, lockPath, 0, nil); err == nil {
		relC("unexpected")
		t.Fatal("cancelled waiter unexpectedly acquired lock")
	}
	releaseB("attempt_exit")

	// A new waiter succeeds once B releases.
	releaseD, err := acquireStartupLock(context.Background(), lockPath, 0, nil)
	if err != nil {
		t.Fatalf("acquireD after releaseB failed: %v", err)
	}
	releaseD("attempt_exit")
}
