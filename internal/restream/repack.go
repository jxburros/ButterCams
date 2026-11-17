package restream

import (
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"
)

// maxRTPPacket is the largest RTP packet the RTSP server sends. gortsplib
// marshals every outgoing packet into a buffer of this size and refuses
// bigger ones, while cameras streaming over TCP may send larger packets.
const maxRTPPacket = 1472

// repacker splits a camera's video into packets the server can send. It is
// switched on for a mount at the first oversized packet: from then on every
// video packet of that mount is reassembled into access units and packed
// again, so the client sees one consistent sequence. It belongs to the
// camera's pump goroutine.
type repacker struct {
	dec264 *rtph264.Decoder
	enc264 *rtph264.Encoder
	dec265 *rtph265.Decoder
	enc265 *rtph265.Encoder
}

func newRepacker(f format.Format) *repacker {
	payload := maxRTPPacket - 12 - 16 // RTP header, room for extensions
	switch vf := f.(type) {
	case *format.H264:
		dec, err := vf.CreateDecoder()
		if err != nil {
			return nil
		}
		enc := &rtph264.Encoder{PayloadType: vf.PayloadTyp, PacketizationMode: 1, PayloadMaxSize: payload}
		if enc.Init() != nil {
			return nil
		}
		return &repacker{dec264: dec, enc264: enc}
	case *format.H265:
		dec, err := vf.CreateDecoder()
		if err != nil {
			return nil
		}
		enc := &rtph265.Encoder{PayloadType: vf.PayloadTyp, PayloadMaxSize: payload}
		if enc.Init() != nil {
			return nil
		}
		return &repacker{dec265: dec, enc265: enc}
	}
	return nil
}

// repack takes one camera packet (never changed) and returns the packets to
// send in its place: none until an access unit is complete.
func (r *repacker) repack(pkt *rtp.Packet) []*rtp.Packet {
	var au [][]byte
	var err error
	if r.dec264 != nil {
		au, err = r.dec264.Decode(pkt)
	} else {
		au, err = r.dec265.Decode(pkt)
	}
	if err != nil || len(au) == 0 {
		return nil // more packets needed, or a damaged unit
	}
	var out []*rtp.Packet
	if r.enc264 != nil {
		out, err = r.enc264.Encode(au)
	} else {
		out, err = r.enc265.Encode(au)
	}
	if err != nil {
		return nil
	}
	for _, p := range out {
		p.Timestamp = pkt.Timestamp
	}
	return out
}
