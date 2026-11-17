// Package stream turns the RTP packets of a camera into samples (access
// units) ready to be written into fragmented MP4. It is shared by the
// recorder (internal/worker), which writes them to files, and the media
// process (internal/media), which sends them to browsers. Nothing here
// decodes video (spec rule 2).
package stream

import (
	"bytes"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// VideoParams are the codec parameter sets a file or live stream is opened
// with.
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

// Equal reports whether two sets are the same.
func (p VideoParams) Equal(o VideoParams) bool {
	return p.Codec == o.Codec && bytes.Equal(p.VPS, o.VPS) && bytes.Equal(p.SPS, o.SPS) && bytes.Equal(p.PPS, o.PPS)
}

// MP4Codec returns the codec description for the MP4 initialization segment.
func (p VideoParams) MP4Codec() codecs.Codec {
	if p.Codec == "h265" {
		return &codecs.H265{VPS: p.VPS, SPS: p.SPS, PPS: p.PPS}
	}
	return &codecs.H264{SPS: p.SPS, PPS: p.PPS}
}

// AudioParams describe the audio track of a file or live stream.
type AudioParams struct {
	Name      string // "aac" or "opus": what is stored
	Codec     codecs.Codec
	Timescale uint32
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
