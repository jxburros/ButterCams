package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/winsvc"
)

func isRemoteDrive(path string) bool { return winsvc.IsRemoteDrive(path) }

// hub fans server-sent events out to connected browsers. A slow client
// loses events; it never holds anything up. Each subscriber carries the
// camera permissions of its user, so a limited Viewer never receives events
// or statistics of other cameras (spec 16.6).
type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]*subscriber
	closed bool
}

type subscriber struct {
	access func() camAccess // read at publish time: permission changes apply at once
}

func newHub() *hub { return &hub{subs: map[chan []byte]*subscriber{}} }

func (h *hub) subscribe(access func() camAccess) chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.subs[ch] = &subscriber{access: access}
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

func frame(event string, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}

// publishFor sends each subscriber the frame built for its permissions.
// Frames for unlimited subscribers are built once. build returns nil to send
// nothing to that subscriber.
func (h *hub) publishFor(build func(camAccess) []byte) {
	h.mu.Lock()
	subs := make(map[chan []byte]*subscriber, len(h.subs))
	for ch, sub := range h.subs {
		subs[ch] = sub
	}
	h.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	var shared []byte
	sharedBuilt := false
	frames := make(map[chan []byte][]byte, len(subs))
	for ch, sub := range subs {
		acc := allCameras
		if sub.access != nil {
			acc = sub.access()
		}
		if !acc.limited {
			if !sharedBuilt {
				shared, sharedBuilt = build(acc), true
			}
			frames[ch] = shared
			continue
		}
		frames[ch] = build(acc)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, f := range frames {
		if f == nil {
			continue
		}
		if _, still := h.subs[ch]; !still {
			continue // unsubscribed (and closed) meanwhile
		}
		select {
		case ch <- f:
		default: // this client is not keeping up
		}
	}
}

// PublishEvent sends a new row of the events table to connected browsers.
func (s *Server) PublishEvent(e index.Event) {
	v := toEventView(e)
	s.hub.publishFor(func(acc camAccess) []byte {
		if e.CameraID != "" && !acc.allowed(e.CameraID) {
			return nil
		}
		return frame("event", v)
	})
}

// PublishHealth sends the overall status after a health check.
func (s *Server) PublishHealth(Health) {
	s.hub.publishFor(func(acc camAccess) []byte { return frame("health", s.healthViewFor(acc)) })
}

func (s *Server) camerasLive(acc camAccess) map[string]any {
	type item struct {
		ID    string     `json:"id"`
		Stats *statsView `json:"stats"`
	}
	stats := s.backend.CameraStats()
	cfg := s.backend.Config()
	out := make([]item, 0, len(cfg.Cameras))
	for _, c := range cfg.Cameras {
		if !acc.allowed(c.ID) {
			continue
		}
		it := item{ID: c.ID}
		if st, ok := stats[c.ID]; ok {
			it.Stats = toStatsView(st)
		}
		out = append(out, it)
	}
	return map[string]any{"cameras": out}
}

// PublishLive sends camera statistics and storage state; the guardian calls
// it every five seconds. It does nothing when nobody is connected.
func (s *Server) PublishLive() {
	if s.hub.count() == 0 {
		return
	}
	s.hub.publishFor(func(acc camAccess) []byte { return frame("cameras", s.camerasLive(acc)) })
	dest := frame("destinations", s.destinationsPayload())
	s.hub.publishFor(func(camAccess) []byte { return dest })
}

// handleStream is GET /api/v1/stream (Server-Sent Events).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "Streaming is not supported by this connection.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sess, _ := sessionOf(r)
	access := func() camAccess { return s.accessFor(sess.username, sess.role) }
	ch := s.hub.subscribe(access)
	defer s.hub.unsubscribe(ch)
	acc := access()
	for _, f := range [][]byte{frame("health", s.healthViewFor(acc)), frame("cameras", s.camerasLive(acc)),
		frame("destinations", s.destinationsPayload())} {
		if _, err := w.Write(f); err != nil {
			return
		}
	}
	flusher.Flush()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	check := time.NewTicker(5 * time.Second)
	defer check.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-check.C:
			// Sign-out, a deleted account or an expired session ends the
			// stream; it must not keep delivering after that.
			if !s.sessions.alive(sess.token) {
				return
			}
		case f, open := <-ch:
			if !open {
				return
			}
			if _, err := w.Write(f); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
