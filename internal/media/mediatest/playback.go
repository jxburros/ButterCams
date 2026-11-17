// Package mediatest holds test clients for the media process: they ask for
// what a browser's player asks for and put the answers together the way the
// player does, so that ffmpeg can decode exactly what a viewer would see.
package mediatest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/media"
)

// PlaybackClient talks to the /play routes of a media handler directly,
// playing the guardian's part: it names the file by its candidate paths.
type PlaybackClient struct {
	// URL is the base address of the handler, without a trailing slash.
	URL string
	// Token, when set, is sent as the media token.
	Token  string
	Client *http.Client
}

// PlayIndex is the answer of /play/index.
type PlayIndex struct {
	MIME       string       `json:"mime"`
	VideoMIME  string       `json:"videoMime"`
	Init       [2]int64     `json:"init"`
	Start      time.Time    `json:"start"`
	DurationMs float64      `json:"durationMs"`
	Chunks     [][4]float64 `json:"chunks"` // startMs, durationMs, offset, size
	Open       bool         `json:"open"`
	Width      int          `json:"width"`
	Height     int          `json:"height"`
	HasAudio   bool         `json:"hasAudio"`
}

// File names one recording by the paths it can be read from.
type File struct {
	Paths []string
	Open  bool // the file is still being recorded
}

func (c *PlaybackClient) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// Do sends a GET for /play/<what> with the given query and extra headers.
// The caller closes the body.
func (c *PlaybackClient) Do(what string, query url.Values, f File, header http.Header) (*http.Response, error) {
	u := c.URL + "/play/" + what
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set(ipc.PathsHeader, media.EncodePaths(f.Paths))
	if f.Open {
		req.Header.Set(ipc.OpenHeader, "1")
	}
	if c.Token != "" {
		req.Header.Set(ipc.TokenHeader, c.Token)
	}
	return c.client().Do(req)
}

func (c *PlaybackClient) body(what string, query url.Values, f File, header http.Header, want int) ([]byte, http.Header, error) {
	res, err := c.Do(what, query, f, header)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, res.Header, err
	}
	if res.StatusCode != want {
		return data, res.Header, fmt.Errorf("/play/%s answered %d: %s", what, res.StatusCode, data)
	}
	return data, res.Header, nil
}

// Index fetches the playback index of a file.
func (c *PlaybackClient) Index(f File) (PlayIndex, error) {
	var ix PlayIndex
	data, _, err := c.body("index", nil, f, nil, http.StatusOK)
	if err != nil {
		return ix, err
	}
	return ix, json.Unmarshal(data, &ix)
}

// Range fetches n bytes at offset off with a range request.
func (c *PlaybackClient) Range(f File, off, n int64) ([]byte, error) {
	h := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+n-1)}}
	data, _, err := c.body("data", nil, f, h, http.StatusPartialContent)
	if err == nil && int64(len(data)) != n {
		err = fmt.Errorf("asked for %d bytes at %d, got %d", n, off, len(data))
	}
	return data, err
}

// Keyframes fetches the keyframe-only fragments between two times
// (milliseconds from the start of the file). With withInit the answer starts
// with the video-only initialization segment and is a complete MP4.
func (c *PlaybackClient) Keyframes(f File, fromMs, toMs float64, withInit bool) ([]byte, error) {
	q := url.Values{"from": {strconv.FormatFloat(fromMs, 'f', -1, 64)}, "to": {strconv.FormatFloat(toMs, 'f', -1, 64)}}
	if withInit {
		q.Set("init", "1")
	}
	data, _, err := c.body("keyframes", q, f, nil, http.StatusOK)
	return data, err
}

// Thumb fetches the keyframe at or before a time as a complete small MP4.
func (c *PlaybackClient) Thumb(f File, tMs float64) ([]byte, http.Header, error) {
	return c.body("thumb", url.Values{"t": {strconv.FormatFloat(tMs, 'f', -1, 64)}}, f, nil, http.StatusOK)
}

// Whole fetches a file the way the player does, the initialization segment
// and then every chunk with its own range request, and returns them
// concatenated: a playable MP4.
func (c *PlaybackClient) Whole(f File) ([]byte, PlayIndex, error) {
	ix, err := c.Index(f)
	if err != nil {
		return nil, ix, err
	}
	out, err := c.Range(f, ix.Init[0], ix.Init[1])
	if err != nil {
		return nil, ix, err
	}
	for _, ch := range ix.Chunks {
		data, err := c.Range(f, int64(ch[2]), int64(ch[3]))
		if err != nil {
			return nil, ix, err
		}
		out = append(out, data...)
	}
	return out, ix, nil
}

// Placement says where Stitch put one file on the player's timeline.
type Placement struct {
	MediaStartMs float64 // where the file's first frame sits on the joined timeline
	Chunks       int     // chunks appended
	// Dropped is the number of chunks at the end of this file that were left
	// out because the next file starts with the same frames (the overlap of
	// the hourly rollover, spec 6.5).
	Dropped int
	// Joined is true when the file continues the previous one without a
	// hole; false when it was placed by its wall-clock time after a gap.
	Joined bool
}

type stitchFile struct {
	ix     PlayIndex
	init   []byte
	chunks [][]byte
	frame  int64 // duration of one video frame, 90 kHz ticks
}

func ticks(ms float64) int64 { return int64(math.Round(ms * 90)) }

// Stitch builds one continuous stream from consecutive files, the way the
// timeline player does with timestampOffset (spec 16.5):
//
//   - A file that starts within a second of the end of the one before it
//     continues it: it is placed exactly at the end of what was kept of the
//     previous file, by media duration, so there is no hole.
//   - At the hourly rollover the old file can end with the frames the new
//     one starts with (both sessions delivered them). When the files overlap
//     on the wall clock by more than half a frame, the chunks at the end of
//     the old file that start at or after the new file's start are left out.
//   - Any other file is placed by its wall-clock distance, leaving the gap.
//
// The result is the first file's initialization segment followed by every
// kept chunk with its decode times moved to the joined timeline.
func (c *PlaybackClient) Stitch(files []File) ([]byte, []Placement, error) {
	var parts []stitchFile
	for _, f := range files {
		ix, err := c.Index(f)
		if err != nil {
			return nil, nil, err
		}
		sf := stitchFile{ix: ix}
		if sf.init, err = c.Range(f, ix.Init[0], ix.Init[1]); err != nil {
			return nil, nil, err
		}
		for _, ch := range ix.Chunks {
			data, err := c.Range(f, int64(ch[2]), int64(ch[3]))
			if err != nil {
				return nil, nil, err
			}
			sf.chunks = append(sf.chunks, data)
		}
		if len(sf.chunks) == 0 {
			return nil, nil, errors.New("a file has no chunks")
		}
		if len(parts) > 0 && ix.MIME != parts[0].ix.MIME {
			return nil, nil, fmt.Errorf("the files have different tracks (%s, %s): the player starts a new media source there", parts[0].ix.MIME, ix.MIME)
		}
		if sf.frame = frameDuration(sf.chunks[0]); sf.frame <= 0 {
			sf.frame = 3000
		}
		parts = append(parts, sf)
	}
	if len(parts) == 0 {
		return nil, nil, errors.New("no files")
	}

	scales := trackTimescales(parts[0].init)
	out := append([]byte(nil), parts[0].init...)
	places := make([]Placement, len(parts))
	var mediaStart int64 // 90 kHz
	for i := range parts {
		p := &parts[i]
		keep := len(p.chunks)
		if i+1 < len(parts) {
			next := parts[i+1]
			end := p.ix.Start.Add(time.Duration(p.ix.DurationMs * float64(time.Millisecond)))
			overlap := ticks(float64(end.Sub(next.ix.Start)) / float64(time.Millisecond))
			if overlap > p.frame/2 && overlap < 90000*5 {
				cut := ticks(p.ix.DurationMs) - overlap
				for keep > 1 && ticks(p.ix.Chunks[keep-1][0]) >= cut-p.frame {
					keep--
				}
			}
		}
		places[i].MediaStartMs = float64(mediaStart) / 90
		places[i].Chunks, places[i].Dropped = keep, len(p.chunks)-keep
		for _, data := range p.chunks[:keep] {
			shifted := append([]byte(nil), data...)
			shiftBaseTimes(shifted, mediaStart, scales)
			out = append(out, shifted...)
		}
		if i+1 == len(parts) {
			break
		}
		kept := ticks(p.ix.DurationMs)
		if keep < len(p.chunks) {
			kept = ticks(p.ix.Chunks[keep][0])
		}
		next := parts[i+1]
		keptEnd := p.ix.Start.Add(time.Duration(kept) * time.Second / 90000)
		if d := next.ix.Start.Sub(keptEnd); d > -time.Second && d < time.Second {
			mediaStart += kept
			places[i+1].Joined = true
		} else {
			mediaStart += ticks(float64(next.ix.Start.Sub(p.ix.Start)) / float64(time.Millisecond))
		}
	}
	return out, places, nil
}

// ---------------------------------------------------------------- MP4 boxes

// walk calls fn for every box in buf; payload excludes the 8-byte header.
func walk(buf []byte, fn func(typ string, payload []byte)) {
	for len(buf) >= 8 {
		size := int(binary.BigEndian.Uint32(buf))
		if size < 8 || size > len(buf) {
			return
		}
		fn(string(buf[4:8]), buf[8:size])
		buf = buf[size:]
	}
}

// trackTimescales reads the timescale of every track from an initialization
// segment.
func trackTimescales(init []byte) map[uint32]uint32 {
	out := map[uint32]uint32{}
	walk(init, func(typ string, moov []byte) {
		if typ != "moov" {
			return
		}
		walk(moov, func(typ string, trak []byte) {
			if typ != "trak" {
				return
			}
			var id, scale uint32
			walk(trak, func(typ string, p []byte) {
				switch typ {
				case "tkhd":
					off := 12
					if len(p) > 0 && p[0] == 1 {
						off = 20
					}
					if len(p) >= off+4 {
						id = binary.BigEndian.Uint32(p[off:])
					}
				case "mdia":
					walk(p, func(typ string, m []byte) {
						if typ != "mdhd" {
							return
						}
						off := 12
						if len(m) > 0 && m[0] == 1 {
							off = 20
						}
						if len(m) >= off+4 {
							scale = binary.BigEndian.Uint32(m[off:])
						}
					})
				}
			})
			if id != 0 && scale != 0 {
				out[id] = scale
			}
		})
	})
	return out
}

// shiftBaseTimes adds shift (90 kHz ticks) to the decode time of every track
// fragment in data, in place: what timestampOffset does in a browser.
func shiftBaseTimes(data []byte, shift int64, scales map[uint32]uint32) {
	walk(data, func(typ string, moof []byte) {
		if typ != "moof" {
			return
		}
		walk(moof, func(typ string, traf []byte) {
			if typ != "traf" {
				return
			}
			var id uint32
			walk(traf, func(typ string, p []byte) {
				switch typ {
				case "tfhd":
					if len(p) >= 8 {
						id = binary.BigEndian.Uint32(p[4:])
					}
				case "tfdt":
					scale := int64(scales[id])
					if scale == 0 {
						scale = 90000
					}
					add := uint64(shift * scale / 90000)
					if len(p) >= 12 && p[0] == 1 {
						binary.BigEndian.PutUint64(p[4:], binary.BigEndian.Uint64(p[4:])+add)
					} else if len(p) >= 8 {
						binary.BigEndian.PutUint32(p[4:], binary.BigEndian.Uint32(p[4:])+uint32(add))
					}
				}
			})
		})
	})
}

// frameDuration returns the duration of the first video sample of a chunk,
// in 90 kHz ticks, or 0 when it cannot be read.
func frameDuration(chunk []byte) int64 {
	var dur int64
	walk(chunk, func(typ string, moof []byte) {
		if typ != "moof" || dur != 0 {
			return
		}
		walk(moof, func(typ string, traf []byte) {
			if typ != "traf" {
				return
			}
			var id uint32
			walk(traf, func(typ string, p []byte) {
				switch typ {
				case "tfhd":
					if len(p) >= 8 {
						id = binary.BigEndian.Uint32(p[4:])
					}
				case "trun":
					if id != 1 || len(p) < 8 || dur != 0 {
						return
					}
					flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
					pos := 8
					if flags&0x1 != 0 {
						pos += 4
					}
					if flags&0x4 != 0 {
						pos += 4
					}
					if flags&0x100 != 0 && pos+4 <= len(p) {
						dur = int64(binary.BigEndian.Uint32(p[pos:]))
					}
				}
			})
		})
	})
	return dur
}
