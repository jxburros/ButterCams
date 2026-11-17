// Package media implements the media process (spec 16.2): everything that
// serves video to browsers. It is started and supervised by the guardian as
// "buttercams media" and is isolated from recording: it never opens the
// database, never writes to staging, destinations or the data root, and a
// crash, hang or overload here leaves recording untouched.
//
// It listens on 127.0.0.1 only, on a port it chooses, and serves only
// requests that carry the token the guardian generated for this run. The
// guardian checks the session, role and camera permission and then
// reverse-proxies:
//
//	GET /live/{camera}/{main|sub}?audio=0|1   WebSocket live stream  (hub.go, live.go, ws.go)
//	GET /play/index                            playback index of a file (playback.go)
//	GET /play/data                             the file's bytes, range requests only
//	GET /play/keyframes?from=&to=              keyframe-only fragments
//	GET /play/thumb?t=                         one keyframe as a small MP4
//
// The /play routes get the candidate paths of the file in ipc.PathsHeader.
package media

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Reporting intervals (spec 16.2).
const (
	HeartbeatInterval = 5 * time.Second
	StatsInterval     = 5 * time.Second
)

// Server is the media process.
type Server struct {
	log *slog.Logger
	out *ipc.Writer

	hub  *Hub
	play *Playback

	mu    sync.Mutex
	token string
	ln    net.Listener
	srv   *http.Server
}

// Run executes the media process until the guardian asks it to stop, stdin
// closes (the guardian is gone) or ctx is cancelled.
func Run(ctx context.Context, in io.Reader, out io.Writer, log *slog.Logger) error {
	s := &Server{log: log, out: ipc.NewWriter(out)}
	s.hub = NewHub(log, s.emit)
	s.play = NewPlayback(log)
	defer s.close()
	return s.run(ctx, in)
}

func (s *Server) send(msg any) {
	if err := s.out.Send(msg); err != nil {
		s.log.Debug("could not send message to the guardian", "error", err.Error())
	}
}

// emit reports an event to the guardian.
func (s *Server) emit(e *ipc.Event) { s.send(e) }

func (s *Server) run(ctx context.Context, in io.Reader) error {
	msgs := make(chan any, 16)
	readErr := make(chan error, 1)
	go func() {
		r := ipc.NewReader(in)
		for {
			m, err := r.Next()
			if errors.Is(err, ipc.ErrMalformed) {
				s.log.Warn("ignoring malformed message from the guardian", "error", err.Error())
				continue
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
			msgs <- m
		}
	}()

	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	stats := time.NewTicker(StatsInterval)
	defer stats.Stop()

	s.send(&ipc.Heartbeat{MemBytes: winsvc.ProcessMemoryBytes()})
	s.log.Info("media process started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			s.log.Info("media process stopping", "reason", "guardian connection closed")
			return err
		case m := <-msgs:
			if s.handle(m) {
				s.log.Info("media process stopping", "reason", "shutdown requested")
				return nil
			}
		case <-heartbeat.C:
			s.send(&ipc.Heartbeat{MemBytes: winsvc.ProcessMemoryBytes()})
		case <-stats.C:
			viewers, sources, overflows := s.hub.Stats()
			if sources == nil {
				sources = []ipc.MediaSource{}
			}
			s.send(&ipc.MediaStats{Viewers: viewers, Sources: sources, Overflows: overflows, OpenFiles: s.play.OpenFiles()})
		}
	}
}

// handle processes one guardian message. It returns true on shutdown.
func (s *Server) handle(m any) (stop bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("handling a guardian message panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	switch msg := m.(type) {
	case *ipc.AssignMedia:
		s.mu.Lock()
		s.token = msg.Token
		listening := s.ln != nil
		s.mu.Unlock()
		s.hub.Assign(msg.Cameras, msg.Restreams, msg.RestreamToken)
		s.play.SetStagingRoots(msg.StagingRoots)
		if !listening {
			port, err := s.listen()
			if err != nil {
				s.log.Error("the media process cannot listen", "error", err.Error())
				s.emit(&ipc.Event{Severity: ipc.SevWarning, Code: "media_listen_failed",
					Message: "Live view cannot start: " + err.Error()})
				return true // exit; the guardian starts the process again
			}
			s.send(&ipc.MediaReady{Port: port})
		}
	case *ipc.SourceChanged:
		s.hub.SourceChanged(msg.CameraID, msg.Session)
	case *ipc.Shutdown:
		return true
	}
	return false
}

// listen starts the loopback HTTP server on a free port.
func (s *Server) listen() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    256 << 10, // the path list of a segment travels in a header
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	s.mu.Lock()
	s.ln, s.srv = ln, srv
	s.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("media server panicked", "panic", fmt.Sprint(r))
			}
		}()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("media server stopped", "error", err.Error())
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) close() {
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ln = nil, nil
	s.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	s.hub.Close()
	s.play.Close()
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// handler routes requests. Every request must carry the run's token and come
// from this computer; anything else gets 403.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live/{camera}/{stream}", func(w http.ResponseWriter, r *http.Request) {
		cam, stream := r.PathValue("camera"), r.PathValue("stream")
		if !naming.ValidID(cam) || (stream != "main" && stream != "sub") {
			fail(w, http.StatusNotFound, "not_found", "No such stream.")
			return
		}
		s.hub.ServeLive(w, r, cam, stream, r.URL.Query().Get("audio") == "1")
	})
	play := func(serve func(http.ResponseWriter, *http.Request, []string, bool)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			paths, err := pathsOf(r)
			if err != nil || len(paths) == 0 {
				fail(w, http.StatusBadRequest, "bad_request", "The request names no file.")
				return
			}
			serve(w, r, paths, r.Header.Get(ipc.OpenHeader) == "1")
		}
	}
	mux.HandleFunc("GET /play/index", play(s.play.ServeIndex))
	mux.HandleFunc("GET /play/data", play(s.play.ServeData))
	mux.HandleFunc("GET /play/keyframes", play(s.play.ServeKeyframes))
	mux.HandleFunc("GET /play/thumb", play(s.play.ServeThumb))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("media request panicked", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				defer func() { recover() }()
				fail(w, http.StatusInternalServerError, "internal", "Something went wrong in the live view process.")
			}
		}()
		s.mu.Lock()
		token := s.token
		s.mu.Unlock()
		got := r.Header.Get(ipc.TokenHeader)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 || ip == nil || !ip.IsLoopback() {
			fail(w, http.StatusForbidden, "forbidden", "The media process only serves the ButterCams service.")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// pathsOf decodes the candidate file paths the guardian sent with a /play
// request.
func pathsOf(r *http.Request) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(ipc.PathsHeader))
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, err
	}
	return paths, nil
}

// EncodePaths builds the value of ipc.PathsHeader. Tests use it; the
// guardian's proxy builds the same value.
func EncodePaths(paths []string) string {
	b, _ := json.Marshal(paths)
	return base64.RawURLEncoding.EncodeToString(b)
}
