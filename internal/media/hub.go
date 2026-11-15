package media

import (
	"log/slog"
	"net/http"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
)

// Hub owns the live stream sources: one per camera and stream, shared by all
// its viewers, started on the first viewer and stopped 30 s after the last
// one leaves (spec 16.3).
type Hub struct {
	log  *slog.Logger
	emit func(*ipc.Event)
}

// NewHub creates the stream hub. emit reports events to the guardian.
func NewHub(log *slog.Logger, emit func(*ipc.Event)) *Hub {
	return &Hub{log: log, emit: emit}
}

// Assign replaces the camera list and the addresses of the workers'
// restreams. restreams maps a camera id to "127.0.0.1:port"; a camera that is
// missing has no main stream. Sources whose camera or address changed are
// reconnected; the others are left alone.
func (h *Hub) Assign(cameras []config.Camera, restreams map[string]string, restreamToken string) {}

// SourceChanged reports that a camera's main stream has a new session (the
// second session of the hourly rollover, or the one after a reconnect).
func (h *Hub) SourceChanged(cameraID string, session uint64) {}

// ServeLive upgrades the request to a WebSocket and streams fragmented MP4
// until the viewer leaves (spec 16.4).
func (h *Hub) ServeLive(w http.ResponseWriter, r *http.Request, cameraID, stream string, audio bool) {
	fail(w, http.StatusNotImplemented, "not_implemented", "Live view is not built yet.")
}

// Stats returns the number of viewers, the open sources and how many times a
// slow viewer's queue was emptied.
func (h *Hub) Stats() (viewers int, sources []ipc.MediaSource, overflows uint64) { return 0, nil, 0 }

// Close stops every source and disconnects every viewer.
func (h *Hub) Close() {}
