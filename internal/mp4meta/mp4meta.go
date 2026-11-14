// Package mp4meta reads and writes the small pieces of MP4 structure that
// ButterCams adds around the fragments produced by the fMP4 writer:
//
//   - a udta/meta/ilst metadata box in the moov with the keys
//     buttercams:start, buttercams:camera and buttercams:gap (spec 6.2);
//   - a 128-byte "free" box written when a file is closed, holding the close
//     reason and the video duration, followed by a standard "mfra" random
//     access index.
//
// Both are ordinary ISO BMFF boxes that players skip or use; the files stay
// plain fragmented MP4. Together they let the index be rebuilt from the files
// alone with three small reads per file (spec 10.4), and they let a file cut
// off by a crash be repaired by truncating it after its last whole fragment.
package mp4meta

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Metadata keys stored in the moov.
const (
	KeyStart  = "buttercams:start"
	KeyCamera = "buttercams:camera"
	KeyGap    = "buttercams:gap"
)

// TimeFormat is the format of buttercams:start: RFC 3339 with milliseconds.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// VideoTimescale is the timescale of the video track (the RTP clock rate of
// H.264 and H.265).
const VideoTimescale = 90000

// VideoTrackID and AudioTrackID are the track numbers the writer uses.
const (
	VideoTrackID = 1
	AudioTrackID = 2
)

// TrailerSize is the size of the close-marker "free" box.
const TrailerSize = 128

const trailerMagic = "buttercams:close="

// ErrNoFragments is returned for a file that holds no complete fragment.
var ErrNoFragments = errors.New("file contains no complete video fragment")

// TicksToDuration converts 90 kHz ticks to a duration.
func TicksToDuration(ticks int64) time.Duration {
	return time.Duration(ticks/VideoTimescale)*time.Second +
		time.Duration(ticks%VideoTimescale)*time.Second/VideoTimescale
}

// TruncateStart rounds a wall-clock time to the precision stored in files and
// in the index, so both always agree.
func TruncateStart(t time.Time) time.Time {
	return t.Round(0).Truncate(time.Millisecond)
}

// ---------------------------------------------------------------- box writing

func box(typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint32(out, uint32(n))
	copy(out[4:], typ)
	for _, p := range payload {
		out = append(out, p...)
	}
	return out
}

func ilstItem(key, value string) []byte {
	zero4 := []byte{0, 0, 0, 0}
	return box("----",
		box("mean", zero4, []byte("com.apple.iTunes")),
		box("name", zero4, []byte(key)),
		box("data", []byte{0, 0, 0, 1}, zero4, []byte(value)), // type 1 = UTF-8
	)
}

// Udta builds the udta box that carries the ButterCams metadata as
// iTunes-style free-form tags, the form ffprobe, VLC and ExifTool display.
func Udta(cameraID string, start time.Time, gap bool) []byte {
	g := "0"
	if gap {
		g = "1"
	}
	hdlr := box("hdlr",
		[]byte{0, 0, 0, 0}, // version, flags
		[]byte{0, 0, 0, 0}, // pre_defined
		[]byte("mdir"),
		[]byte("appl"), []byte{0, 0, 0, 0, 0, 0, 0, 0},
		[]byte{0}, // empty name
	)
	ilst := box("ilst",
		ilstItem(KeyStart, start.Format(TimeFormat)),
		ilstItem(KeyCamera, cameraID),
		ilstItem(KeyGap, g),
	)
	meta := box("meta", []byte{0, 0, 0, 0}, hdlr, ilst)
	return box("udta", meta)
}

// AppendToMoov appends child boxes to the moov of an initialization segment
// (ftyp + moov) and fixes the moov size.
func AppendToMoov(init []byte, child []byte) ([]byte, error) {
	off := 0
	for off+8 <= len(init) {
		size := int(binary.BigEndian.Uint32(init[off:]))
		typ := string(init[off+4 : off+8])
		if size < 8 || off+size > len(init) {
			break
		}
		if typ == "moov" {
			if off+size != len(init) {
				return nil, fmt.Errorf("mp4meta: moov is not the last box of the init segment")
			}
			out := make([]byte, 0, len(init)+len(child))
			out = append(out, init...)
			out = append(out, child...)
			binary.BigEndian.PutUint32(out[off:], uint32(size+len(child)))
			return out, nil
		}
		off += size
	}
	return nil, fmt.Errorf("mp4meta: init segment has no moov box")
}

// Fragment describes one written fragment for the random access index.
type Fragment struct {
	Time       uint64 // decode time of its first video sample, 90 kHz
	MoofOffset uint64
}

// Trailer builds the bytes appended when a file is closed: the close-marker
// "free" box followed by an "mfra" index of the fragments that start with a
// keyframe. mfra must be the last box of the file, so the marker precedes it.
func Trailer(reason string, dur90k int64, frags []Fragment) []byte {
	text := trailerMagic + reason + ";dur=" + strconv.FormatInt(dur90k, 10) + ";"
	payload := bytes.Repeat([]byte{' '}, TrailerSize-8)
	copy(payload, text)
	free := box("free", payload)

	tfra := make([]byte, 0, 12+len(frags)*19)
	tfra = append(tfra, 1, 0, 0, 0) // version 1, flags 0
	tfra = binary.BigEndian.AppendUint32(tfra, VideoTrackID)
	tfra = binary.BigEndian.AppendUint32(tfra, 0) // 1-byte traf/trun/sample numbers
	tfra = binary.BigEndian.AppendUint32(tfra, uint32(len(frags)))
	for _, f := range frags {
		tfra = binary.BigEndian.AppendUint64(tfra, f.Time)
		tfra = binary.BigEndian.AppendUint64(tfra, f.MoofOffset)
		tfra = append(tfra, 1, 1, 1)
	}
	tfraBox := box("tfra", tfra)
	mfraSize := uint32(8 + len(tfraBox) + 16)
	mfro := box("mfro", []byte{0, 0, 0, 0}, binary.BigEndian.AppendUint32(nil, mfraSize))
	mfra := box("mfra", tfraBox, mfro)

	return append(free, mfra...)
}

// ---------------------------------------------------------------- box reading

type boxHeader struct {
	typ     string
	offset  int64 // of the box
	size    int64 // whole box
	hdrSize int64
}

// readHeader reads the box header at off. It returns io.EOF at the exact end
// of the file and io.ErrUnexpectedEOF for a cut-off header.
func readHeader(r io.ReaderAt, off, fileSize int64) (boxHeader, error) {
	if off == fileSize {
		return boxHeader{}, io.EOF
	}
	if off+8 > fileSize {
		return boxHeader{}, io.ErrUnexpectedEOF
	}
	var b [16]byte
	if _, err := r.ReadAt(b[:8], off); err != nil {
		return boxHeader{}, err
	}
	h := boxHeader{typ: string(b[4:8]), offset: off, hdrSize: 8}
	size := int64(binary.BigEndian.Uint32(b[:4]))
	switch size {
	case 0:
		size = fileSize - off
	case 1:
		if off+16 > fileSize {
			return boxHeader{}, io.ErrUnexpectedEOF
		}
		if _, err := r.ReadAt(b[8:16], off+8); err != nil {
			return boxHeader{}, err
		}
		size = int64(binary.BigEndian.Uint64(b[8:16]))
		h.hdrSize = 16
	}
	if size < h.hdrSize {
		return boxHeader{}, fmt.Errorf("mp4meta: invalid box size at offset %d", off)
	}
	h.size = size
	return h, nil
}

// children iterates the child boxes inside buf.
func children(buf []byte, fn func(typ string, payload []byte)) {
	for len(buf) >= 8 {
		size := int(binary.BigEndian.Uint32(buf))
		if size < 8 || size > len(buf) {
			return
		}
		fn(string(buf[4:8]), buf[8:size])
		buf = buf[size:]
	}
}

func find(buf []byte, path ...string) []byte {
	for _, want := range path {
		var next []byte
		found := false
		children(buf, func(typ string, payload []byte) {
			if !found && typ == want {
				next, found = payload, true
			}
		})
		if !found {
			return nil
		}
		if want == "meta" && len(next) >= 4 {
			next = next[4:] // meta is a full box
		}
		buf = next
	}
	return buf
}

// Info is what ButterCams knows about a recording from the file alone.
type Info struct {
	CameraID    string
	Start       time.Time
	Gap         bool
	VideoCodec  string
	AudioCodec  string
	Dur90k      int64
	CloseReason string // empty when the file has no close marker
	Size        int64
}

// Duration returns the length of the video track.
func (i Info) Duration() time.Duration { return TicksToDuration(i.Dur90k) }

// End returns the wall-clock end of the recording, to the millisecond.
func (i Info) End() time.Time { return EndOf(i.Start, i.Dur90k) }

// EndOf returns the end time of a recording that starts at start and holds
// dur90k ticks of video, at the millisecond precision used everywhere.
func EndOf(start time.Time, dur90k int64) time.Time {
	return start.Add(TicksToDuration(dur90k)).Truncate(time.Millisecond)
}

func codecName(fourcc string) string {
	switch fourcc {
	case "avc1", "avc3":
		return "h264"
	case "hvc1", "hev1":
		return "h265"
	case "mp4a":
		return "aac"
	case "Opus", "opus":
		return "opus"
	}
	return strings.TrimSpace(strings.ToLower(fourcc))
}

func parseMoov(moov []byte, info *Info) {
	children(moov, func(typ string, payload []byte) {
		switch typ {
		case "trak":
			mdia := find(payload, "mdia")
			hdlr := find(mdia, "hdlr")
			stsd := find(mdia, "minf", "stbl", "stsd")
			if len(hdlr) < 12 || len(stsd) < 16 {
				return
			}
			handler := string(hdlr[8:12])
			fourcc := codecName(string(stsd[12:16]))
			switch handler {
			case "vide":
				info.VideoCodec = fourcc
			case "soun":
				info.AudioCodec = fourcc
			}
		case "udta":
			ilst := find(payload, "meta", "ilst")
			children(ilst, func(typ string, item []byte) {
				if typ != "----" {
					return
				}
				var name, value string
				children(item, func(t string, p []byte) {
					switch t {
					case "name":
						if len(p) >= 4 {
							name = string(p[4:])
						}
					case "data":
						if len(p) >= 8 {
							value = string(p[8:])
						}
					}
				})
				switch name {
				case KeyCamera:
					info.CameraID = value
				case KeyStart:
					if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
						info.Start = t
					}
				case KeyGap:
					info.Gap = value == "1"
				}
			})
		}
	})
}

// fragInfo is the video part of one moof.
type fragInfo struct {
	hasVideo  bool
	base      uint64
	duration  uint64
	firstSync bool
}

func parseMoof(moof []byte) fragInfo {
	var fi fragInfo
	children(moof, func(typ string, traf []byte) {
		if typ != "traf" {
			return
		}
		var trackID uint32
		var base, dur uint64
		firstSync := true
		children(traf, func(t string, p []byte) {
			switch t {
			case "tfhd":
				if len(p) >= 8 {
					trackID = binary.BigEndian.Uint32(p[4:])
				}
			case "tfdt":
				if len(p) >= 12 && p[0] == 1 {
					base = binary.BigEndian.Uint64(p[4:])
				} else if len(p) >= 8 {
					base = uint64(binary.BigEndian.Uint32(p[4:]))
				}
			case "trun":
				if len(p) < 8 {
					return
				}
				flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				count := int(binary.BigEndian.Uint32(p[4:]))
				pos := 8
				if flags&0x1 != 0 {
					pos += 4
				}
				if flags&0x4 != 0 {
					if pos+4 <= len(p) {
						firstSync = binary.BigEndian.Uint32(p[pos:])&0x10000 == 0
					}
					pos += 4
				}
				for i := 0; i < count; i++ {
					if flags&0x100 != 0 {
						if pos+4 > len(p) {
							return
						}
						dur += uint64(binary.BigEndian.Uint32(p[pos:]))
						pos += 4
					}
					if flags&0x200 != 0 {
						pos += 4
					}
					if flags&0x400 != 0 {
						if i == 0 && pos+4 <= len(p) {
							firstSync = binary.BigEndian.Uint32(p[pos:])&0x10000 == 0
						}
						pos += 4
					}
					if flags&0x800 != 0 {
						pos += 4
					}
				}
			}
		})
		if trackID == VideoTrackID {
			fi = fragInfo{hasVideo: true, base: base, duration: dur, firstSync: firstSync}
		}
	})
	return fi
}

type scanResult struct {
	info    Info
	frags   []Fragment
	goodEnd int64 // offset just past the last complete fragment (or the moov)
	hasMoov bool
	nFrags  int
}

const maxMetaBox = 16 << 20

// scan walks the top-level boxes of a recording.
func scan(r io.ReaderAt, fileSize int64) (scanResult, error) {
	var res scanResult
	res.info.Size = fileSize
	off := int64(0)
	var pending *fragInfo // moof seen, waiting for its mdat
	var pendingOff int64
	for {
		h, err := readHeader(r, off, fileSize)
		if err != nil {
			break // EOF or a cut-off / corrupt header: stop at the last good box
		}
		end := h.offset + h.size
		complete := end <= fileSize
		switch h.typ {
		case "moov":
			if !complete || h.size > maxMetaBox {
				return res, fmt.Errorf("mp4meta: incomplete moov box")
			}
			buf := make([]byte, h.size-h.hdrSize)
			if _, err := r.ReadAt(buf, h.offset+h.hdrSize); err != nil {
				return res, err
			}
			parseMoov(buf, &res.info)
			res.hasMoov = true
			res.goodEnd = end
		case "moof":
			if !complete || h.size > maxMetaBox {
				return res, nil
			}
			buf := make([]byte, h.size-h.hdrSize)
			if _, err := r.ReadAt(buf, h.offset+h.hdrSize); err != nil {
				return res, nil
			}
			fi := parseMoof(buf)
			pending, pendingOff = &fi, h.offset
		case "mdat":
			if !complete {
				return res, nil
			}
			if pending != nil {
				if pending.hasVideo {
					if e := int64(pending.base + pending.duration); e > res.info.Dur90k {
						res.info.Dur90k = e
					}
					if pending.firstSync {
						res.frags = append(res.frags, Fragment{Time: pending.base, MoofOffset: uint64(pendingOff)})
					}
				}
				res.nFrags++
				res.goodEnd = end
				pending = nil
			}
		case "free":
			if complete && h.size == TrailerSize {
				buf := make([]byte, TrailerSize-8)
				if _, err := r.ReadAt(buf, h.offset+8); err == nil {
					if reason, _, ok := parseTrailer(buf); ok {
						res.info.CloseReason = reason
					}
				}
			}
		}
		if !complete {
			return res, nil
		}
		off = end
	}
	if !res.hasMoov {
		return res, fmt.Errorf("mp4meta: no moov box found")
	}
	return res, nil
}

func parseTrailer(payload []byte) (reason string, dur int64, ok bool) {
	s := string(payload)
	if !strings.HasPrefix(s, trailerMagic) {
		return "", 0, false
	}
	for _, part := range strings.Split(strings.TrimSpace(s), ";") {
		k, v, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		switch k {
		case "buttercams:close":
			reason = v
		case "dur":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return "", 0, false
			}
			dur = n
		}
	}
	return reason, dur, reason != ""
}

// readInit parses the moov at the start of the file.
func readInit(r io.ReaderAt, fileSize int64) (Info, error) {
	info := Info{Size: fileSize}
	off := int64(0)
	for i := 0; i < 8; i++ {
		h, err := readHeader(r, off, fileSize)
		if err != nil {
			break
		}
		if h.typ == "moov" {
			if h.offset+h.size > fileSize || h.size > maxMetaBox {
				return info, fmt.Errorf("mp4meta: incomplete moov box")
			}
			buf := make([]byte, h.size-h.hdrSize)
			if _, err := r.ReadAt(buf, h.offset+h.hdrSize); err != nil {
				return info, err
			}
			parseMoov(buf, &info)
			return info, nil
		}
		if h.typ == "moof" || h.typ == "mdat" {
			break
		}
		off = h.offset + h.size
	}
	return info, fmt.Errorf("mp4meta: no moov box found")
}

// Read returns the metadata of a recording. For a cleanly closed file it
// needs three small reads (moov, mfro, close marker); otherwise it falls back
// to walking every fragment.
func Read(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	return ReadFrom(f, st.Size())
}

// ReadFrom is Read for an already open file.
func ReadFrom(r io.ReaderAt, size int64) (Info, error) {
	info, err := readInit(r, size)
	if err != nil {
		return info, err
	}
	// Fast path: the last 16 bytes are the mfro box, which gives the size of
	// the mfra box; the close marker sits right before it.
	if size >= 16+TrailerSize {
		var tail [16]byte
		if _, err := r.ReadAt(tail[:], size-16); err == nil &&
			binary.BigEndian.Uint32(tail[:4]) == 16 && string(tail[4:8]) == "mfro" {
			mfraSize := int64(binary.BigEndian.Uint32(tail[12:16]))
			markerOff := size - mfraSize - TrailerSize
			if mfraSize >= 24 && markerOff > 0 {
				buf := make([]byte, TrailerSize)
				if _, err := r.ReadAt(buf, markerOff); err == nil &&
					binary.BigEndian.Uint32(buf[:4]) == TrailerSize && string(buf[4:8]) == "free" {
					if reason, dur, ok := parseTrailer(buf[8:]); ok {
						info.CloseReason, info.Dur90k = reason, dur
						return info, nil
					}
				}
			}
		}
	}
	res, err := scan(r, size)
	if err != nil {
		return info, err
	}
	return res.info, nil
}

// Repair makes a file that was cut off mid-write (crash, power loss) a
// well-formed recording: it removes anything after the last complete
// fragment, appends the close marker with the given reason and the mfra
// index, and returns the metadata and the SHA-256 of the result.
//
// It returns ErrNoFragments when the file holds no complete fragment; the
// caller then deletes it.
func Repair(path, reason string) (Info, string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return Info{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, "", err
	}
	res, err := scan(f, st.Size())
	if err != nil {
		return res.info, "", err
	}
	if res.nFrags == 0 || res.info.Dur90k == 0 {
		return res.info, "", ErrNoFragments
	}
	if err := f.Truncate(res.goodEnd); err != nil {
		return res.info, "", err
	}
	trailer := Trailer(reason, res.info.Dur90k, res.frags)
	if _, err := f.WriteAt(trailer, res.goodEnd); err != nil {
		return res.info, "", err
	}
	if err := f.Sync(); err != nil {
		return res.info, "", err
	}
	res.info.CloseReason = reason
	res.info.Size = res.goodEnd + int64(len(trailer))

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return res.info, "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return res.info, "", err
	}
	return res.info, hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile returns the SHA-256 and size of a file.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FragSample describes one sample of a fragment.
type FragSample struct {
	Duration  uint32
	Size      uint32
	PTSOffset int32
	NonSync   bool
}

// FragTrack is the part of a fragment that belongs to one track.
type FragTrack struct {
	ID       uint32
	BaseTime uint64 // decode time of the first sample, in the track's timescale
	Samples  []FragSample
}

const sampleFlagNonSync = 1 << 16

// AppendFragmentHeader appends a moof box and the header of the mdat box that
// follows it. The caller then writes the sample data itself, track after
// track in the order given, so the (large) video data is never copied into an
// intermediate buffer. The layout is the one the mediacommon fMP4 writer
// produces: one traf per track with tfhd (default-base-is-moof), tfdt
// (version 1) and one trun.
func AppendFragmentHeader(dst []byte, seq uint32, tracks []FragTrack) []byte {
	type plan struct {
		flags     uint32
		entrySize int
		trunSize  int
		dataSize  int
	}
	plans := make([]plan, len(tracks))
	moofSize := 8 + 16
	dataTotal := 0
	for i, t := range tracks {
		p := plan{flags: 0x000001 | 0x000100 | 0x000200, entrySize: 8}
		for _, s := range t.Samples {
			if s.NonSync && p.flags&0x000400 == 0 {
				p.flags |= 0x000400
				p.entrySize += 4
			}
			if s.PTSOffset != 0 && p.flags&0x000800 == 0 {
				p.flags |= 0x000800
				p.entrySize += 4
			}
			p.dataSize += int(s.Size)
		}
		p.trunSize = 8 + 4 + 4 + 4 + len(t.Samples)*p.entrySize
		plans[i] = p
		moofSize += 8 + 16 + 20 + p.trunSize
		dataTotal += p.dataSize
	}

	be := binary.BigEndian
	start := len(dst)
	dst = be.AppendUint32(dst, uint32(moofSize))
	dst = append(dst, "moof"...)
	dst = be.AppendUint32(dst, 16)
	dst = append(dst, "mfhd"...)
	dst = be.AppendUint32(dst, 0)
	dst = be.AppendUint32(dst, seq)

	offset := moofSize + 8 // first sample byte, relative to the start of the moof
	for i, t := range tracks {
		p := plans[i]
		dst = be.AppendUint32(dst, uint32(8+16+20+p.trunSize))
		dst = append(dst, "traf"...)

		dst = be.AppendUint32(dst, 16)
		dst = append(dst, "tfhd"...)
		dst = be.AppendUint32(dst, 0x020000) // default-base-is-moof
		dst = be.AppendUint32(dst, t.ID)

		dst = be.AppendUint32(dst, 20)
		dst = append(dst, "tfdt"...)
		dst = be.AppendUint32(dst, 1<<24) // version 1
		dst = be.AppendUint64(dst, t.BaseTime)

		dst = be.AppendUint32(dst, uint32(p.trunSize))
		dst = append(dst, "trun"...)
		dst = be.AppendUint32(dst, 1<<24|p.flags) // version 1: signed composition offsets
		dst = be.AppendUint32(dst, uint32(len(t.Samples)))
		dst = be.AppendUint32(dst, uint32(offset))
		for _, s := range t.Samples {
			dst = be.AppendUint32(dst, s.Duration)
			dst = be.AppendUint32(dst, s.Size)
			if p.flags&0x000400 != 0 {
				var f uint32
				if s.NonSync {
					f = sampleFlagNonSync
				}
				dst = be.AppendUint32(dst, f)
			}
			if p.flags&0x000800 != 0 {
				dst = be.AppendUint32(dst, uint32(s.PTSOffset))
			}
		}
		offset += p.dataSize
	}
	if len(dst)-start != moofSize {
		panic("mp4meta: moof size miscalculated") // a programming error, caught by the first test
	}
	dst = be.AppendUint32(dst, uint32(8+dataTotal))
	dst = append(dst, "mdat"...)
	return dst
}
