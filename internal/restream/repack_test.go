package restream

import (
	"bytes"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

// Cameras on TCP may send RTP packets bigger than the server can send; the
// restream must pass them on in pieces, not drop them.
func TestRepackSplitsOversizedPackets(t *testing.T) {
	f := &format.H264{PayloadTyp: 96, PacketizationMode: 1,
		SPS: []byte{0x67, 0x42, 0xc0, 0x1e, 0xd9, 0x00, 0xa0, 0x47, 0xfe, 0xc8}, PPS: []byte{0x68, 0xce, 0x3c, 0x80}}
	r := newRepacker(f)
	if r == nil {
		t.Fatal("no repacker")
	}
	nalu := make([]byte, 5000)
	nalu[0] = 0x65 // IDR slice
	for i := 1; i < len(nalu); i++ {
		nalu[i] = byte(i)
	}
	in := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, Marker: true, Timestamp: 123456, SequenceNumber: 7}, Payload: nalu}
	orig := append([]byte(nil), nalu...)
	out := r.repack(in)
	if len(out) < 4 {
		t.Fatalf("expected the unit in several packets, got %d", len(out))
	}
	if !bytes.Equal(in.Payload, orig) || in.Timestamp != 123456 {
		t.Fatal("the camera's packet was changed")
	}
	dec, _ := f.CreateDecoder()
	var got [][]byte
	for _, p := range out {
		if p.MarshalSize() > maxRTPPacket {
			t.Fatalf("packet of %d bytes is still too large", p.MarshalSize())
		}
		if p.Timestamp != 123456 {
			t.Fatalf("timestamp changed: %d", p.Timestamp)
		}
		if au, err := dec.Decode(p); err == nil {
			got = au
		}
	}
	if len(got) != 1 || !bytes.Equal(got[0], orig) {
		t.Fatalf("the unit did not survive repacking: %d NAL units", len(got))
	}
}
