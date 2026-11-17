package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
)

// Limits of live view and playback (spec 16.4).
const (
	// MaxMediaConnections is the total number of proxied media requests in
	// flight.
	MaxMediaConnections = 200
	// MaxStreamsPerCamera is how many live streams of one camera one user
	// session may have open.
	MaxStreamsPerCamera = 4
	// MediaDialTimeout is how long the media process has to accept a proxied
	// request before the guardian answers 503.
	MediaDialTimeout = 2 * time.Second
)

// MediaStatus is the state of the media process for GET /system and the
// Dashboard.
type MediaStatus struct {
	Enabled   bool              `json:"enabled"`
	State     string            `json:"state"` // "running", "starting" or "off"
	Viewers   int               `json:"viewers"`
	Sources   []ipc.MediaSource `json:"sources"`
	Overflows uint64            `json:"overflows"`
	MemBytes  uint64            `json:"memBytes"`
}

// mediaGate limits the load viewers can put on the guardian: it only ever
// copies bytes, and only for a bounded number of connections (spec 16.9).
type mediaGate struct {
	slots chan struct{}

	mu      sync.Mutex
	streams map[string]int // session token + camera → open live streams

	transport *http.Transport
}

func newMediaGate() *mediaGate {
	dialer := &net.Dialer{Timeout: MediaDialTimeout}
	return &mediaGate{
		slots:   make(chan struct{}, MaxMediaConnections),
		streams: map[string]int{},
		transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true, // video does not compress, and nothing may be buffered
		},
	}
}

func (g *mediaGate) acquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *mediaGate) release() { <-g.slots }

func (g *mediaGate) addStream(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streams[key] >= MaxStreamsPerCamera {
		return false
	}
	g.streams[key]++
	return true
}

func (g *mediaGate) dropStream(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streams[key] <= 1 {
		delete(g.streams, key)
	} else {
		g.streams[key]--
	}
}

// sameOrigin reports whether a WebSocket request comes from a page of this
// server. Browsers always send Origin on WebSocket handshakes; a request
// without one is not from a browser page and carries no ambient cookie risk.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// proxyMedia forwards the request to the media process as path, adding the
// run's token and the given headers. Nothing is buffered.
func (s *Server) proxyMedia(w http.ResponseWriter, r *http.Request, path string, extra map[string]string) {
	if !s.backend.Config().Live.Enabled {
		writeError(w, http.StatusServiceUnavailable, "live_disabled", "Live view and playback are switched off on the System page.")
		return
	}
	addr, token, ok := s.backend.Media()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "media_unavailable", "Live view is restarting. It will be back in a few seconds.")
		return
	}
	if !s.media.acquire() {
		writeError(w, http.StatusServiceUnavailable, "media_busy", "Too many video connections are open. Close some tiles or tabs and try again.")
		return
	}
	defer s.media.release()

	proxy := &httputil.ReverseProxy{
		Transport:     s.media.transport,
		FlushInterval: -1, // write through at once: live fragments must not wait
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: addr, Path: path, RawQuery: r.URL.RawQuery}
			pr.Out.Host = addr
			// The media process trusts the guardian only: it gets the token
			// and never the browser's session.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del(CSRFHeader)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Origin") // checked here; the media process sees only the guardian
			for _, h := range []string{ipc.TokenHeader, ipc.PathsHeader, ipc.OpenHeader} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set(ipc.TokenHeader, token)
			for k, v := range extra {
				pr.Out.Header.Set(k, v)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the viewer left
			}
			s.log.Debug("media request failed", "path", path, "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "media_unavailable", "Live view is restarting. It will be back in a few seconds.")
		},
	}
	proxy.ServeHTTP(w, r)
}

// handleLive is GET /live/{camera}/{main|sub}: the WebSocket live stream.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	cam, stream := r.PathValue("camera"), r.PathValue("stream")
	if !naming.ValidID(cam) || (stream != "main" && stream != "sub") {
		writeError(w, http.StatusNotFound, "not_found", "No such stream.")
		return
	}
	if !s.cameraAllowed(w, r, cam) {
		return
	}
	if _, ok := s.backend.Config().Camera(cam); !ok {
		writeError(w, http.StatusNotFound, "not_found", "No such camera.")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "This page may not open video streams.")
		return
	}
	sess, _ := sessionOf(r)
	key := sess.token + "\x00" + cam
	if !s.media.addStream(key) {
		writeError(w, http.StatusServiceUnavailable, "too_many_streams", "This camera is already open in four places in this browser session.")
		return
	}
	defer s.media.dropStream(key)
	s.proxyMedia(w, r, "/live/"+cam+"/"+stream, nil)
}

// handlePlay is GET /segments/{id}/play/{index|data|keyframes|thumb}. The
// guardian resolves the segment to file paths (nearest copy first) and the
// media process serves from the first it can open.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	what := r.PathValue("what")
	switch what {
	case "index", "data", "keyframes", "thumb":
	default:
		writeError(w, http.StatusNotFound, "not_found", "No such API endpoint.")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such recording.")
		return
	}
	if _, ok := s.segmentAllowed(w, r, id); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	seg, paths, err := s.backend.SegmentPaths(ctx, id)
	cancel()
	if err != nil || len(paths) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "No copy of this recording can be reached right now.")
		return
	}
	list, _ := json.Marshal(paths)
	extra := map[string]string{ipc.PathsHeader: base64.RawURLEncoding.EncodeToString(list)}
	if !seg.Closed() {
		extra[ipc.OpenHeader] = "1"
	}
	s.proxyMedia(w, r, "/play/"+what, extra)
}
