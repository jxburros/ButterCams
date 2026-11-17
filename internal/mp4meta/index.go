package mp4meta

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// This file holds what playback needs from a recording (spec 16.5): the list
// of keyframe-aligned chunks with their byte ranges, the initialization
// segment, the codec strings a browser wants, and single keyframes. For a
// closed file all of it comes from the header and the mfra index at the end;
// for the file being written the fragment headers are walked instead.

// Chunk is a run of fragments that starts with a keyframe: the unit the
// player fetches with one range request and can start decoding at.
type Chunk struct {
	Time   uint64 // decode time of its first video sample, 90 kHz, from the start of the file
	Dur    uint64 // video duration, 90 kHz
	Offset int64  // of its first moof box
	Size   int64  // up to the next chunk, or the end of the fragments
}

// Index is the playback index of one recording.
type Index struct {
	// InitSize is the length of the initialization segment (ftyp + moov),
	// which starts at offset 0.
	InitSize int64
	// VideoInit is an initialization segment that declares the video track
	// only. Keyframe-only fragments and thumbnails are sent with it.
	VideoInit []byte

	Start  time.Time // wall-clock time of the first frame; zero when unknown
	Dur90k int64     // length of the video so far
	Chunks []Chunk
	// End is the offset just past the last complete fragment.
	End int64
	// Closed reports whether the file has its close marker: nothing will be
	// added to it.
	Closed bool

	VideoCodec string // "h264" or "h265"
	AudioCodec string // "aac", "opus" or ""
	// VideoCodecString and AudioCodecString are the RFC 6381 names for the
	// MIME type, for example "avc1.64001f" and "mp4a.40.2".
	VideoCodecString string
	AudioCodecString string
	Width, Height    int
}

// HasAudio reports whether the file has an audio track.
func (ix *Index) HasAudio() bool { return ix.AudioCodecString != "" }

// MIME returns the type string for MediaSource.addSourceBuffer.
func (ix *Index) MIME() string {
	if ix.HasAudio() {
		return `video/mp4; codecs="` + ix.VideoCodecString + ", " + ix.AudioCodecString + `"`
	}
	return ix.VideoMIME()
}

// VideoMIME returns the type string of the video track alone.
func (ix *Index) VideoMIME() string { return `video/mp4; codecs="` + ix.VideoCodecString + `"` }

// ErrNotReady is returned for a file whose header has not been written
// completely yet.
var ErrNotReady = errors.New("the recording has no complete header yet")

// ---------------------------------------------------------------- header

// ReadHeader reads the initialization segment of a recording into an Index
// without any chunks. End is set to the end of the header.
func ReadHeader(r io.ReaderAt, size int64) (*Index, error) {
	off := int64(0)
	for i := 0; i < 8; i++ {
		h, err := readHeader(r, off, size)
		if err != nil {
			return nil, ErrNotReady
		}
		if h.typ == "moof" || h.typ == "mdat" {
			break
		}
		if h.typ != "moov" {
			if h.offset+h.size > size {
				return nil, ErrNotReady
			}
			off = h.offset + h.size
			continue
		}
		end := h.offset + h.size
		if end > size {
			return nil, ErrNotReady
		}
		if h.size > maxMetaBox {
			return nil, fmt.Errorf("mp4meta: moov box too large")
		}
		init := make([]byte, end)
		if _, err := r.ReadAt(init, 0); err != nil {
			return nil, err
		}
		ix := &Index{InitSize: end, End: end}
		moov := init[h.offset+h.hdrSize:]
		var info Info
		parseMoov(moov, &info)
		ix.Start, ix.VideoCodec, ix.AudioCodec = info.Start, info.VideoCodec, info.AudioCodec
		parseTracks(moov, ix)
		if ix.VideoCodecString == "" {
			return nil, fmt.Errorf("mp4meta: the recording has no usable video track")
		}
		ix.VideoInit = videoOnlyInit(init, h.offset)
		return ix, nil
	}
	return nil, fmt.Errorf("mp4meta: no moov box found")
}

// parseTracks reads the sample entries of the tracks: codec strings and the
// picture size.
func parseTracks(moov []byte, ix *Index) {
	children(moov, func(typ string, trak []byte) {
		if typ != "trak" {
			return
		}
		mdia := find(trak, "mdia")
		hdlr := find(mdia, "hdlr")
		stsd := find(mdia, "minf", "stbl", "stsd")
		if len(hdlr) < 12 || len(stsd) < 16 {
			return
		}
		entrySize := int(binary.BigEndian.Uint32(stsd[8:]))
		if entrySize < 8 || 8+entrySize > len(stsd) {
			return
		}
		fourcc := string(stsd[12:16])
		entry := stsd[16 : 8+entrySize]
		switch string(hdlr[8:12]) {
		case "vide":
			if len(entry) < 78 {
				return
			}
			ix.Width = int(binary.BigEndian.Uint16(entry[24:]))
			ix.Height = int(binary.BigEndian.Uint16(entry[26:]))
			children(entry[78:], func(t string, p []byte) {
				switch t {
				case "avcC":
					if len(p) >= 4 {
						ix.VideoCodecString = fmt.Sprintf("%s.%02x%02x%02x", fourcc, p[1], p[2], p[3])
					}
				case "hvcC":
					if s := hevcCodecString(fourcc, p); s != "" {
						ix.VideoCodecString = s
					}
				}
			})
		case "soun":
			if len(entry) < 28 {
				return
			}
			switch fourcc {
			case "Opus", "opus":
				ix.AudioCodecString = "opus"
			case "mp4a":
				ix.AudioCodecString = "mp4a.40.2"
				children(entry[28:], func(t string, p []byte) {
					if t == "esds" {
						if aot := esdsObjectType(p); aot > 0 {
							ix.AudioCodecString = fmt.Sprintf("mp4a.40.%d", aot)
						}
					}
				})
			}
		}
	})
}

// hevcCodecString builds "hvc1.1.6.L93.B0" from an hvcC box (ISO/IEC
// 14496-15, annex E). The profile, tier, level and constraints are read from
// the SPS the box carries: some writers copy the box's own copy of them from
// the SPS without removing its emulation-prevention bytes, which gives
// strings browsers do not know.
func hevcCodecString(fourcc string, c []byte) string {
	if sps := hvccSPS(c); sps != nil {
		if s := HEVCCodecString(fourcc, sps); s != "" {
			return s
		}
	}
	if len(c) < 13 {
		return ""
	}
	return hevcString(fourcc, c[1:13])
}

// hvccSPS returns the first SPS in an hvcC box's parameter set arrays.
func hvccSPS(c []byte) []byte {
	if len(c) < 23 {
		return nil
	}
	n := int(c[22])
	p := c[23:]
	for i := 0; i < n; i++ {
		if len(p) < 3 {
			return nil
		}
		typ := p[0] & 0x3F
		count := int(binary.BigEndian.Uint16(p[1:]))
		p = p[3:]
		for j := 0; j < count; j++ {
			if len(p) < 2 {
				return nil
			}
			l := int(binary.BigEndian.Uint16(p))
			if len(p) < 2+l {
				return nil
			}
			if typ == 33 {
				return p[2 : 2+l]
			}
			p = p[2+l:]
		}
	}
	return nil
}

// HEVCCodecString builds the codec string of an H.265 stream from its SPS
// (NAL unit as sent, with emulation-prevention bytes), with the given sample
// entry name ("hvc1"). It returns "" for an SPS too short to read.
func HEVCCodecString(fourcc string, sps []byte) string {
	raw := h264.EmulationPreventionRemove(sps)
	// 2 bytes NAL header, 1 byte ids, then profile_tier_level: 12 bytes.
	if len(raw) < 15 {
		return ""
	}
	return hevcString(fourcc, raw[3:15])
}

// hevcString formats the 12 bytes of general_profile_tier_level: profile
// space and profile, the compatibility flags in reverse bit order, tier and
// level, then the constraint bytes without trailing zeros.
func hevcString(fourcc string, ptl []byte) string {
	space := ptl[0] >> 6
	tier := (ptl[0] >> 5) & 1
	profile := ptl[0] & 0x1F
	compat := binary.BigEndian.Uint32(ptl[1:5])
	var rev uint32 // the compatibility flags are written in reverse bit order
	for i := 0; i < 32; i++ {
		rev = rev<<1 | (compat>>i)&1
	}
	var b strings.Builder
	b.WriteString(fourcc)
	b.WriteByte('.')
	if space > 0 {
		b.WriteByte('A' + space - 1)
	}
	fmt.Fprintf(&b, "%d.%X.", profile, rev)
	if tier == 0 {
		b.WriteByte('L')
	} else {
		b.WriteByte('H')
	}
	fmt.Fprintf(&b, "%d", ptl[11])
	constraints := ptl[5:11]
	n := len(constraints)
	for n > 0 && constraints[n-1] == 0 {
		n--
	}
	for _, v := range constraints[:n] {
		fmt.Fprintf(&b, ".%X", v)
	}
	return b.String()
}

// esdsObjectType returns the MPEG-4 audio object type stored in an esds box
// (2 for AAC-LC), or 0 when it cannot be read.
func esdsObjectType(p []byte) int {
	if len(p) < 4 {
		return 0
	}
	p = p[4:] // version and flags
	// descriptor reads a tag and its length and returns the body.
	descriptor := func(b []byte) (tag byte, body, rest []byte, ok bool) {
		if len(b) < 2 {
			return 0, nil, nil, false
		}
		tag = b[0]
		n, i := 0, 1
		for ; i < len(b) && i <= 4; i++ {
			n = n<<7 | int(b[i]&0x7F)
			if b[i]&0x80 == 0 {
				i++
				break
			}
		}
		if i+n > len(b) {
			n = len(b) - i
		}
		return tag, b[i : i+n], b[i+n:], true
	}
	tag, es, _, ok := descriptor(p)
	if !ok || tag != 0x03 || len(es) < 3 {
		return 0
	}
	flags := es[2]
	es = es[3:]
	if flags&0x80 != 0 && len(es) >= 2 {
		es = es[2:]
	}
	if flags&0x40 != 0 && len(es) >= 1 && len(es) >= 1+int(es[0]) {
		es = es[1+int(es[0]):]
	}
	if flags&0x20 != 0 && len(es) >= 2 {
		es = es[2:]
	}
	tag, dc, _, ok := descriptor(es)
	if !ok || tag != 0x04 || len(dc) < 13 {
		return 0
	}
	tag, asc, _, ok := descriptor(dc[13:])
	if !ok || tag != 0x05 || len(asc) < 2 {
		return 0
	}
	aot := int(asc[0] >> 3)
	if aot == 31 {
		aot = 32 + int(asc[0]&0x07)<<3 + int(asc[1]>>5)
	}
	return aot
}

// videoOnlyInit returns a copy of the initialization segment without the
// audio track: the moov keeps only video trak boxes and their trex.
func videoOnlyInit(init []byte, moovOff int64) []byte {
	if moovOff+8 > int64(len(init)) {
		return nil
	}
	var kept [][]byte
	isVideo := map[uint32]bool{}
	moov := init[moovOff+8:]
	children(moov, func(typ string, payload []byte) {
		if typ != "trak" {
			return
		}
		tkhd := find(payload, "tkhd")
		hdlr := find(payload, "mdia", "hdlr")
		if len(hdlr) < 12 || len(tkhd) < 24 {
			return
		}
		idOff := 12 // version 0: flags, creation, modification, track id
		if tkhd[0] == 1 {
			idOff = 20
		}
		isVideo[binary.BigEndian.Uint32(tkhd[idOff:])] = string(hdlr[8:12]) == "vide"
	})
	children(moov, func(typ string, payload []byte) {
		switch typ {
		case "trak":
			hdlr := find(payload, "mdia", "hdlr")
			if len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
				return
			}
		case "mvex":
			var trex [][]byte
			children(payload, func(t string, p []byte) {
				if t == "trex" && (len(p) < 8 || !isVideo[binary.BigEndian.Uint32(p[4:])]) {
					return
				}
				trex = append(trex, box(t, p))
			})
			kept = append(kept, box("mvex", trex...))
			return
		}
		kept = append(kept, box(typ, payload))
	})
	out := append([]byte(nil), init[:moovOff]...)
	return append(out, box("moov", kept...)...)
}

// ---------------------------------------------------------------- closed files

// ReadIndex returns the playback index of a recording. For a cleanly closed
// file it reads the header and the mfra box at the end (two small reads plus
// the close marker); any other file is scanned fragment by fragment.
func ReadIndex(r io.ReaderAt, size int64) (*Index, error) {
	ix, err := ReadHeader(r, size)
	if err != nil {
		return nil, err
	}
	if readMfra(r, size, ix) {
		return ix, nil
	}
	ix.Chunks, ix.Dur90k, ix.End, ix.Closed = nil, 0, ix.InitSize, false
	ExtendIndex(r, size, ix)
	return ix, nil
}

// readMfra fills the chunk list from the random access index of a closed
// file. It reports false when the file has none that can be trusted.
func readMfra(r io.ReaderAt, size int64, ix *Index) bool {
	if size < ix.InitSize+16+TrailerSize {
		return false
	}
	var tail [16]byte
	if _, err := r.ReadAt(tail[:], size-16); err != nil ||
		binary.BigEndian.Uint32(tail[:4]) != 16 || string(tail[4:8]) != "mfro" {
		return false
	}
	mfraSize := int64(binary.BigEndian.Uint32(tail[12:16]))
	markerOff := size - mfraSize - TrailerSize
	if mfraSize < 24 || mfraSize > maxMetaBox || markerOff < ix.InitSize {
		return false
	}
	buf := make([]byte, TrailerSize+mfraSize)
	if _, err := r.ReadAt(buf, markerOff); err != nil {
		return false
	}
	if binary.BigEndian.Uint32(buf[:4]) != TrailerSize || string(buf[4:8]) != "free" {
		return false
	}
	_, dur, ok := parseTrailer(buf[8:TrailerSize])
	if !ok {
		return false
	}
	mfra := buf[TrailerSize:]
	if binary.BigEndian.Uint32(mfra[:4]) != uint32(mfraSize) || string(mfra[4:8]) != "mfra" {
		return false
	}
	tfra := find(mfra[8:], "tfra")
	if len(tfra) < 16 || tfra[0] != 1 || binary.BigEndian.Uint32(tfra[4:]) != VideoTrackID {
		return false
	}
	sizes := binary.BigEndian.Uint32(tfra[8:])
	entry := 16 + int(sizes>>4&3) + 1 + int(sizes>>2&3) + 1 + int(sizes&3) + 1
	n := int(binary.BigEndian.Uint32(tfra[12:]))
	if n < 0 || 16+n*entry > len(tfra) {
		return false
	}
	chunks := make([]Chunk, 0, n)
	for i := 0; i < n; i++ {
		e := tfra[16+i*entry:]
		c := Chunk{Time: binary.BigEndian.Uint64(e), Offset: int64(binary.BigEndian.Uint64(e[8:]))}
		if c.Offset < ix.InitSize || c.Offset >= markerOff {
			return false
		}
		if k := len(chunks); k > 0 {
			prev := &chunks[k-1]
			if c.Offset <= prev.Offset || c.Time < prev.Time {
				return false
			}
			prev.Size, prev.Dur = c.Offset-prev.Offset, c.Time-prev.Time
		}
		chunks = append(chunks, c)
	}
	if k := len(chunks); k > 0 {
		last := &chunks[k-1]
		last.Size = markerOff - last.Offset
		if uint64(dur) > last.Time {
			last.Dur = uint64(dur) - last.Time
		}
	}
	ix.Chunks, ix.Dur90k, ix.End, ix.Closed = chunks, dur, markerOff, true
	return true
}

// ---------------------------------------------------------------- growing files

// ScannedFragment is one complete fragment (moof + mdat) found by
// ScanFragments.
type ScannedFragment struct {
	Offset   int64 // of the moof
	Size     int64 // moof and mdat together
	HasVideo bool
	Time     uint64 // decode time of the first video sample, 90 kHz
	Dur      uint64 // video duration
	Key      bool   // the first video sample is a keyframe
}

// ScanFragments walks the box headers of a recording from offset from, which
// must be the start of a top-level box, and returns the complete fragments
// it finds. size is the length of the file as far as it may be read.
//
// A fragment is returned only when its moof and its whole mdat lie inside
// size, so a fragment the writer is still working on is never listed. next
// is where the following scan continues. closed is true when the close
// marker was reached: the file is finished and next is the marker's offset.
func ScanFragments(r io.ReaderAt, from, size int64) (frags []ScannedFragment, next int64, closed bool) {
	next = from
	for {
		h, err := readHeader(r, next, size)
		if err != nil {
			return frags, next, false
		}
		switch h.typ {
		case "moof":
			moofEnd := h.offset + h.size
			if moofEnd > size || h.size > maxMetaBox {
				return frags, next, false
			}
			m, err := readHeader(r, moofEnd, size)
			if err != nil || m.typ != "mdat" || m.offset+m.size > size {
				return frags, next, false
			}
			buf := make([]byte, h.size-h.hdrSize)
			if _, err := r.ReadAt(buf, h.offset+h.hdrSize); err != nil {
				return frags, next, false
			}
			fi := parseMoof(buf)
			frags = append(frags, ScannedFragment{Offset: h.offset, Size: h.size + m.size,
				HasVideo: fi.hasVideo, Time: fi.base, Dur: fi.duration, Key: fi.hasVideo && fi.firstSync})
			next = m.offset + m.size
		case "free":
			if h.size == TrailerSize && h.offset+h.size <= size {
				buf := make([]byte, TrailerSize-8)
				if _, err := r.ReadAt(buf, h.offset+8); err == nil {
					if _, _, ok := parseTrailer(buf); ok {
						return frags, next, true
					}
				}
			}
			if h.offset+h.size > size {
				return frags, next, false
			}
			next = h.offset + h.size
		case "mfra":
			return frags, next, true
		default:
			// An mdat without its moof, or anything unknown: skip it when it
			// is complete.
			if h.offset+h.size > size {
				return frags, next, false
			}
			next = h.offset + h.size
		}
	}
}

// ExtendIndex continues the index of a file that is being written: it scans
// from ix.End up to size and appends what is complete. Fragments that do not
// start with a keyframe extend the chunk before them.
func ExtendIndex(r io.ReaderAt, size int64, ix *Index) {
	if ix.Closed || size <= ix.End {
		return
	}
	frags, next, closed := ScanFragments(r, ix.End, size)
	for _, f := range frags {
		if f.HasVideo && f.Key {
			ix.Chunks = append(ix.Chunks, Chunk{Time: f.Time, Dur: f.Dur, Offset: f.Offset, Size: f.Size})
		} else if n := len(ix.Chunks); n > 0 {
			c := &ix.Chunks[n-1]
			c.Size = f.Offset + f.Size - c.Offset
			if f.HasVideo && f.Time+f.Dur > c.Time {
				c.Dur = f.Time + f.Dur - c.Time
			}
		}
		// A fragment before the first keyframe cannot be decoded; it is left
		// out. The recorder never writes one.
		if f.HasVideo {
			if e := int64(f.Time + f.Dur); e > ix.Dur90k {
				ix.Dur90k = e
			}
		}
	}
	ix.End, ix.Closed = next, closed
}

// ---------------------------------------------------------------- keyframes

// firstVideoSample finds the first sample of the video track in a moof box
// (payload without the box header): its offset from the start of the moof
// and its size.
func firstVideoSample(moof []byte) (dataOff int64, size uint32, ok bool) {
	children(moof, func(typ string, traf []byte) {
		if typ != "traf" || ok {
			return
		}
		var trackID, defSize uint32
		var baseOff int64
		found := false
		children(traf, func(t string, p []byte) {
			switch t {
			case "tfhd":
				if len(p) < 8 {
					return
				}
				flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				trackID = binary.BigEndian.Uint32(p[4:])
				pos := 8
				if flags&0x1 != 0 { // base data offset: not written by the recorder
					pos += 8
				}
				if flags&0x2 != 0 {
					pos += 4
				}
				if flags&0x8 != 0 {
					pos += 4
				}
				if flags&0x10 != 0 && pos+4 <= len(p) {
					defSize = binary.BigEndian.Uint32(p[pos:])
				}
			case "trun":
				if found || len(p) < 8 {
					return
				}
				flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				if binary.BigEndian.Uint32(p[4:]) == 0 {
					return
				}
				pos := 8
				if flags&0x1 != 0 {
					if pos+4 > len(p) {
						return
					}
					baseOff = int64(int32(binary.BigEndian.Uint32(p[pos:])))
					pos += 4
				}
				if flags&0x4 != 0 {
					pos += 4
				}
				if flags&0x100 != 0 {
					pos += 4
				}
				size = defSize
				if flags&0x200 != 0 {
					if pos+4 > len(p) {
						return
					}
					size = binary.BigEndian.Uint32(p[pos:])
				}
				found = true
			}
		})
		if trackID == VideoTrackID && found && size > 0 {
			dataOff, ok = baseOff, true
		}
	})
	return dataOff, size, ok
}

// MaxKeyframeSize bounds one keyframe read for a thumbnail or for
// keyframe-only playback.
const MaxKeyframeSize = 16 << 20

// KeyframeFragment builds a fragment that holds only the first frame of a
// chunk, a keyframe, lasting as long as the whole chunk (spec 16.5). The
// result is one moof + mdat for the video track, to be appended after an
// initialization segment.
func KeyframeFragment(r io.ReaderAt, c Chunk, seq uint32) ([]byte, error) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], c.Offset); err != nil {
		return nil, err
	}
	moofSize := int64(binary.BigEndian.Uint32(hdr[:4]))
	if string(hdr[4:8]) != "moof" || moofSize < 8 || moofSize > maxMetaBox || moofSize > c.Size {
		return nil, fmt.Errorf("mp4meta: no fragment at offset %d", c.Offset)
	}
	moof := make([]byte, moofSize-8)
	if _, err := r.ReadAt(moof, c.Offset+8); err != nil {
		return nil, err
	}
	off, size, ok := firstVideoSample(moof)
	if !ok || off < moofSize || off+int64(size) > c.Size || size > MaxKeyframeSize {
		return nil, fmt.Errorf("mp4meta: the fragment at offset %d has no readable keyframe", c.Offset)
	}
	dur := c.Dur
	if dur == 0 {
		dur = 1
	}
	if dur > 0xFFFFFFFF {
		dur = 0xFFFFFFFF
	}
	out := AppendFragmentHeader(make([]byte, 0, 128+int(size)), seq, []FragTrack{{
		ID: VideoTrackID, BaseTime: c.Time, Samples: []FragSample{{Duration: uint32(dur), Size: size}},
	}})
	n := len(out)
	out = out[:n+int(size)]
	if _, err := r.ReadAt(out[n:], c.Offset+off); err != nil {
		return nil, err
	}
	return out, nil
}
