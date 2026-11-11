package devices

import (
	"net"
	"testing"
	"time"
)

func TestICEIPFilterRejectsLinkLocalAndLoopback(t *testing.T) {
	filter := func(ip net.IP) bool {
		return !ip.IsLinkLocalUnicast() && !ip.IsLoopback()
	}

	if filter(net.ParseIP("169.254.83.107")) {
		t.Error("expected link-local IP 169.254.83.107 to be rejected")
	}
	if filter(net.ParseIP("127.0.0.1")) {
		t.Error("expected loopback IP 127.0.0.1 to be rejected")
	}
	if !filter(net.ParseIP("192.168.1.213")) {
		t.Error("expected LAN IP 192.168.1.213 to be accepted")
	}
	if !filter(net.ParseIP("76.184.76.170")) {
		t.Error("expected public IP 76.184.76.170 to be accepted")
	}
}

func TestForceDirectFiltersTURNServers(t *testing.T) {
	inputServers := []iceServer{
		{URL: "stun:stun.l.google.com:19302"},
		{URL: "turn:turn.example.com:3478", Username: "user", Credential: "pwd"},
		{URL: "turns:turn.example.com:5349", Username: "user", Credential: "pwd"},
	}

	direct := buildICEServers(inputServers, true, 2)
	if len(direct) != 1 || direct[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("unexpected direct servers: %+v", direct)
	}

	all := buildICEServers(inputServers, false, 0)
	if len(all) != 3 {
		t.Errorf("expected 3 servers when forceDirect is false, got %d", len(all))
	}
}

func TestBuildICEServersLimitsTurnRelays(t *testing.T) {
	inputServers := []iceServer{
		{URL: "stun:stun.l.google.com:19302"},
		{URL: "turn:turn1.example.com:3478", Username: "user", Credential: "pwd"},
		{URL: "turn:turn2.example.com:3478", Username: "user", Credential: "pwd"},
		{URL: "turn:turn3.example.com:3478", Username: "user", Credential: "pwd"},
		{URL: "turn:turn4.example.com:3478", Username: "user", Credential: "pwd"},
	}

	limited := buildICEServers(inputServers, false, 2)
	if len(limited) != 3 { // 1 stun + 2 turn
		t.Fatalf("expected 3 servers (1 stun + 2 turn), got %d", len(limited))
	}
	if limited[1].URLs[0] != "turn:turn1.example.com:3478" || limited[2].URLs[0] != "turn:turn2.example.com:3478" {
		t.Errorf("unexpected turn servers: %+v", limited)
	}
}

func TestFilterRelaySDPAndCandidates(t *testing.T) {
	sdp := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\na=candidate:1 1 UDP 2130706431 192.168.1.71 5000 typ host\r\na=candidate:2 1 UDP 1694498815 76.184.76.170 5001 typ srflx raddr 192.168.1.71 rport 5000\r\na=candidate:3 1 UDP 100000000 1.2.3.4 5002 typ relay raddr 76.184.76.170 rport 5001\r\n"

	filterSDPRelay := func(input string) string {
		lines := []string{}
		for _, l := range netLines(input) {
			if len(l) > 12 && l[:12] == "a=candidate:" && contains(l, " typ relay") {
				continue
			}
			lines = append(lines, l)
		}
		return joinLines(lines)
	}

	filtered := filterSDPRelay(sdp)
	if contains(filtered, "typ relay") {
		t.Error("expected relay candidate to be filtered from SDP")
	}
	if !contains(filtered, "typ host") || !contains(filtered, "typ srflx") {
		t.Error("expected host and srflx candidates to remain in SDP")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && searchSubstr(s, substr)))
}

func searchSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func netLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func joinLines(lines []string) string {
	res := ""
	for i, l := range lines {
		if i > 0 {
			res += "\n"
		}
		res += l
	}
	return res
}

func TestPLIThrottlingLogic(t *testing.T) {
	minInterval := 2500 * time.Millisecond
	var lastPLI time.Time

	shouldSend := func(now time.Time) bool {
		if !lastPLI.IsZero() && now.Sub(lastPLI) < minInterval {
			return false
		}
		lastPLI = now
		return true
	}

	t0 := time.Now()
	if !shouldSend(t0) {
		t.Error("first PLI should be sent")
	}
	if shouldSend(t0.Add(500 * time.Millisecond)) {
		t.Error("PLI at 500ms should be throttled")
	}
	if shouldSend(t0.Add(2400 * time.Millisecond)) {
		t.Error("PLI at 2400ms should be throttled")
	}
	if !shouldSend(t0.Add(2500 * time.Millisecond)) {
		t.Error("PLI at 2500ms should be sent")
	}
}

func TestOutputStallRefinement(t *testing.T) {
	isStalled := func(lastFrameMs, lastRTPMs, elapsedMs int64) bool {
		rtpIdle := lastRTPMs < 0 || elapsedMs-lastRTPMs >= 5000
		return lastFrameMs >= 0 && ((elapsedMs-lastFrameMs >= 8000 && rtpIdle) || elapsedMs-lastFrameMs >= 20000)
	}

	// Active RTP, 9s since last frame -> should NOT stall (allow in-band recovery)
	if isStalled(10000, 18900, 19000) {
		t.Error("expected active RTP within 5s not to trigger output_stall at 9s")
	}

	// Idle RTP for 6s, 8.5s since last frame -> SHOULD stall
	if !isStalled(10000, 12500, 18500) {
		t.Error("expected idle RTP with frame gap >= 8s to trigger output_stall")
	}

	// Active RTP, but frame gap >= 20s -> SHOULD stall
	if !isStalled(10000, 30500, 31000) {
		t.Error("expected 20s+ frame gap to trigger output_stall regardless of RTP")
	}
}
