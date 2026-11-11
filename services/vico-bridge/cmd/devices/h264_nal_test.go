package devices

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

func TestMainStreamFrameSurvivesLargeRTPBurst(t *testing.T) {
	builder := newLiveH264Builder()
	// A large fragmented keyframe used to exceed the old 50-packet window.
	for i := 0; i < 120; i++ {
		fuHeader := byte(5)
		if i == 0 {
			fuHeader |= 0x80
		}
		if i == 119 {
			fuHeader |= 0x40
		}
		builder.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: 90000, Marker: i == 119}, Payload: []byte{0x7c, fuHeader, 0x88}})
	}
	builder.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: 120, Timestamp: 93000, Marker: true}, Payload: []byte{0x41, 0x80}})
	sample := builder.Pop()
	if sample == nil || sample.PrevDroppedPackets != 0 {
		t.Fatalf("large keyframe was lost: sample=%v", sample)
	}
	counts := map[uint8]uint64{}
	countAnnexBNALTypes(sample.Data, counts)
	if counts[5] != 1 {
		t.Fatalf("expected one IDR NAL, got %v", counts)
	}
}

func TestLiveRTPHoleAgesOutBeforePacketWindowFills(t *testing.T) {
	builder := samplebuilder.New(2048, &codecs.H264Packet{}, 90000, samplebuilder.WithMaxTimeDelay(liveRTPReorderWindow))
	for i := 0; i < 18; i++ {
		if i == 1 {
			continue
		}
		builder.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(90000 + i*6000), Marker: true}, Payload: []byte{0x41, 0x80}})
	}
	var drops uint16
	for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
		drops += sample.PrevDroppedPackets
	}
	if drops != 1 {
		t.Fatalf("expected recovery past one missing packet, got %d dropped packets", drops)
	}
}
