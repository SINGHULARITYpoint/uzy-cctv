package devices

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type parsedHealthEvent struct {
	event string
	ts    time.Time
	raw   string
	kv    map[string]string
}

func parseHealthLogEvents(t *testing.T, path string) []parsedHealthEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read health log %s: %v", path, err)
	}
	var out []parsedHealthEvent
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kv := make(map[string]string)
		for _, tok := range strings.Fields(line) {
			if k, v, ok := strings.Cut(tok, "="); ok {
				kv[k] = v
			}
		}
		ev := parsedHealthEvent{event: kv["event"], raw: line, kv: kv}
		if tsStr := kv["ts_utc"]; tsStr != "" {
			ts, err := time.Parse(time.RFC3339Nano, tsStr)
			if err != nil {
				t.Fatalf("invalid ts_utc %q in %q: %v", tsStr, line, err)
			}
			ev.ts = ts
		}
		out = append(out, ev)
	}
	return out
}

func findEvent(events []parsedHealthEvent, name string) (parsedHealthEvent, bool) {
	for _, ev := range events {
		if ev.event == name {
			return ev, true
		}
	}
	return parsedHealthEvent{}, false
}

func waitForLogSubstring(t *testing.T, path, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), substr) {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	raw, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %q in %s; contents:\n%s", substr, path, string(raw))
}

func TestStartupLockHelperProcess(t *testing.T) {
	if os.Getenv("GO_STARTUP_LOCK_HELPER") != "1" {
		return
	}
	mode := os.Getenv("SL_MODE")
	lockPath := os.Getenv("SL_LOCK_PATH")
	logPath := os.Getenv("SL_HEALTH_LOG")
	holdMs, _ := strconv.Atoi(os.Getenv("SL_HOLD_MS"))
	streamAfterMs, _ := strconv.Atoi(os.Getenv("SL_STREAM_AFTER_MS"))
	waitTimeoutMs, _ := strconv.Atoi(os.Getenv("SL_WAIT_TIMEOUT_MS"))

	fh, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	defer fh.Close()
	health := log.New(fh, "", log.LstdFlags|log.LUTC|log.Lmicroseconds)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if mode == "wait_and_cancel_on_stdin" {
		go func() {
			_, _ = io.ReadAll(os.Stdin)
			cancel()
		}()
	}

	err = runWithStartupLock(ctx, lockPath, time.Duration(waitTimeoutMs)*time.Millisecond, health, func(release func(string)) error {
		health.Printf("event=wake_start ts_utc=%s", utcNowTag())
		if mode == "hold_until_killed" {
			health.Printf("event=wake wake_code=0 ts_utc=%s", utcNowTag())
			select {}
		}
		time.Sleep(30 * time.Millisecond)
		health.Printf("event=wake wake_code=0 ts_utc=%s", utcNowTag())
		health.Printf("event=ice_state state=checking ice=/ ts_utc=%s", utcNowTag())
		time.Sleep(30 * time.Millisecond)
		health.Printf("event=ice_state state=connected ice=relay/prflx ts_utc=%s", utcNowTag())
		health.Printf("event=channel_open ts_utc=%s", utcNowTag())
		if holdMs > 0 {
			time.Sleep(time.Duration(holdMs) * time.Millisecond)
		}
		health.Printf("event=first_frame ts_utc=%s", utcNowTag())
		release("first_frame")
		if streamAfterMs > 0 {
			time.Sleep(time.Duration(streamAfterMs) * time.Millisecond)
			health.Printf("event=still_streaming ts_utc=%s", utcNowTag())
		}
		return nil
	})
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func spawnLockHelper(t *testing.T, envMap map[string]string) (*exec.Cmd, io.WriteCloser, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStartupLockHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_STARTUP_LOCK_HELPER=1")
	for k, v := range envMap {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdin io.WriteCloser
	if envMap["SL_MODE"] == "wait_and_cancel_on_stdin" {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, stdin, nil
}

func TestStartupLockCrossProcessOutlivesOldExpiry(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "start.lock")
	logA := filepath.Join(dir, "holderA.log")
	logB := filepath.Join(dir, "waiterB.log")

	// Holder A outlives the old 18s expiry (19.5s) and continues streaming for 3s after first_frame.
	cmdA, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":            "hold_outlive",
		"SL_LOCK_PATH":       lockPath,
		"SL_HEALTH_LOG":      logA,
		"SL_HOLD_MS":         "19500",
		"SL_STREAM_AFTER_MS": "3000",
	})
	defer func() {
		if cmdA.Process != nil {
			_ = cmdA.Process.Kill()
		}
	}()
	waitForLogSubstring(t, logA, "event=lock_acquired", 5*time.Second)

	cmdB, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":       "attempt_after_wait",
		"SL_LOCK_PATH":  lockPath,
		"SL_HEALTH_LOG": logB,
		"SL_HOLD_MS":    "200",
	})
	defer func() {
		if cmdB.Process != nil {
			_ = cmdB.Process.Kill()
		}
	}()

	waitForLogSubstring(t, logB, "event=lock_waiting", 5*time.Second)

	if err := cmdA.Wait(); err != nil {
		t.Fatalf("holder A exited with error: %v", err)
	}
	if err := cmdB.Wait(); err != nil {
		t.Fatalf("waiter B exited with error: %v", err)
	}

	evA := parseHealthLogEvents(t, logA)
	evB := parseHealthLogEvents(t, logB)

	aAcq, okA1 := findEvent(evA, "lock_acquired")
	aWake, okA2 := findEvent(evA, "wake")
	aIce, okA3 := findEvent(evA, "ice_state")
	aDC, okA4 := findEvent(evA, "channel_open")
	aFF, okA5 := findEvent(evA, "first_frame")
	aRel, okA6 := findEvent(evA, "lock_released")
	aStream, okA7 := findEvent(evA, "still_streaming")
	if !okA1 || !okA2 || !okA3 || !okA4 || !okA5 || !okA6 || !okA7 {
		t.Fatalf("holder A missing expected events: acq=%t wake=%t ice=%t dc=%t ff=%t rel=%t stream=%t", okA1, okA2, okA3, okA4, okA5, okA6, okA7)
	}
	bWait, okB0 := findEvent(evB, "lock_waiting")
	bAcq, okB1 := findEvent(evB, "lock_acquired")
	bWake, okB2 := findEvent(evB, "wake")
	bFF, okB3 := findEvent(evB, "first_frame")
	bRel, okB4 := findEvent(evB, "lock_released")
	if !okB0 || !okB1 || !okB2 || !okB3 || !okB4 {
		t.Fatalf("waiter B missing expected events: wait=%t acq=%t wake=%t ff=%t rel=%t", okB0, okB1, okB2, okB3, okB4)
	}

	if bAcq.ts.Before(aRel.ts) {
		t.Fatalf("waiter B acquired lock (%v) before holder A released (%v)", bAcq.ts, aRel.ts)
	}
	if bWake.ts.Before(aRel.ts) {
		t.Fatalf("waiter B woke (%v) before holder A released (%v)", bWake.ts, aRel.ts)
	}
	waitMs, _ := strconv.Atoi(bAcq.kv["wait_ms"])
	if waitMs < 18500 {
		t.Fatalf("expected waiter B to wait >18.5s past old expiry, got wait_ms=%d", waitMs)
	}
	if aRel.kv["reason"] != "first_frame" || bRel.kv["reason"] != "first_frame" {
		t.Fatalf("expected release reason=first_frame, got A=%q B=%q", aRel.kv["reason"], bRel.kv["reason"])
	}
	if !aStream.ts.After(bAcq.ts) {
		t.Fatalf("expected already-streaming A (%v) to keep running while B started (%v)", aStream.ts, bAcq.ts)
	}
	if aAcq.ts.After(aWake.ts) || aWake.ts.After(aIce.ts) || aDC.ts.After(aFF.ts) || aFF.ts.After(aRel.ts) {
		t.Fatalf("timestamps out of order in holder A: acq=%v wake=%v ice=%v dc=%v ff=%v rel=%v", aAcq.ts, aWake.ts, aIce.ts, aDC.ts, aFF.ts, aRel.ts)
	}
	_ = bWait
	_ = bFF
	rawAll, _ := os.ReadFile(logB)
	if regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`).Match(rawAll) {
		t.Fatalf("raw IP leaked into sanitized health log: %s", string(rawAll))
	}
}

func TestStartupLockCrossProcessCancelledWaiterNeverHandshakes(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "start.lock")
	logA := filepath.Join(dir, "holderA.log")
	logB := filepath.Join(dir, "waiterB.log")
	logC := filepath.Join(dir, "waiterC.log")

	cmdA, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":       "hold_until_killed",
		"SL_LOCK_PATH":  lockPath,
		"SL_HEALTH_LOG": logA,
	})
	defer func() {
		if cmdA.Process != nil {
			_ = cmdA.Process.Kill()
			_ = cmdA.Wait()
		}
	}()
	waitForLogSubstring(t, logA, "event=lock_acquired", 5*time.Second)

	// Subcase 1: Waiter cancelled externally while blocked (stdin EOF cancels ctx).
	cmdB, stdinB, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":       "wait_and_cancel_on_stdin",
		"SL_LOCK_PATH":  lockPath,
		"SL_HEALTH_LOG": logB,
	})
	waitForLogSubstring(t, logB, "event=lock_waiting", 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	_ = stdinB.Close()
	if err := cmdB.Wait(); err == nil {
		t.Fatal("expected cancelled waiter B to exit non-zero")
	}
	evB := parseHealthLogEvents(t, logB)
	if canc, ok := findEvent(evB, "lock_cancelled"); !ok || canc.ts.IsZero() {
		t.Fatalf("expected lock_cancelled with ts_utc in waiter B, got %+v", evB)
	}
	for _, forbidden := range []string{"lock_acquired", "wake_start", "wake", "ice_state", "channel_open", "first_frame"} {
		if _, found := findEvent(evB, forbidden); found {
			t.Fatalf("cancelled waiter B executed forbidden handshake step %q", forbidden)
		}
	}

	// Subcase 2: Waiter hits explicit startup-lock wait timeout cleanly without handshake.
	cmdC, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":            "attempt_after_wait",
		"SL_LOCK_PATH":       lockPath,
		"SL_HEALTH_LOG":      logC,
		"SL_WAIT_TIMEOUT_MS": "350",
	})
	if err := cmdC.Wait(); err == nil {
		t.Fatal("expected timed-out waiter C to exit non-zero")
	}
	evC := parseHealthLogEvents(t, logC)
	if canc, ok := findEvent(evC, "lock_cancelled"); !ok || canc.kv["reason"] != "timeout" {
		t.Fatalf("expected lock_cancelled reason=timeout in waiter C, got %+v", evC)
	}
	for _, forbidden := range []string{"lock_acquired", "wake_start", "wake", "ice_state", "channel_open", "first_frame"} {
		if _, found := findEvent(evC, forbidden); found {
			t.Fatalf("timed-out waiter C executed forbidden handshake step %q", forbidden)
		}
	}
}

func TestStartupLockCrossProcessCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "start.lock")
	logA := filepath.Join(dir, "holderA.log")
	logB := filepath.Join(dir, "waiterB.log")

	cmdA, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":       "hold_until_killed",
		"SL_LOCK_PATH":  lockPath,
		"SL_HEALTH_LOG": logA,
	})
	waitForLogSubstring(t, logA, "event=lock_acquired", 5*time.Second)

	cmdB, _, _ := spawnLockHelper(t, map[string]string{
		"SL_MODE":       "attempt_after_wait",
		"SL_LOCK_PATH":  lockPath,
		"SL_HEALTH_LOG": logB,
		"SL_HOLD_MS":    "150",
	})
	defer func() {
		if cmdB.Process != nil {
			_ = cmdB.Process.Kill()
		}
	}()
	waitForLogSubstring(t, logB, "event=lock_waiting", 5*time.Second)
	time.Sleep(350 * time.Millisecond)

	// Confirm B did NOT start handshake while A is alive, then kill A without cleanup.
	for _, forbidden := range []string{"lock_acquired", "wake_start", "wake"} {
		if _, found := findEvent(parseHealthLogEvents(t, logB), forbidden); found {
			t.Fatalf("waiter B proceeded with %q while holder A was still alive", forbidden)
		}
	}
	killAt := time.Now()
	if err := cmdA.Process.Kill(); err != nil {
		t.Fatalf("kill holder A: %v", err)
	}
	_ = cmdA.Wait()

	if err := cmdB.Wait(); err != nil {
		t.Fatalf("waiter B failed after holder A crashed: %v", err)
	}
	recoveredIn := time.Since(killAt)
	if recoveredIn > 3*time.Second {
		t.Fatalf("waiter B took too long to recover lock after crash: %v", recoveredIn)
	}
	evB := parseHealthLogEvents(t, logB)
	for _, want := range []string{"lock_waiting", "lock_acquired", "wake_start", "wake", "ice_state", "channel_open", "first_frame", "lock_released"} {
		ev, ok := findEvent(evB, want)
		if !ok || ev.ts.IsZero() {
			t.Fatalf("recovered waiter B missing %q with valid ts_utc", want)
		}
	}
}
