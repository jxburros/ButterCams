package stream

import (
	"bytes"
	"fmt"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"github.com/jxburros/buttercams/internal/mp4meta"
)

// InitSegment builds the MP4 initialization segment (ftyp + moov) for a
// video track and an optional audio track. It is the header of every
// recording and of every live stream.
//
// H.265 is always written with the "hvc1" sample entry: Safari plays nothing
// else, and every other player accepts it. "hvc1" promises that all parameter
// sets are in the header, which holds here: the recorder starts a new file,
// and the live muxer a new header, whenever they change (spec 16.5).
func InitSegment(vp VideoParams, ap *AudioParams) ([]byte, error) {
	if !vp.Complete() {
		return nil, fmt.Errorf("the video parameter sets are incomplete")
	}
	tracks := []*fmp4.InitTrack{{ID: mp4meta.VideoTrackID, TimeScale: mp4meta.VideoTimescale, Codec: vp.MP4Codec()}}
	if ap != nil {
		tracks = append(tracks, &fmp4.InitTrack{ID: mp4meta.AudioTrackID, TimeScale: ap.Timescale, Codec: ap.Codec})
	}
	var ib seekablebuffer.Buffer
	if err := (fmp4.Init{Tracks: tracks}).Marshal(&ib); err != nil {
		return nil, fmt.Errorf("building the MP4 header: %w", err)
	}
	out := ib.Bytes()
	if vp.Codec == "h265" {
		// The sample entry type sits right before its first child box, hvcC,
		// separated by the 78 fixed bytes of a visual sample entry. Replacing
		// the four characters changes no size.
		if i := bytes.Index(out, []byte("hev1")); i >= 0 && bytes.Contains(out[i:], []byte("hvcC")) {
			copy(out[i:], "hvc1")
		}
	}
	return out, nil
}
