package media

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/stream"
	"github.com/jxburros/buttercams/internal/version"
)

// Connection timing. The values are the recorder's (spec 6.1); a sub stream
// behaves towards its camera exactly like a recording session does.
const (
	// ConnectTimeout bounds RTSP DESCRIBE/SETUP/PLAY.
	ConnectTimeout = 10 * time.Second
	// StallTimeout is how long a stream may send no video before it is
	// given up and connected again.
	StallTimeout = 10 * time.Second
	// MainRetry is how often the main stream is tried again while a viewer
	// waits for it (the worker's loopback restream answers at once).
	MainRetry = time.Second
	// stableAfter is how long a stream must play before its backoff resets.
	stableAfter = time.Minute
	// packetQueue is the capacity of a connection's packet channel: several
	// seconds of a high-bitrate stream.
	packetQueue = 4096
)

var backoffSteps = []time.Duration{
	2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second,
}

// backoff produces the reconnect delays 2, 4, 8, 16, 32, 60, 60 … seconds
// with ±20% jitter, like the recorder.
type backoff struct{ step int }

func (b *backoff) next() time.Duration {
	d := backoffSteps[len(backoffSteps)-1]
	if b.step < len(backoffSteps) {
		d = backoffSteps[b.step]
	}
	b.step++
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (b *backoff) reset() { b.step = 0 }

// target is where a source gets its stream from, as decided by the hub from
// the guardian's last assignment.
type target struct {
	ok     bool
	reason string // why there is nothing to connect to

	url   string // rtsp:// URL without credentials
	user  string
	pass  string
	udp   bool
	audio bool // set the audio track up too (main stream)
	main  bool
}

// processStart anchors monotonic time.
var processStart = time.Now()

func monoNow() int64 { return int64(time.Since(processStart)) }

// conn is one RTSP connection in PLAY state: to a camera's sub stream, or to
// a worker's loopback restream.
type conn struct {
	id     uint64
	client *gortsplib.Client

	videoFormat format.Format
	audioFormat format.Format // nil without usable audio

	pkts      chan *stream.Packet
	dropped   atomic.Uint64
	lastVideo atomic.Int64 // monotonic time of the last video packet
	since     time.Time

	done chan struct{}
	err  error

	// Owned by the source's goroutine.
	proc        *stream.Processor
	aproc       *stream.Processor // created only while a viewer listens
	audioOff    bool              // the audio cannot be used
	lastDropped uint64
}

func (c *conn) close() { c.client.Close() }

func (c *conn) silence() time.Duration { return time.Duration(monoNow() - c.lastVideo.Load()) }

// dialError is a failed connection attempt with a message for the viewer.
type dialError struct {
	msg string
	// config is true for problems that retrying soon cannot fix (wrong
	// password, wrong path, unsupported codec).
	config bool
}

func (e *dialError) Error() string { return e.msg }

func classifyDial(err error, main bool) *dialError {
	var de *dialError
	if errors.As(err, &de) {
		return de
	}
	var bad liberrors.ErrClientBadStatusCode
	if errors.As(err, &bad) {
		switch bad.Code {
		case base.StatusUnauthorized:
			if main {
				return &dialError{msg: "The recorder refused the connection."}
			}
			return &dialError{msg: "Authentication failed: check the camera user name and password.", config: true}
		case base.StatusNotFound:
			if main {
				return &dialError{msg: "The camera is not connected right now."}
			}
			return &dialError{msg: "Sub stream not found: check the sub stream path.", config: true}
		case base.StatusNotEnoughBandwidth, base.StatusServiceUnavailable:
			if main {
				return &dialError{msg: "The main stream is busy."}
			}
			return &dialError{msg: "The camera refused another connection for its sub stream."}
		}
		return &dialError{msg: fmt.Sprintf("The camera answered RTSP %d %s.", bad.Code, bad.Message)}
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "connection refused") || strings.Contains(s, "actively refused") {
		if main {
			return &dialError{msg: "The recorder is restarting."}
		}
		return &dialError{msg: "Cannot connect to the camera: connection refused."}
	}
	if main {
		return &dialError{msg: "The main stream is not available: " + config.Redact(err.Error())}
	}
	return &dialError{msg: "Cannot connect to the camera: " + config.Redact(err.Error())}
}

// usableAudio reports whether the live muxer can carry an audio format:
// AAC and Opus as they are, G.711 and G.726 through the recorder's converter.
func usableAudio(f format.Format) bool {
	switch a := f.(type) {
	case *format.MPEG4Audio:
		return a.Config != nil
	case *format.MPEG4AudioLATM:
		return !a.CPresent && a.StreamMuxConfig != nil && len(a.StreamMuxConfig.Programs) == 1 &&
			len(a.StreamMuxConfig.Programs[0].Layers) == 1
	case *format.Opus, *format.G711, *format.G726:
		return true
	}
	return false
}

// dial performs DESCRIBE, SETUP and PLAY within ConnectTimeout.
func dial(ctx context.Context, tgt target, id uint64) (*conn, error) {
	u, err := base.ParseURL(tgt.url)
	if err != nil {
		return nil, &dialError{msg: "The stream address is not valid.", config: true}
	}
	if tgt.user != "" {
		u.User = url.UserPassword(tgt.user, tgt.pass)
	}
	proto := gortsplib.ProtocolTCP
	if tgt.udp {
		proto = gortsplib.ProtocolUDP
	}
	c := &conn{id: id, pkts: make(chan *stream.Packet, packetQueue), done: make(chan struct{}), since: time.Now()}
	client := &gortsplib.Client{
		Scheme:        u.Scheme,
		Host:          u.Host,
		Protocol:      &proto,
		ReadTimeout:   ConnectTimeout,
		WriteTimeout:  ConnectTimeout,
		UserAgent:     "ButterCams/" + version.Version + " (live view)",
		OnPacketsLost: func(uint64) {},
		OnDecodeError: func(error) {},
	}
	c.client = client

	// Start only prepares the client (no network I/O). It runs before any
	// path below can call Close: Close on a client that is not started yet
	// panics or blocks forever.
	if err := client.Start(); err != nil {
		return nil, classifyDial(err, tgt.main)
	}
	res := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				res <- fmt.Errorf("connecting panicked: %v", r)
			}
		}()
		res <- c.connect(client, u, tgt)
	}()
	timer := time.NewTimer(ConnectTimeout)
	defer timer.Stop()
	select {
	case err := <-res:
		if err != nil {
			client.Close()
			return nil, classifyDial(err, tgt.main)
		}
	case <-timer.C:
		client.Close()
		<-res
		return nil, &dialError{msg: "The connection timed out."}
	case <-ctx.Done():
		client.Close()
		<-res
		return nil, &dialError{msg: "cancelled"}
	}
	go func() {
		c.err = client.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *conn) connect(client *gortsplib.Client, u *base.URL, tgt target) error {
	// The client was started by the caller.
	desc, _, err := client.Describe(u)
	if err != nil {
		return err
	}
	var videoMedia, audioMedia *description.Media
	unsupported := ""
	for _, m := range desc.Medias {
		if m.IsBackChannel {
			continue
		}
		for _, f := range m.Formats {
			switch m.Type {
			case description.MediaTypeVideo:
				if videoMedia != nil {
					continue
				}
				switch f.(type) {
				case *format.H264, *format.H265:
					videoMedia, c.videoFormat = m, f
				default:
					if unsupported == "" {
						unsupported = f.Codec()
					}
				}
			case description.MediaTypeAudio:
				if tgt.audio && audioMedia == nil && usableAudio(f) {
					audioMedia, c.audioFormat = m, f
				}
			}
		}
	}
	if videoMedia == nil {
		if unsupported != "" {
			return &dialError{msg: fmt.Sprintf("The stream is %s; live view shows H.264 and H.265 only.", unsupported), config: true}
		}
		return &dialError{msg: "The stream has no video track.", config: true}
	}
	if _, err := client.Setup(desc.BaseURL, videoMedia, 0, 0); err != nil {
		return err
	}
	if audioMedia != nil {
		if _, err := client.Setup(desc.BaseURL, audioMedia, 0, 0); err != nil {
			audioMedia, c.audioFormat = nil, nil // audio never prevents video
		}
	}
	deliver := func(track stream.Track, media *description.Media) func(*rtp.Packet) {
		return func(pkt *rtp.Packet) {
			if track == stream.TrackVideo {
				c.lastVideo.Store(monoNow())
			}
			pts, ok := client.PacketPTS(media, pkt)
			p := &stream.Packet{Session: c.id, Track: track, RTP: pkt, PTS: pts, PTSValid: ok, Recv: time.Now()}
			select {
			case c.pkts <- p:
			default:
				c.dropped.Add(1) // the source's goroutine is behind; it resynchronizes
			}
		}
	}
	client.OnPacketRTP(videoMedia, c.videoFormat, deliver(stream.TrackVideo, videoMedia))
	if audioMedia != nil {
		client.OnPacketRTP(audioMedia, c.audioFormat, deliver(stream.TrackAudio, audioMedia))
	}
	c.lastVideo.Store(monoNow())
	if _, err := client.Play(nil); err != nil {
		return err
	}
	proc, err := stream.NewProcessor(c.videoFormat, nil)
	if err != nil {
		return &dialError{msg: err.Error(), config: true}
	}
	c.proc = proc
	return nil
}
