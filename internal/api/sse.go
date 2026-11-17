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
// loses events; it never holds anything up.
type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	closed bool
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.subs[ch] = struct{}{}
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

func (h *hub) publish(event string, v any) {
	if h.count() == 0 {
		return
	}
	f := frame(event, v)
	if f == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- f:
		default: // this client is not keeping up
		}
	}
}

// PublishEvent sends a new row of the events table to connected browsers.
func (s *Server) PublishEvent(e index.Event) { s.hub.publish("event", toEventView(e)) }

// PublishHealth sends the overall status after a health check.
func (s *Server) PublishHealth(h Health) {
	s.hub.publish("health", healthView{Health: h, Version: s.healthView().Version,
		UptimeSec: int64(time.Since(s.backend.StartedAt()).Seconds())})
}

func (s *Server) camerasLive() map[string]any {
	type item struct {
		ID    string     `json:"id"`
		Stats *statsView `json:"stats"`
	}
	stats := s.backend.CameraStats()
	cfg := s.backend.Config()
	out := make([]item, 0, len(cfg.Cameras))
	for _, c := range cfg.Cameras {
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
	s.hub.publish("cameras", s.camerasLive())
	s.hub.publish("destinations", s.destinationsPayload())
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

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	for _, f := range [][]byte{frame("health", s.healthView()), frame("cameras", s.camerasLive()),
		frame("destinations", s.destinationsPayload())} {
		if _, err := w.Write(f); err != nil {
			return
		}
	}
	flusher.Flush()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
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
