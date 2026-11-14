package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/audio"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/version"
)

// dialKind classifies why a connection attempt failed.
type dialKind int

const (
	dialOther       dialKind = iota // unreachable, timeout, protocol error: back off and retry
	dialAuth                        // RTSP 401: wrong credentials
	dialUnsupported                 // the camera sends a video codec ButterCams does not record
	dialRefused                     // connection refused or RTSP 453/503: no further session allowed
)

type dialError struct {
	kind dialKind
	msg  string
}

func (e *dialError) Error() string { return e.msg }

func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection refused") || strings.Contains(s, "actively refused")
}

func classifyDialError(err error) *dialError {
	var de *dialError
	if errors.As(err, &de) {
		return de
	}
	var bad liberrors.ErrClientBadStatusCode
	if errors.As(err, &bad) {
		switch bad.Code {
		case base.StatusUnauthorized:
			return &dialError{dialAuth, "Authentication failed"}
		case base.StatusNotEnoughBandwidth, base.StatusServiceUnavailable:
			return &dialError{dialRefused, fmt.Sprintf("camera refused the session (RTSP %d %s)", bad.Code, bad.Message)}
		case base.StatusNotFound:
			return &dialError{dialOther, "stream not found (RTSP 404): check the stream path"}
		}
		return &dialError{dialOther, fmt.Sprintf("camera answered RTSP %d %s", bad.Code, bad.Message)}
	}
	if isConnRefused(err) {
		return &dialError{dialRefused, "connection refused"}
	}
	return &dialError{dialOther, config.Redact(err.Error())}
}

// audioTrack describes the audio a session will deliver.
type audioTrack struct {
	media  *description.Media
	format format.Format
	source string // codec the camera sends: aac, opus, pcm_mulaw, pcm_alaw, g726
}

// session is one RTSP connection to a camera in PLAY state.
type session struct {
	id     uint64
	client *gortsplib.Client

	videoCodec  string // h264 or h265
	videoMedia  *description.Media
	videoFormat format.Format
	audio       *audioTrack
	audioNote   string // why audio is not recorded, if it is not

	playedAt      time.Time
	lastVideo     atomic.Int64 // wall clock (UnixNano) of the last video packet
	lastVideoMono atomic.Int64 // monotonic time of the last video packet
	bytes         atomic.Uint64
	lost          atomic.Uint64

	done chan struct{}
	err  error
}

// processStart anchors monotonic time: durations since it are immune to
// wall-clock changes (spec 6.6).
var processStart = time.Now()

// videoSilence returns how long ago the last video packet arrived, measured
// on the monotonic clock.
func (s *session) videoSilence() time.Duration {
	return time.Since(processStart) - time.Duration(s.lastVideoMono.Load())
}

func (s *session) close() {
	s.client.Close()
}

// lastVideoTime returns when the last video packet arrived (the PLAY time
// before any packet).
func (s *session) lastVideoTime() time.Time {
	return time.Unix(0, s.lastVideo.Load())
}

func videoCodecName(f format.Format) string {
	switch f.(type) {
	case *format.MJPEG:
		return "MJPEG"
	case *format.MPEG4Video:
		return "MPEG-4 Part 2"
	}
	return f.Codec()
}

// describeAudio decides what to do with an audio format.
func describeAudio(f format.Format) (source string, supported bool, label string) {
	switch a := f.(type) {
	case *format.MPEG4Audio:
		if a.Config == nil {
			return "aac", false, "AAC without a configuration"
		}
		return "aac", true, "AAC"
	case *format.MPEG4AudioLATM:
		if a.CPresent || a.StreamMuxConfig == nil || len(a.StreamMuxConfig.Programs) != 1 ||
			len(a.StreamMuxConfig.Programs[0].Layers) != 1 {
			return "aac", false, "AAC (LATM, multi-stream)"
		}
		return "aac", true, "AAC"
	case *format.Opus:
		return "opus", true, "Opus"
	case *format.G711:
		name, label := "pcm_alaw", "G.711 A-law"
		if a.MULaw {
			name, label = "pcm_mulaw", "G.711 µ-law"
		}
		if a.ChannelCount > 1 || !audio.SupportedRate(a.SampleRate) {
			return name, false, fmt.Sprintf("%s at %d Hz, %d channels", label, a.SampleRate, a.ChannelCount)
		}
		return name, true, label
	case *format.G726:
		if _, err := audio.NewG726Decoder(a.BitRate*1000, a.BigEndian); err != nil {
			if _, err2 := audio.NewG726Decoder(a.BitRate, a.BigEndian); err2 != nil {
				return "g726", false, fmt.Sprintf("G.726 at %d bit/s", a.BitRate)
			}
		}
		return "g726", true, "G.726"
	}
	return strings.ToLower(f.Codec()), false, f.Codec()
}

// dialOptions control a connection attempt.
type dialOptions struct {
	id       uint64
	cam      config.Camera
	password string
	publish  func(*Packet)    // nil for a probe that only inspects the stream
	onPacket func(*Packet)    // optional tap used by the probe
	now      func() time.Time // wall clock; nil means time.Now
}

// dialSession performs DESCRIBE, SETUP and PLAY within ConnectTimeout.
func dialSession(ctx context.Context, opt dialOptions) (*session, error) {
	u, err := base.ParseURL(opt.cam.URL())
	if err != nil {
		return nil, &dialError{dialOther, "invalid stream address: " + err.Error()}
	}
	if opt.cam.Username != "" {
		u.User = url.UserPassword(opt.cam.Username, opt.password)
	}

	proto := gortsplib.ProtocolTCP
	if opt.cam.Transport == "udp" {
		proto = gortsplib.ProtocolUDP
	}
	s := &session{id: opt.id, done: make(chan struct{})}
	c := &gortsplib.Client{
		Scheme:       u.Scheme,
		Host:         u.Host,
		Protocol:     &proto,
		ReadTimeout:  ConnectTimeout,
		WriteTimeout: ConnectTimeout,
		UserAgent:    "ButterCams/" + version.Version,
		OnPacketsLost: func(n uint64) {
			s.lost.Add(n)
		},
		OnDecodeError: func(error) {},
	}
	s.client = c

	type result struct{ err error }
	resCh := make(chan result, 1)
	go func() {
		resCh <- result{s.connect(c, u, opt)}
	}()

	timer := time.NewTimer(ConnectTimeout)
	defer timer.Stop()
	select {
	case r := <-resCh:
		if r.err != nil {
			c.Close()
			return nil, classifyDialError(r.err)
		}
	case <-timer.C:
		c.Close()
		<-resCh
		return nil, &dialError{dialOther, "connection timed out"}
	case <-ctx.Done():
		c.Close()
		<-resCh
		return nil, &dialError{dialOther, "cancelled"}
	}

	go func() {
		s.err = c.Wait()
		close(s.done)
	}()
	return s, nil
}

func (s *session) connect(c *gortsplib.Client, u *base.URL, opt dialOptions) error {
	if err := c.Start(); err != nil {
		return err
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		return err
	}

	var unsupportedVideo string
	for _, m := range desc.Medias {
		if m.IsBackChannel {
			continue
		}
		for _, f := range m.Formats {
			switch m.Type {
			case description.MediaTypeVideo:
				if s.videoMedia != nil {
					continue
				}
				switch f.(type) {
				case *format.H264:
					s.videoCodec, s.videoMedia, s.videoFormat = "h264", m, f
				case *format.H265:
					s.videoCodec, s.videoMedia, s.videoFormat = "h265", m, f
				default:
					if unsupportedVideo == "" {
						unsupportedVideo = videoCodecName(f)
					}
				}
			case description.MediaTypeAudio:
				if s.audio != nil || s.audioNote != "" || !opt.cam.RecordAudio {
					continue
				}
				source, ok, label := describeAudio(f)
				if ok {
					s.audio = &audioTrack{media: m, format: f, source: source}
				} else {
					s.audioNote = "audio not recorded: " + label
				}
			}
		}
	}
	if s.videoMedia == nil {
		if unsupportedVideo != "" {
			return &dialError{dialUnsupported, fmt.Sprintf(
				"Camera sends %s; ButterCams v1 records H.264 or H.265 only. Change the camera's encoding setting.",
				unsupportedVideo)}
		}
		return &dialError{dialUnsupported, "The stream has no video track."}
	}
	if s.audio != nil {
		s.audioNote = "" // a usable audio track was found after an unusable one
	}

	if _, err := c.Setup(desc.BaseURL, s.videoMedia, 0, 0); err != nil {
		return err
	}
	if s.audio != nil {
		if _, err := c.Setup(desc.BaseURL, s.audio.media, 0, 0); err != nil {
			// Audio must never prevent video recording.
			s.audioNote = "audio not recorded: the camera refused the audio track"
			s.audio = nil
		}
	}

	clock := opt.now
	if clock == nil {
		clock = time.Now
	}
	deliver := func(track Track, media *description.Media) func(*rtp.Packet) {
		return func(pkt *rtp.Packet) {
			now := clock()
			if track == TrackVideo {
				s.lastVideo.Store(now.UnixNano())
				s.lastVideoMono.Store(int64(time.Since(processStart)))
			}
			s.bytes.Add(uint64(len(pkt.Payload)))
			pts, ok := c.PacketPTS(media, pkt)
			p := &Packet{Session: s.id, Track: track, RTP: pkt, PTS: pts, PTSValid: ok, Recv: now}
			if opt.publish != nil {
				opt.publish(p)
			}
			if opt.onPacket != nil {
				opt.onPacket(p)
			}
		}
	}
	c.OnPacketRTP(s.videoMedia, s.videoFormat, deliver(TrackVideo, s.videoMedia))
	if s.audio != nil {
		c.OnPacketRTP(s.audio.media, s.audio.format, deliver(TrackAudio, s.audio.media))
	}

	s.playedAt = time.Now()
	s.lastVideo.Store(clock().UnixNano())
	s.lastVideoMono.Store(int64(time.Since(processStart)))
	if _, err := c.Play(nil); err != nil {
		return err
	}
	return nil
}
