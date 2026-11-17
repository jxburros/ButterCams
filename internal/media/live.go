package media

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/stream"
)

// The live muxer (spec 16.4): one moof+mdat per video frame, so the server
// adds no delay. It uses the same fragment writer as the recorder.

const (
	// defaultFrameTicks is the duration given to a frame before the stream's
	// own frame interval is known: 1/15 s at 90 kHz.
	defaultFrameTicks = 6000
	// maxFrameGap is the longest step between two frames that is taken from
	// the camera's timestamps. Anything longer (a stall, a reconnect, another
	// session) is replaced by one frame interval, so the viewer's timeline
	// has no holes and the browser never waits at one.
	maxFrameGap = 2 * mp4meta.VideoTimescale
	// maxAudioBatch bounds the audio samples sent in one fragment.
	maxAudioBatch = 64
)

// initSeg is one initialization segment with the MIME type that describes
// it. Viewers remember which one they were sent last by its pointer.
type initSeg struct {
	mime   string
	width  int
	height int
	data   []byte
}

// initMessage is the text message that precedes an initialization segment.
func (i *initSeg) message() []byte {
	return []byte(fmt.Sprintf(`{"type":"init","mime":%q,"width":%d,"height":%d}`, i.mime, i.width, i.height))
}

// stateMessage is the text message sent while there is no video.
func stateMessage(msg string) []byte {
	return []byte(fmt.Sprintf(`{"type":"state","state":"waiting","message":%q}`, msg))
}

// videoCodecString returns the RFC 6381 codec string of the parameter sets:
// "avc1.PPCCLL" for H.264 and "hvc1.…" for H.265.
func videoCodecString(vp stream.VideoParams) string {
	if vp.Codec == "h265" {
		return hevcCodecString(vp.SPS)
	}
	if len(vp.SPS) < 4 {
		return "avc1.42E01E"
	}
	return fmt.Sprintf("avc1.%02X%02X%02X", vp.SPS[1], vp.SPS[2], vp.SPS[3])
}

// hevcCodecString builds the codec string from the profile, tier and level
// in the SPS (ISO/IEC 14496-15, annex E), for example "hvc1.1.6.L93.B0".
func hevcCodecString(sps []byte) string {
	if s := mp4meta.HEVCCodecString("hvc1", sps); s != "" {
		return s
	}
	return "hvc1.1.6.L93.B0"
}

// audioCodecString returns the codec string of an audio track.
func audioCodecString(ap *stream.AudioParams) string {
	if ap.Name == "opus" {
		return "opus"
	}
	if c, ok := ap.Codec.(*codecs.MPEG4Audio); ok && c.Config.Type > 0 {
		return fmt.Sprintf("mp4a.40.%d", int(c.Config.Type))
	}
	return "mp4a.40.2"
}

// buildInit makes the initialization segment for a video track and an
// optional audio track.
func buildInit(vp stream.VideoParams, ap *stream.AudioParams, width, height int) (*initSeg, error) {
	data, err := stream.InitSegment(vp, ap)
	if err != nil {
		return nil, err
	}
	list := videoCodecString(vp)
	if ap != nil {
		list += "," + audioCodecString(ap)
	}
	return &initSeg{mime: `video/mp4; codecs="` + list + `"`, width: width, height: height, data: data}, nil
}

// sameInit reports whether two initialization segments are interchangeable.
func sameInit(a, b *initSeg) bool {
	return a != nil && b != nil && a.mime == b.mime && a.width == b.width && a.height == b.height && bytes.Equal(a.data, b.data)
}

// muxer turns samples into fragments on one continuous timeline. The
// timeline carries on across reconnects and across the switch to another
// RTSP session, so the browser sees one unbroken stream.
type muxer struct {
	seq uint32

	// video, 90 kHz
	started bool
	sess    uint64 // session of the last frame
	lastDTS int64  // camera time of the last frame
	lastDur int64  // the stream's frame interval, as last measured
	out     int64  // output time of the last frame

	// audio, in the audio timescale
	timescale uint32
	aMeta     []mp4meta.FragSample
	aData     []byte
	aBase     int64 // output time of the first pending sample
	aNext     int64 // output time after the last pending sample
	aEnd      int64 // output time after the last sample sent
}

// video builds the fragment for one access unit and returns it with the
// duration it was given.
func (m *muxer) video(u stream.Sample, sess uint64) []byte {
	dur := m.lastDur
	if dur <= 0 {
		dur = defaultFrameTicks
	}
	switch {
	case !m.started:
		m.started = true
	case sess != m.sess:
		m.out += dur
	default:
		d := u.DTS - m.lastDTS
		if d <= 0 || d > maxFrameGap {
			d = dur
		} else {
			m.lastDur, dur = d, d
		}
		m.out += d
	}
	m.sess, m.lastDTS = sess, u.DTS

	size := 0
	for _, n := range u.NALUs {
		size += 4 + len(n)
	}
	m.seq++
	sample := [1]mp4meta.FragSample{{Duration: uint32(dur), Size: uint32(size), PTSOffset: int32(u.PTS - u.DTS), NonSync: !u.Key}}
	track := [1]mp4meta.FragTrack{{ID: mp4meta.VideoTrackID, BaseTime: uint64(m.out), Samples: sample[:]}}
	buf := make([]byte, 0, 160+size)
	buf = mp4meta.AppendFragmentHeader(buf, m.seq, track[:])
	for _, n := range u.NALUs {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(n)))
		buf = append(buf, n...)
	}
	return buf
}

// audio queues one audio access unit. It returns true when the batch should
// be flushed without waiting for the next video frame.
func (m *muxer) audio(u stream.Sample, timescale uint32) bool {
	if !m.started || timescale == 0 {
		return false // nothing before the first picture
	}
	if timescale != m.timescale {
		m.timescale, m.aEnd = timescale, 0
		m.aMeta, m.aData = m.aMeta[:0], m.aData[:0]
	}
	if len(m.aMeta) == 0 {
		// Place the sample on the video's timeline: the camera's audio and
		// video clocks share an origin, and the last frame fixes where that
		// origin is on the output timeline.
		t := u.PTS + (m.out-m.lastDTS)*int64(timescale)/mp4meta.VideoTimescale
		if t < m.aEnd {
			t = m.aEnd
		}
		m.aBase, m.aNext = t, t
	}
	m.aData = append(m.aData, u.Data...)
	m.aMeta = append(m.aMeta, mp4meta.FragSample{Duration: u.Dur, Size: uint32(len(u.Data))})
	m.aNext += int64(u.Dur)
	return len(m.aMeta) >= maxAudioBatch
}

// flushAudio returns the pending audio samples as one fragment, or nil.
func (m *muxer) flushAudio() []byte {
	if len(m.aMeta) == 0 {
		return nil
	}
	m.seq++
	track := [1]mp4meta.FragTrack{{ID: mp4meta.AudioTrackID, BaseTime: uint64(m.aBase), Samples: m.aMeta}}
	buf := make([]byte, 0, 96+16*len(m.aMeta)+len(m.aData))
	buf = mp4meta.AppendFragmentHeader(buf, m.seq, track[:])
	buf = append(buf, m.aData...)
	m.aEnd = m.aNext
	m.aMeta, m.aData = m.aMeta[:0], m.aData[:0]
	return buf
}

// dropAudio forgets pending audio (the audio track went away).
func (m *muxer) dropAudio() {
	m.aMeta, m.aData = m.aMeta[:0], m.aData[:0]
}
