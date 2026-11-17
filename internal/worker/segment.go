package worker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"

	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/stream"
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
	frags []mp4meta.Fragment

	// The fragment being assembled. The buffers are reused for every
	// fragment, so steady-state recording allocates nothing here.
	hdr   []byte               // moof + mdat header
	vData []byte               // video samples, each NAL unit length-prefixed
	aData []byte               // audio samples
	vMeta []mp4meta.FragSample // one entry per pending video sample
	aMeta []mp4meta.FragSample

	// video timeline, 90 kHz, relative to the first sample of the file
	vOrigin   int64 // session DTS of file time zero
	vCount    int
	lastRel   int64
	lastDur   int64
	fragStart int64
	fragKey   bool

	// audio timeline, audio timescale, relative to the same instant
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

	tracks := []*fmp4.InitTrack{{ID: mp4meta.VideoTrackID, TimeScale: mp4meta.VideoTimescale, Codec: vp.MP4Codec()}}
	if ap != nil {
		tracks = append(tracks, &fmp4.InitTrack{ID: mp4meta.AudioTrackID, TimeScale: ap.Timescale, Codec: ap.Codec})
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
	return s.ap.Name
}

// writeVideo appends one video access unit. The first one must be a keyframe.
func (s *segment) writeVideo(u sample) error {
	if s.vCount == 0 {
		s.vOrigin = u.DTS
	}
	rel := u.DTS - s.vOrigin
	if n := len(s.vMeta); s.vCount > 0 {
		d := rel - s.lastRel
		if d <= 0 || d > 10*mp4meta.VideoTimescale {
			// Timestamps went backwards or jumped: keep the file's timeline
			// continuous and re-anchor the origin.
			d = s.lastDur
			if d <= 0 {
				d = defaultFrameDur
			}
			rel = s.lastRel + d
			s.vOrigin = u.DTS - rel
		}
		if n > 0 {
			s.vMeta[n-1].Duration = uint32(d)
		}
		s.lastDur = d
	}
	if len(s.vMeta) > 0 && (u.Key || rel-s.fragStart >= maxFragmentTicks) {
		if err := s.flush(); err != nil {
			return err
		}
	}
	if len(s.vMeta) == 0 {
		s.fragStart, s.fragKey = rel, u.Key
	}
	// The NAL units go into the file exactly as received, each behind its
	// four-byte length.
	before := len(s.vData)
	for _, nalu := range u.NALUs {
		s.vData = binary.BigEndian.AppendUint32(s.vData, uint32(len(nalu)))
		s.vData = append(s.vData, nalu...)
	}
	s.vMeta = append(s.vMeta, mp4meta.FragSample{
		Size: uint32(len(s.vData) - before), PTSOffset: int32(u.PTS - u.DTS), NonSync: !u.Key,
	})
	s.lastRel = rel
	s.vCount++
	return nil
}

// writeAudio appends one audio access unit. Audio problems never reach the
// caller: they turn audio off for the rest of the file (spec 6.3).
func (s *segment) writeAudio(u sample) {
	if s.ap == nil || s.audioOff || s.vCount == 0 {
		return
	}
	t := u.PTS - s.vOrigin*int64(s.ap.Timescale)/mp4meta.VideoTimescale
	if t < 0 {
		return // before the first video frame of this file
	}
	if n := len(s.aMeta); n > 0 {
		d := t - s.aLastT
		if d <= 0 {
			return // duplicate or reordered packet
		}
		if d > int64(s.ap.Timescale)*3600 {
			s.audioOff = true
			return
		}
		s.aMeta[n-1].Duration = uint32(d)
	} else {
		if t < s.aEnd {
			t = s.aEnd
		}
		s.aFragBase = t
	}
	s.aData = append(s.aData, u.Data...)
	s.aMeta = append(s.aMeta, mp4meta.FragSample{Duration: u.Dur, Size: uint32(len(u.Data))})
	s.aLastT, s.aLastNom = t, u.Dur
}

// flush writes the pending samples as one fragment: a moof, then an mdat
// holding the video samples followed by the audio samples.
func (s *segment) flush() error {
	if len(s.vMeta) == 0 && len(s.aMeta) == 0 {
		return nil
	}
	s.seq++
	var tracks [2]mp4meta.FragTrack
	n := 0
	if len(s.vMeta) > 0 {
		tracks[n] = mp4meta.FragTrack{ID: mp4meta.VideoTrackID, BaseTime: uint64(s.fragStart), Samples: s.vMeta}
		n++
	}
	if len(s.aMeta) > 0 {
		tracks[n] = mp4meta.FragTrack{ID: mp4meta.AudioTrackID, BaseTime: uint64(s.aFragBase), Samples: s.aMeta}
		n++
		s.aEnd = s.aLastT + int64(s.aLastNom)
	}
	s.hdr = mp4meta.AppendFragmentHeader(s.hdr[:0], s.seq, tracks[:n])
	off := s.bytes
	if err := s.write(s.hdr); err != nil {
		return err
	}
	if len(s.vData) > 0 {
		if err := s.write(s.vData); err != nil {
			return err
		}
	}
	if len(s.aData) > 0 {
		if err := s.write(s.aData); err != nil {
			return err
		}
	}
	if len(s.vMeta) > 0 && s.fragKey {
		s.frags = append(s.frags, mp4meta.Fragment{Time: uint64(s.fragStart), MoofOffset: uint64(off)})
	}
	s.vMeta, s.aMeta = s.vMeta[:0], s.aMeta[:0]
	s.vData, s.aData = s.vData[:0], s.aData[:0]

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
	if n := len(s.vMeta); n > 0 {
		d := s.lastDur
		if d <= 0 {
			d = defaultFrameDur
		}
		s.vMeta[n-1].Duration = uint32(d)
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
		videoCodec: s.vp.Codec,
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

// Shorter names for the stream types the recorder uses everywhere.
type (
	videoParams = stream.VideoParams
	audioParams = stream.AudioParams
	sample      = stream.Sample
)
