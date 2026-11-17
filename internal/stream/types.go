// Package stream turns the RTP packets of a camera session into timed
// samples, without decoding video (spec rule 2). The recorder workers use it
// to write files and the media process uses it to feed live viewers, so both
// read cameras in exactly the same way.
package stream

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/pion/rtp"
)

// Track identifies the stream a packet belongs to.
type Track uint8

// Tracks.
const (
	TrackVideo Track = iota
	TrackAudio
)

// Packet is one RTP packet exactly as received from a camera, with the timing
// information needed to use it. Packets handed out by a fan-out are shared by
// every reader and must never be changed: a reader that needs to change one
// copies it first (spec 16.9).
type Packet struct {
	Session  uint64 // the RTSP session that produced it
	Track    Track
	RTP      *rtp.Packet
	PTS      int64 // presentation time in clock-rate units; valid when PTSValid
	PTSValid bool
	Recv     time.Time // wall clock (with monotonic reading) at receipt
}

// Sample is one access unit ready to be written.
type Sample struct {
	Video bool
	PTS   int64 // track timescale
	DTS   int64
	Dur   uint32 // audio only: nominal duration
	Key   bool
	NALUs [][]byte // video: the NAL units of the access unit, as received
	Data  []byte   // audio: the access unit
	Recv  time.Time
}

// VideoParams are the codec parameter sets of a video stream.
type VideoParams struct {
	Codec string // "h264" or "h265"
	VPS   []byte
	SPS   []byte
	PPS   []byte
}

// Complete reports whether every parameter set the codec needs is known.
func (p VideoParams) Complete() bool {
	if p.Codec == "h265" {
		return len(p.VPS) > 0 && len(p.SPS) > 0 && len(p.PPS) > 0
	}
	return len(p.SPS) > 0 && len(p.PPS) > 0
}

// Equal reports whether two sets of parameters are the same.
func (p VideoParams) Equal(o VideoParams) bool {
	return p.Codec == o.Codec && bytes.Equal(p.VPS, o.VPS) && bytes.Equal(p.SPS, o.SPS) && bytes.Equal(p.PPS, o.PPS)
}

// MP4Codec returns the codec description for an MP4 initialization segment.
// H.265 is written with the hvc1 sample entry, which Safari requires.
func (p VideoParams) MP4Codec() codecs.Codec {
	if p.Codec == "h265" {
		return &codecs.H265{VPS: p.VPS, SPS: p.SPS, PPS: p.PPS}
	}
	return &codecs.H264{SPS: p.SPS, PPS: p.PPS}
}

// MIMECodec returns the RFC 6381 codec string of the video, as browsers
// expect it in MediaSource.isTypeSupported: avc1.PPCCLL or hvc1.….
func (p VideoParams) MIMECodec() string {
	if p.Codec == "h265" {
		return hevcCodecString(p.SPS)
	}
	if len(p.SPS) >= 4 {
		return "avc1." + hex.EncodeToString(p.SPS[1:4])
	}
	return "avc1.42e01e"
}

// hevcCodecString builds hvc1.<profile space+idc>.<compat flags>.<tier+level>.<constraints>
// from the profile_tier_level of the SPS.
func hevcCodecString(sps []byte) (out string) {
	defer func() {
		if recover() != nil {
			out = "hvc1.1.6.L93.B0"
		}
	}()
	var s h265.SPS
	if err := s.Unmarshal(sps); err != nil {
		return "hvc1.1.6.L93.B0"
	}
	ptl := s.ProfileTierLevel
	space := ""
	switch ptl.GeneralProfileSpace {
	case 1:
		space = "A"
	case 2:
		space = "B"
	case 3:
		space = "C"
	}
	// The compatibility flags are written in reverse bit order, in hex.
	var compat uint32
	for i := 0; i < 32; i++ {
		if ptl.GeneralProfileCompatibilityFlag[i] {
			compat |= 1 << uint(i)
		}
	}
	tier := "L"
	if ptl.GeneralTierFlag == 1 {
		tier = "H"
	}
	cons := byte(0)
	if ptl.GeneralProgressiveSourceFlag {
		cons |= 0x80
	}
	if ptl.GeneralInterlacedSourceFlag {
		cons |= 0x40
	}
	if ptl.GeneralNonPackedConstraintFlag {
		cons |= 0x20
	}
	if ptl.GeneralFrameOnlyConstraintFlag {
		cons |= 0x10
	}
	return fmt.Sprintf("hvc1.%s%d.%X.%s%d.%X", space, ptl.GeneralProfileIdc, compat, tier, ptl.GeneralLevelIdc, cons)
}

// AudioParams describe an audio track as it is stored or sent.
type AudioParams struct {
	Name      string // "aac" or "opus"
	Codec     codecs.Codec
	Timescale uint32
}

// MIMECodec returns the RFC 6381 codec string of the audio.
func (a AudioParams) MIMECodec() string {
	if a.Name == "opus" {
		return "opus"
	}
	if c, ok := a.Codec.(*codecs.MPEG4Audio); ok && c.Config.Type != 0 {
		return fmt.Sprintf("mp4a.40.%d", int(c.Config.Type))
	}
	return "mp4a.40.2"
}
