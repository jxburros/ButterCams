package media

import (
	"log/slog"
	"net/http"
)

// Playback serves recorded files to the browser's player: a small index per
// file and plain byte ranges (spec 16.5). It keeps no per-viewer state.
type Playback struct {
	log *slog.Logger
}

// NewPlayback creates the playback file server.
func NewPlayback(log *slog.Logger) *Playback { return &Playback{log: log} }

// SetStagingRoots names the folders whose reads count against the read
// budget (spec 16.9, rule 5).
func (p *Playback) SetStagingRoots(roots []string) {}

// ServeIndex is GET /play/index. paths are the candidate locations of the
// file, nearest first; open is true for the file still being recorded.
func (p *Playback) ServeIndex(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	fail(w, http.StatusNotImplemented, "not_implemented", "Playback is not built yet.")
}

// ServeData is GET /play/data: byte ranges of the file.
func (p *Playback) ServeData(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	fail(w, http.StatusNotImplemented, "not_implemented", "Playback is not built yet.")
}

// ServeKeyframes is GET /play/keyframes?from=&to=: keyframe-only fragments.
func (p *Playback) ServeKeyframes(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	fail(w, http.StatusNotImplemented, "not_implemented", "Playback is not built yet.")
}

// ServeThumb is GET /play/thumb?t=: one keyframe as a complete small MP4.
func (p *Playback) ServeThumb(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	fail(w, http.StatusNotImplemented, "not_implemented", "Playback is not built yet.")
}

// OpenFiles returns the number of file handles currently held open.
func (p *Playback) OpenFiles() int { return 0 }

// Close releases every file handle.
func (p *Playback) Close() {}
