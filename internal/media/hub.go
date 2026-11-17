package media

import (
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
)

// SourceLinger is how long a source stays open after its last viewer left
// (spec 16.1: nothing runs when nobody watches).
const SourceLinger = 30 * time.Second

const restreamUser = ipc.RestreamUser

// Hub owns the live stream sources: one per camera and stream, shared by all
// its viewers, started on the first viewer and stopped 30 s after the last
// one leaves (spec 16.3).
type Hub struct {
	log  *slog.Logger
	emit func(*ipc.Event)

	// Timing, fixed after NewHub; tests shorten it.
	Linger       time.Duration
	StallTimeout time.Duration
	MainRetry    time.Duration

	overflows atomic.Uint64
	ids       atomic.Uint64

	mu            sync.Mutex
	closed        bool
	cams          map[string]config.Camera
	restreams     map[string]string
	restreamToken string
	sources       map[sourceKey]*source
}

// NewHub creates the stream hub. emit reports events to the guardian.
func NewHub(log *slog.Logger, emit func(*ipc.Event)) *Hub {
	return &Hub{log: log, emit: emit, Linger: SourceLinger, StallTimeout: StallTimeout, MainRetry: MainRetry,
		cams: map[string]config.Camera{}, restreams: map[string]string{}, sources: map[sourceKey]*source{}}
}

func (h *Hub) nextID() uint64 { return h.ids.Add(1) }

// targetOf returns where a source connects to under the current assignment.
func (h *Hub) targetOf(key sourceKey) target {
	h.mu.Lock()
	defer h.mu.Unlock()
	cam, known := h.cams[key.camera]
	return targetFor(key, cam, known, h.restreams[key.camera], h.restreamToken)
}

// Assign replaces the camera list and the addresses of the workers'
// restreams. restreams maps a camera id to "127.0.0.1:port"; a camera that is
// missing has no main stream. Sources whose camera or address changed are
// reconnected; the others are left alone.
func (h *Hub) Assign(cameras []config.Camera, restreams map[string]string, restreamToken string) {
	cams := make(map[string]config.Camera, len(cameras))
	for _, c := range cameras {
		cams[c.ID] = c
	}
	addrs := make(map[string]string, len(restreams))
	for id, a := range restreams {
		addrs[id] = a
	}
	h.mu.Lock()
	h.cams, h.restreams, h.restreamToken = cams, addrs, restreamToken
	var gone, kept []*source
	for key, s := range h.sources {
		if _, ok := cams[key.camera]; ok {
			kept = append(kept, s)
		} else {
			gone = append(gone, s)
			delete(h.sources, key)
		}
	}
	h.mu.Unlock()
	for _, s := range gone {
		s.stop() // the camera was deleted
	}
	for _, s := range kept {
		s.poke(s.kick) // each source compares its target and reconnects if it changed
	}
}

// SourceChanged reports that a camera's main stream has a new session (the
// second session of the hourly rollover, or the one after a reconnect).
func (h *Hub) SourceChanged(cameraID string, session uint64) {
	h.mu.Lock()
	s := h.sources[sourceKey{cameraID, "main"}]
	h.mu.Unlock()
	if s != nil {
		s.poke(s.switchCh)
	}
}

// ServeLive upgrades the request to a WebSocket and streams fragmented MP4
// until the viewer leaves (spec 16.4).
func (h *Hub) ServeLive(w http.ResponseWriter, r *http.Request, cameraID, stream string, audio bool) {
	key := sourceKey{cameraID, stream}
	h.mu.Lock()
	_, known := h.cams[cameraID]
	closed := h.closed
	h.mu.Unlock()
	if closed {
		fail(w, http.StatusServiceUnavailable, "media_unavailable", "Live view is restarting.")
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "not_found", "No such camera.")
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered
	}
	v := newViewer(audio && stream == "main")

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.Close()
		return
	}
	s := h.sources[key]
	if s == nil {
		s = newSource(h, key)
		h.sources[key] = s
		go s.run()
	}
	s.attach(v)
	h.mu.Unlock()

	v.serve(c) // until the viewer leaves

	if s.detach(v) == 0 {
		h.idle(s)
	}
}

// idle starts the linger of a source that has no viewers.
func (h *Hub) idle(s *source) {
	s.mu.Lock()
	if s.linger != nil {
		s.linger.Stop()
	}
	s.linger = time.AfterFunc(h.Linger, func() {
		defer func() { recover() }()
		h.mu.Lock()
		n, _ := s.counts()
		if n > 0 || h.sources[s.key] != s {
			h.mu.Unlock()
			return
		}
		delete(h.sources, s.key)
		h.mu.Unlock()
		s.stop()
	})
	s.mu.Unlock()
}

// Stats returns the number of viewers, the open sources and how many times a
// slow viewer's queue was emptied.
func (h *Hub) Stats() (viewers int, sources []ipc.MediaSource, overflows uint64) {
	h.mu.Lock()
	list := make([]*source, 0, len(h.sources))
	for _, s := range h.sources {
		list = append(list, s)
	}
	h.mu.Unlock()
	sources = make([]ipc.MediaSource, 0, len(list))
	for _, s := range list {
		s.mu.Lock()
		n, state := len(s.viewers), s.state
		s.mu.Unlock()
		viewers += n
		sources = append(sources, ipc.MediaSource{CameraID: s.key.camera, Stream: s.key.stream, Viewers: n, State: state})
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].CameraID != sources[j].CameraID {
			return sources[i].CameraID < sources[j].CameraID
		}
		return sources[i].Stream < sources[j].Stream
	})
	return viewers, sources, h.overflows.Load()
}

// Close stops every source and disconnects every viewer.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	list := make([]*source, 0, len(h.sources))
	for key, s := range h.sources {
		list = append(list, s)
		delete(h.sources, key)
	}
	h.mu.Unlock()
	for _, s := range list {
		s.stop()
	}
}
