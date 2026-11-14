package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
)

// Fragmenting and flushing (spec 6.2).
const (
	// MaxFragment forces a fragment when no keyframe arrives for this long.
	MaxFragment = 4 * time.Second
	// SyncInterval is the longest time written data may stay unflushed.
	SyncInterval = 2 * time.Second

	maxFragmentTicks = int64(MaxFragment / time.Second * mp4meta.VideoTimescale)
	defaultFrameDur  = 3000 // 1/30 s at 90 kHz, used only for a one-frame file
)

// videoParams are the codec parameter sets a file is opened with.
type videoParams struct {
	codec string // "h264" or "h265"
	vps   []byte
	sps   []byte
	pps   []byte
}

func (p videoParams) complete() bool {
	if p.codec == "h265" {
		return len(p.vps) > 0 && len(p.sps) > 0 && len(p.pps) > 0
	}
	return len(p.sps) > 0 && len(p.pps) > 0
}

func (p videoParams) equal(o videoParams) bool {
	return p.codec == o.codec && bytes.Equal(p.vps, o.vps) && bytes.Equal(p.sps, o.sps) && bytes.Equal(p.pps, o.pps)
}

func (p videoParams) mp4Codec() codecs.Codec {
	if p.codec == "h265" {
		return &codecs.H265{VPS: p.vps, SPS: p.sps, PPS: p.pps}
	}
	return &codecs.H264{SPS: p.sps, PPS: p.pps}
}

// audioParams describe the audio track of a file.
type audioParams struct {
	name      string // "aac" or "opus": what is stored in the file
	codec     codecs.Codec
	timescale uint32
}

// sample is one access unit ready to be written.
type sample struct {
	video bool
	pts   int64 // track timescale
	dts   int64
	dur   uint32 // audio only: nominal duration
	key   bool
	data  []byte
	recv  time.Time
}

// closedSegment is the result of closing a file.
type closedSegment struct {
	path       string
	start      time.Time
	end        time.Time
	bytes      int64
	sha256     string
	gap        bool
	videoCodec string
	audioCodec string
}

// segment writes one recording file: ftyp + moov, then one moof/mdat fragment
// per keyframe interval. Every byte written also goes through SHA-256, so the
// file never has to be read back to be hashed.
type segment struct {
	f         *os.File
	partPath  string
	finalPath string
	cameraID  string
	start     time.Time
	gap       bool
	hourEnd   time.Time

	vp       videoParams
	ap       *audioParams
	audioOff bool // audio failed: dropped for the rest of this file

	hash  hash.Hash
	bytes int64
	seq   uint32
	buf   seekablebuffer.Buffer
	frags []mp4meta.Fragment

	// video timeline, 90 kHz, relative to the first sample of the file
	vOrigin   int64 // session DTS of file time zero
	vCount    int
	vPending  []*fmp4.Sample
	last      *fmp4.Sample
	lastRel   int64
	lastDur   int64
	fragStart int64
	fragKey   bool

	// audio timeline, audio timescale, relative to the same instant
	aPending  []*fmp4.Sample
	aLast     *fmp4.Sample
	aLastT    int64
	aLastNom  uint32
	aFragBase int64
	aEnd      int64

	lastSync time.Time
	now      func() time.Time
}

// openSegment creates <staging>/<camera>/<date>/<name>.part.mp4 and writes
// the initialization segment. taken reports names (closed form) that must not
// be reused even though no such file is on disk any more.
func openSegment(staging, cameraID string, start time.Time, vp videoParams, ap *audioParams, gap bool,
	taken func(finalName string) bool) (*segment, error) {
	start = mp4meta.TruncateStart(start).In(time.Local)
	hour := naming.HourStart(start)
	dir := filepath.Join(staging, naming.RelDir(cameraID, hour.Format("2006-01-02")))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	name := naming.NextFree(cameraID, start, func(final string) bool {
		return exists(filepath.Join(dir, final)) || exists(filepath.Join(dir, naming.PartName(final))) ||
			(taken != nil && taken(final))
	})

	tracks := []*fmp4.InitTrack{{ID: mp4meta.VideoTrackID, TimeScale: mp4meta.VideoTimescale, Codec: vp.mp4Codec()}}
	if ap != nil {
		tracks = append(tracks, &fmp4.InitTrack{ID: mp4meta.AudioTrackID, TimeScale: ap.timescale, Codec: ap.codec})
	}
	var ib seekablebuffer.Buffer
	if err := (fmp4.Init{Tracks: tracks}).Marshal(&ib); err != nil {
		return nil, fmt.Errorf("building the MP4 header: %w", err)
	}
	init, err := mp4meta.AppendToMoov(ib.Bytes(), mp4meta.Udta(cameraID, start, gap))
	if err != nil {
		return nil, err
	}

	s := &segment{
		partPath:  filepath.Join(dir, naming.PartName(name)),
		finalPath: filepath.Join(dir, name),
		cameraID:  cameraID,
		start:     start,
		gap:       gap,
		hourEnd:   hour.Add(naming.SegmentDuration),
		vp:        vp,
		ap:        ap,
		hash:      sha256.New(),
		now:       time.Now,
	}
	f, err := os.OpenFile(s.partPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	s.lastSync = s.now()
	if err := s.write(init); err != nil {
		f.Close()
		os.Remove(s.partPath)
		return nil, err
	}
	return s, nil
}

func (s *segment) write(b []byte) error {
	n, err := s.f.Write(b)
	s.hash.Write(b[:n])
	s.bytes += int64(n)
	return err
}

func (s *segment) name() string { return filepath.Base(s.partPath) }

func (s *segment) audioName() string {
	if s.ap == nil {
		return ""
	}
	return s.ap.name
}

// writeVideo appends one video access unit. The first one must be a keyframe.
func (s *segment) writeVideo(u sample) error {
	if s.vCount == 0 {
		s.vOrigin = u.dts
	}
	rel := u.dts - s.vOrigin
	if s.last != nil {
		d := rel - s.lastRel
		if d <= 0 || d > 10*mp4meta.VideoTimescale {
			// Timestamps went backwards or jumped: keep the file's timeline
			// continuous and re-anchor the origin.
			d = s.lastDur
			if d <= 0 {
				d = defaultFrameDur
			}
			rel = s.lastRel + d
			s.vOrigin = u.dts - rel
		}
		s.last.Duration = uint32(d)
		s.lastDur = d
	}
	if len(s.vPending) > 0 && (u.key || rel-s.fragStart >= maxFragmentTicks) {
		if err := s.flush(); err != nil {
			return err
		}
	}
	if len(s.vPending) == 0 {
		s.fragStart, s.fragKey = rel, u.key
	}
	smp := &fmp4.Sample{PTSOffset: int32(u.pts - u.dts), IsNonSyncSample: !u.key, Payload: u.data}
	s.vPending = append(s.vPending, smp)
	s.last, s.lastRel = smp, rel
	s.vCount++
	return nil
}

// writeAudio appends one audio access unit. Audio problems never reach the
// caller: they turn audio off for the rest of the file (spec 6.3).
func (s *segment) writeAudio(u sample) {
	if s.ap == nil || s.audioOff || s.vCount == 0 {
		return
	}
	t := u.pts - s.vOrigin*int64(s.ap.timescale)/mp4meta.VideoTimescale
	if t < 0 {
		return // before the first video frame of this file
	}
	if s.aLast != nil {
		d := t - s.aLastT
		if d <= 0 {
			return // duplicate or reordered packet
		}
		if d > int64(s.ap.timescale)*3600 {
			s.audioOff = true
			return
		}
		s.aLast.Duration = uint32(d)
	} else {
		if t < s.aEnd {
			t = s.aEnd
		}
		s.aFragBase = t
	}
	smp := &fmp4.Sample{Duration: u.dur, Payload: u.data}
	s.aPending = append(s.aPending, smp)
	s.aLast, s.aLastT, s.aLastNom = smp, t, u.dur
}

// flush writes the pending samples as one fragment.
func (s *segment) flush() error {
	if len(s.vPending) == 0 && len(s.aPending) == 0 {
		return nil
	}
	s.seq++
	part := fmp4.Part{SequenceNumber: s.seq}
	if len(s.vPending) > 0 {
		part.Tracks = append(part.Tracks, &fmp4.PartTrack{
			ID: mp4meta.VideoTrackID, BaseTime: uint64(s.fragStart), Samples: s.vPending,
		})
	}
	if len(s.aPending) > 0 {
		part.Tracks = append(part.Tracks, &fmp4.PartTrack{
			ID: mp4meta.AudioTrackID, BaseTime: uint64(s.aFragBase), Samples: s.aPending,
		})
		s.aEnd = s.aLastT + int64(s.aLastNom)
	}
	s.buf.Reset()
	if err := part.Marshal(&s.buf); err != nil {
		return fmt.Errorf("building fragment: %w", err)
	}
	off := s.bytes
	if err := s.write(s.buf.Bytes()); err != nil {
		return err
	}
	if len(s.vPending) > 0 && s.fragKey {
		s.frags = append(s.frags, mp4meta.Fragment{Time: uint64(s.fragStart), MoofOffset: uint64(off)})
	}
	clear(s.vPending)
	clear(s.aPending)
	s.vPending, s.aPending = s.vPending[:0], s.aPending[:0]
	s.aLast = nil

	if now := s.now(); now.Sub(s.lastSync) >= SyncInterval {
		s.lastSync = now
		if err := s.f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// duration returns the length of the video written so far, in 90 kHz ticks.
func (s *segment) duration() int64 {
	if s.vCount == 0 {
		return 0
	}
	d := s.lastDur
	if d <= 0 {
		d = defaultFrameDur
	}
	return s.lastRel + d
}

// close writes everything still buffered, appends the close marker and the
// random access index, flushes, and renames the file to its final name.
func (s *segment) close(reason string) (closedSegment, error) {
	if s.f == nil {
		return closedSegment{}, fmt.Errorf("segment already closed")
	}
	fail := func(err error) (closedSegment, error) {
		s.abort()
		return closedSegment{}, err
	}
	if s.last != nil {
		d := s.lastDur
		if d <= 0 {
			d = defaultFrameDur
		}
		s.last.Duration = uint32(d)
		s.lastDur = d
	}
	if err := s.flush(); err != nil {
		return fail(err)
	}
	dur := s.duration()
	if err := s.write(mp4meta.Trailer(reason, dur, s.frags)); err != nil {
		return fail(err)
	}
	if err := s.f.Sync(); err != nil {
		return fail(err)
	}
	if err := s.f.Close(); err != nil {
		s.f = nil
		return closedSegment{}, err
	}
	s.f = nil
	if err := os.Rename(s.partPath, s.finalPath); err != nil {
		return closedSegment{}, err
	}
	return closedSegment{
		path:       s.finalPath,
		start:      s.start,
		end:        mp4meta.EndOf(s.start, dur),
		bytes:      s.bytes,
		sha256:     hex.EncodeToString(s.hash.Sum(nil)),
		gap:        s.gap,
		videoCodec: s.vp.codec,
		audioCodec: s.audioName(),
	}, nil
}

// abort closes the file handle without finishing the file. The .part.mp4
// stays on disk and is repaired like a crash-cut file.
func (s *segment) abort() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}
