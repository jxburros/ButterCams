// Package restream is the loopback restream of a recorder worker (spec 16.3):
// a small RTSP server on 127.0.0.1 that hands the packets a camera's main
// stream delivers to the recorder on to one client, the media process, so
// that live view never needs a second connection to the camera.
//
// It is the only live-view code inside the recorder process, and it is built
// so that it cannot disturb a recording (spec 16.9):
//
//   - It is idle unless watched: a fan-out reader is registered on the first
//     PLAY and removed when the client leaves.
//   - It serves one client per RTSP session of a camera.
//   - It never makes the recorder wait. Packets reach it over a bounded
//     channel that drops when full, and it hands them to the RTSP server's
//     write queue, which drops the client when it does not keep up.
//   - Fan-out packets are read-only: the header is copied before the server
//     stamps its own SSRC on it.
//   - Every goroutine recovers from panics. A failure closes the camera's
//     mounts and keeps its restream off until the camera's next session.
//
// Nothing here runs on a camera's goroutine. What the restream needs to know
// about a camera it asks through Source, whose methods never wait.
package restream

import (
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/stream"
)

const (
	// ReaderQueue is the capacity of the fan-out reader of a watched camera.
	ReaderQueue = 1024
	// writeQueue is the RTSP server's queue of outbound packets per client.
	writeQueue = 1024
	// sweepInterval is how often a watched camera's sessions are checked, so
	// that the mount of a session that ended is closed.
	sweepInterval = 250 * time.Millisecond
)

// Session describes one playing RTSP session of a camera.
type Session struct {
	ID    uint64
	Video format.Format
	Audio format.Format // nil when the session has no usable audio track
}

// Source is what the restream needs from the worker. Every method returns at
// once and none of them runs on, or waits for, a camera's goroutine.
type Source interface {
	// Sessions returns the playing sessions of a camera, oldest first (two
	// during the hourly rollover). ok is false for an unknown camera.
	Sessions(cameraID string) (sessions []Session, ok bool)
	// Subscribe registers a fan-out reader for the camera's packets. It
	// returns nil when the camera is gone.
	Subscribe(cameraID string, buffer int) *stream.Reader
	// Unsubscribe removes a reader returned by Subscribe.
	Unsubscribe(cameraID string, r *stream.Reader)
	// Failed reports that the camera's restream stopped after an internal
	// error.
	Failed(cameraID, reason string)
}

// Options configure a Server.
type Options struct {
	// Token is the password clients must present (user ipc.RestreamUser).
	Token string
	Log   *slog.Logger
	// PumpHook, when set, is called for every packet a pump takes from its
	// reader. Tests use it to stall a pump or make it panic.
	PumpHook func(cameraID string)
}

// Server is the loopback restream of one worker.
type Server struct {
	src  Source
	opt  Options
	log  *slog.Logger
	srv  *gortsplib.Server
	port int

	mu     sync.Mutex
	cams   map[string]*camera
	closed bool
	wg     sync.WaitGroup
}

type camera struct {
	id     string
	mounts map[uint64]*mount
	stop   chan struct{} // closes to end the running pump; nil without one

	// After a failure the restream of the camera stays off while failedAt is
	// still its newest session.
	failed   bool
	failedAt uint64
}

// mount serves one RTSP session of a camera to one client.
type mount struct {
	cam     *camera
	sess    Session
	st      *gortsplib.ServerStream
	video   *description.Media
	audio   *description.Media
	client  *gortsplib.ServerSession
	playing bool
	conns   int // connections that were given this mount by DESCRIBE
}

// Start listens on a free loopback port.
func Start(src Source, opt Options) (*Server, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	s := &Server{src: src, opt: opt, log: opt.Log.With("component", "restream"), cams: map[string]*camera{}}
	// The port is chosen first: the RTSP server wants a fixed address.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	s.srv = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    net.JoinHostPort("127.0.0.1", fmt.Sprint(s.port)),
		WriteQueueSize: writeQueue,
	}
	if err := s.srv.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// Port returns the loopback port the restream listens on.
func (s *Server) Port() int { return s.port }

// Close disconnects every client and stops every goroutine of the restream.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, cam := range s.cams {
		s.stopPumpLocked(cam)
		for id := range cam.mounts {
			delete(cam.mounts, id)
		}
	}
	s.mu.Unlock()
	s.srv.Close() // closes every connection, session and stream of the server
	s.wg.Wait()
}

func (s *Server) stopPumpLocked(cam *camera) {
	if cam.stop != nil {
		close(cam.stop)
		cam.stop = nil
	}
}

// closeStream ends a mount's stream, which disconnects its client. It runs
// on its own goroutine: the caller may be inside one of the stream's own
// callbacks.
func (s *Server) closeStream(m *mount) {
	go func() {
		defer func() { recover() }()
		m.st.Close()
	}()
}

// removeMountLocked forgets a mount and disconnects its client.
func (s *Server) removeMountLocked(m *mount) {
	if m.cam.mounts[m.sess.ID] != m {
		return
	}
	delete(m.cam.mounts, m.sess.ID)
	m.client, m.playing = nil, false
	s.closeStream(m)
	s.stopIdlePumpLocked(m.cam)
}

// stopIdlePumpLocked ends the camera's pump when no client is playing, which
// also removes its fan-out reader.
func (s *Server) stopIdlePumpLocked(cam *camera) {
	for _, m := range cam.mounts {
		if m.playing {
			return
		}
	}
	s.stopPumpLocked(cam)
}

// --------------------------------------------------------------- handlers

func status(code base.StatusCode) *base.Response { return &base.Response{StatusCode: code} }

// guard turns a panic in a handler into an RTSP 500.
func (s *Server) guard(what string, res **base.Response) {
	if r := recover(); r != nil {
		s.log.Error("restream handler panicked", "handler", what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		*res = status(base.StatusInternalServerError)
	}
}

func loopback(conn *gortsplib.ServerConn) bool {
	addr, ok := conn.NetConn().RemoteAddr().(*net.TCPAddr)
	return ok && addr.IP.IsLoopback()
}

// admit checks where a request comes from and its credentials.
func (s *Server) admit(conn *gortsplib.ServerConn, req *base.Request) (*base.Response, error) {
	if !loopback(conn) {
		return status(base.StatusForbidden), nil
	}
	if !conn.VerifyCredentials(req, ipc.RestreamUser, s.opt.Token) {
		return status(base.StatusUnauthorized), liberrors.ErrServerAuth{}
	}
	return nil, nil
}

// OnConnOpen implements gortsplib.ServerHandlerOnConnOpen.
func (s *Server) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	defer func() { recover() }()
	if !loopback(ctx.Conn) {
		ctx.Conn.Close()
	}
}

// OnConnClose implements gortsplib.ServerHandlerOnConnClose.
func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	defer func() { recover() }()
	m, _ := ctx.Conn.UserData().(*mount)
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m.conns--
	if m.conns <= 0 && m.client == nil {
		s.removeMountLocked(m) // described but never played
	}
}

// OnDescribe implements gortsplib.ServerHandlerOnDescribe: it describes the
// newest session of the camera named by the path.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (res *base.Response, st *gortsplib.ServerStream, err error) {
	defer s.guard("describe", &res)
	if res, err := s.admit(ctx.Conn, ctx.Request); res != nil {
		return res, nil, err
	}
	if prev, _ := ctx.Conn.UserData().(*mount); prev != nil {
		return status(base.StatusOK), prev.st, nil
	}
	id := strings.Trim(ctx.Path, "/")
	sessions, ok := s.src.Sessions(id)
	if !ok || len(sessions) == 0 {
		return status(base.StatusNotFound), nil, nil
	}
	newest := sessions[len(sessions)-1]

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return status(base.StatusServiceUnavailable), nil, nil
	}
	cam := s.cams[id]
	if cam == nil {
		cam = &camera{id: id, mounts: map[uint64]*mount{}}
		s.cams[id] = cam
	}
	if cam.failed {
		if cam.failedAt == newest.ID {
			return status(base.StatusNotFound), nil, nil
		}
		cam.failed = false
	}
	s.sweepLocked(cam, sessions)
	m := cam.mounts[newest.ID]
	if m == nil {
		m = &mount{cam: cam, sess: newest}
		m.video = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{newest.Video}}
		desc := &description.Session{Medias: []*description.Media{m.video}}
		if newest.Audio != nil {
			m.audio = &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{newest.Audio}}
			desc.Medias = append(desc.Medias, m.audio)
		}
		m.st = &gortsplib.ServerStream{Server: s.srv, Desc: desc}
		if err := m.st.Initialize(); err != nil {
			s.log.Warn("restream: cannot serve the session", "camera", id, "error", err.Error())
			return status(base.StatusInternalServerError), nil, nil
		}
		cam.mounts[newest.ID] = m
	} else if m.client != nil {
		return status(base.StatusServiceUnavailable), nil, nil // one client per session
	}
	m.conns++
	ctx.Conn.SetUserData(m)
	return status(base.StatusOK), m.st, nil
}

// OnSetup implements gortsplib.ServerHandlerOnSetup.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (res *base.Response, st *gortsplib.ServerStream, err error) {
	defer s.guard("setup", &res)
	if res, err := s.admit(ctx.Conn, ctx.Request); res != nil {
		return res, nil, err
	}
	m, _ := ctx.Conn.UserData().(*mount)
	if m == nil {
		return status(base.StatusNotFound), nil, nil // SETUP without DESCRIBE
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed || m.cam.mounts[m.sess.ID] != m:
		return status(base.StatusNotFound), nil, nil // the session ended meanwhile
	case m.client != nil && m.client != ctx.Session:
		return status(base.StatusServiceUnavailable), nil, nil
	}
	m.client = ctx.Session
	ctx.Session.SetUserData(m)
	return status(base.StatusOK), m.st, nil
}

// OnPlay implements gortsplib.ServerHandlerOnPlay. The first PLAY of a camera
// registers its fan-out reader and starts its pump.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (res *base.Response, err error) {
	defer s.guard("play", &res)
	m, _ := ctx.Session.UserData().(*mount)
	if m == nil {
		return status(base.StatusNotFound), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || m.cam.mounts[m.sess.ID] != m || m.client != ctx.Session {
		return status(base.StatusNotFound), nil
	}
	if m.cam.stop == nil {
		reader := s.src.Subscribe(m.cam.id, ReaderQueue)
		if reader == nil {
			return status(base.StatusNotFound), nil
		}
		stop := make(chan struct{})
		m.cam.stop = stop
		s.wg.Add(1)
		go s.pump(m.cam, reader, stop)
	}
	m.playing = true
	return status(base.StatusOK), nil
}

// OnSessionClose implements gortsplib.ServerHandlerOnSessionClose.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	defer func() { recover() }()
	m, _ := ctx.Session.UserData().(*mount)
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.client == ctx.Session {
		s.removeMountLocked(m)
	}
}

// OnStreamWriteError implements gortsplib.ServerHandlerOnStreamWriteError: a
// client that does not keep up is dropped. Nothing ever waits for it.
func (s *Server) OnStreamWriteError(ctx *gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
	defer func() { recover() }()
	s.log.Warn("restream client is not keeping up; disconnecting it", "error", ctx.Error.Error())
	ctx.Session.Close()
}

// ------------------------------------------------------------------- pump

// sweepLocked closes the mounts of sessions that are no longer playing.
func (s *Server) sweepLocked(cam *camera, sessions []Session) {
	for id, m := range cam.mounts {
		live := false
		for _, sess := range sessions {
			live = live || sess.ID == id
		}
		if !live {
			s.removeMountLocked(m)
		}
	}
}

// pump copies a camera's packets from its fan-out reader to the mount of the
// session each packet belongs to. It ends when the last client has left.
func (s *Server) pump(cam *camera, reader *stream.Reader, stop chan struct{}) {
	defer s.wg.Done()
	defer s.src.Unsubscribe(cam.id, reader)
	defer func() {
		if r := recover(); r != nil {
			s.fail(cam, stop, r)
		}
	}()
	tick := time.NewTicker(sweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case pkt := <-reader.C:
			if s.opt.PumpHook != nil {
				s.opt.PumpHook(cam.id)
			}
			s.forward(cam, pkt)
		case <-tick.C:
			sessions, _ := s.src.Sessions(cam.id)
			s.mu.Lock()
			s.sweepLocked(cam, sessions)
			s.mu.Unlock()
		}
	}
}

func (s *Server) forward(cam *camera, pkt *stream.Packet) {
	s.mu.Lock()
	m := cam.mounts[pkt.Session]
	if m == nil || !m.playing {
		s.mu.Unlock()
		return // a session nobody watches
	}
	st, media, f := m.st, m.video, m.sess.Video
	if pkt.Track == stream.TrackAudio {
		media, f = m.audio, m.sess.Audio
	}
	s.mu.Unlock()
	if media == nil || pkt.RTP == nil || pkt.RTP.PayloadType != f.PayloadType() {
		return
	}
	// Fan-out packets are read-only and the server writes its own SSRC into
	// the header, so it gets a copy of the header. The payload is shared and
	// only read.
	cp := &rtp.Packet{Header: pkt.RTP.Header, Payload: pkt.RTP.Payload}
	st.WritePacketRTP(media, cp) //nolint:errcheck // a closed stream: its client is gone
}

// fail handles a panic in a pump: the camera's mounts are closed and its
// restream stays off until the camera has a new session.
func (s *Server) fail(cam *camera, stop chan struct{}, r any) {
	defer func() { recover() }()
	s.log.Error("restream pump panicked", "camera", cam.id, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	sessions, _ := s.src.Sessions(cam.id)
	s.mu.Lock()
	cam.failed, cam.failedAt = true, 0
	if n := len(sessions); n > 0 {
		cam.failedAt = sessions[n-1].ID
	}
	if cam.stop == stop {
		cam.stop = nil
	}
	for _, m := range cam.mounts {
		s.removeMountLocked(m)
	}
	s.mu.Unlock()
	s.src.Failed(cam.id, fmt.Sprint(r))
}

// Active returns how many mounts exist and how many pumps (and with them
// fan-out readers) are running. Both are zero while nobody watches.
func (s *Server) Active() (mounts, pumps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cam := range s.cams {
		mounts += len(cam.mounts)
		if cam.stop != nil {
			pumps++
		}
	}
	return mounts, pumps
}
