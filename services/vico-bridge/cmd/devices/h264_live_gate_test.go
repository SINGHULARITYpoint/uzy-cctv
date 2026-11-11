package devices

import (
	"encoding/hex"
	"sync/atomic"
	"testing"
)

func TestParseH264SPSCG6Profiles(t *testing.T) {
	sps360Bytes, err := hex.DecodeString("67640016ac3b505017fcb080000003008000000f42")
	if err != nil {
		t.Fatal(err)
	}
	sps360, ok := parseH264SPS(sps360Bytes)
	if !ok || !sps360.HasDimensions || sps360.ProfileIDC != 100 || sps360.LevelIDC != 22 || sps360.Width != 640 || sps360.Height != 360 {
		t.Fatalf("unexpected 360p SPS parse result: ok=%t %+v", ok, sps360)
	}

	sps2kBytes, err := hex.DecodeString("67640032ac3b50120051d080000003008000000f42")
	if err != nil {
		t.Fatal(err)
	}
	sps2k, ok := parseH264SPS(sps2kBytes)
	if !ok || !sps2k.HasDimensions || sps2k.ProfileIDC != 100 || sps2k.LevelIDC != 50 || sps2k.Width != 2304 || sps2k.Height != 1296 {
		t.Fatalf("unexpected 2K SPS parse result: ok=%t %+v", ok, sps2k)
	}
}

func TestStreamProfileAndLiveGateLowBandwidth(t *testing.T) {
	defSpec, err := parseStreamProfile("")
	if err != nil || defSpec.Name != "2k" || defSpec.Resolution != "2304x1296" || defSpec.LevelIDC != 50 {
		t.Fatalf("unexpected default profile: %+v err=%v", defSpec, err)
	}
	lowSpec, err := parseStreamProfile("360p")
	if err != nil || lowSpec.Name != "360p" || lowSpec.Resolution != "640x360" || lowSpec.LevelIDC != 22 {
		t.Fatalf("unexpected 360p profile: %+v err=%v", lowSpec, err)
	}
	if _, err := parseStreamProfile("invalid"); err == nil {
		t.Fatal("expected error for unknown stream profile")
	}

	sps360, _ := hex.DecodeString("67640016ac3b505017fcb080000003008000000f42")
	sps2k, _ := hex.DecodeString("67640032ac3b50120051d080000003008000000f42")
	makeIDR := func(sps []byte) []byte {
		out := append([]byte{0, 0, 0, 1}, sps...)
		out = append(out, 0, 0, 0, 1, 0x68, 1, 0, 0, 0, 1, 0x65, 2)
		return out
	}

	var postStartAck atomic.Bool
	var lowGate h264LiveGate
	lowGate.configureProfile(lowSpec, &postStartAck)

	// Pre-startLive preview at 640x360 must NOT satisfy the low-bandwidth gate.
	if got, ok := lowGate.pushFrame(makeIDR(sps360)); ok || len(got) != 0 {
		t.Fatal("accepted initial preview before startLive ack")
	}

	// Once startLive is acked, 2K SPS still mismatches 360p, whereas fresh 640x360 L2.2 opens and verifies dimensions.
	postStartAck.Store(true)
	if got, ok := lowGate.pushFrame(makeIDR(sps2k)); ok || len(got) != 0 {
		t.Fatal("accepted 2K stream when configured for 360p")
	}
	got, ok := lowGate.pushFrame(makeIDR(sps360))
	if !ok || len(got) == 0 || !lowGate.started || lowGate.lastSPS.Width != 640 || lowGate.lastSPS.Height != 360 {
		t.Fatalf("360p post-startLive IDR failed: ok=%t started=%t sps=%+v", ok, lowGate.started, lowGate.lastSPS)
	}

	// Preserve 2K gate rejecting real 640x360 preview.
	var twoKGate h264LiveGate
	twoKGate.configureProfile(defSpec, &postStartAck)
	if got, ok := twoKGate.pushFrame(makeIDR(sps360)); ok || len(got) != 0 {
		t.Fatal("2K gate accepted 640x360 stream")
	}
	if got, ok := twoKGate.pushFrame(makeIDR(sps2k)); !ok || len(got) == 0 || twoKGate.lastSPS.Width != 2304 || twoKGate.lastSPS.Height != 1296 {
		t.Fatalf("2K gate failed to verify real 2304x1296 stream: ok=%t sps=%+v", ok, twoKGate.lastSPS)
	}
}

func TestLiveGateStartsOnMainIDR(t *testing.T) {
	g := &h264LiveGate{}
	preview := []byte{0, 0, 0, 1, 0x67, 100, 0, 22, 0, 0, 0, 1, 0x68, 1, 0, 0, 0, 1, 0x65, 1}
	main := []byte{0, 0, 0, 1, 0x67, 100, 0, 50, 0, 0, 0, 1, 0x68, 2}
	idr := []byte{0, 0, 0, 1, 0x65, 2}
	if got := g.push(preview); got != nil {
		t.Fatal("preview should not reach restreamer")
	}
	if got := g.push(main); got != nil {
		t.Fatal("main parameter sets should wait for an IDR")
	}
	got := g.push(idr)
	if len(got) == 0 || !g.started {
		t.Fatal("main IDR did not start the stream")
	}
	counts := map[uint8]uint64{}
	countAnnexBNALTypes(got, counts)
	if counts[7] != 1 || counts[8] != 1 || counts[5] != 1 {
		t.Fatalf("expected SPS/PPS/IDR, got %v", counts)
	}
	mainOnly := &h264LiveGate{}
	mainOnly.push(main)
	if got := mainOnly.push(idr); len(got) == 0 {
		t.Fatal("main-only session was held waiting for preview")
	}
}
