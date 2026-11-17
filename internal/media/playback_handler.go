package media

import (
	"net/http"

	"github.com/jxburros/buttercams/internal/ipc"
)

// NewPlaybackHandler serves the /play routes of a Playback without the
// token check of the media process. Tests use it to play recordings the way
// a browser does, through the same code the media process runs.
func NewPlaybackHandler(p *Playback) http.Handler {
	mux := http.NewServeMux()
	route := func(serve func(http.ResponseWriter, *http.Request, []string, bool)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			paths, err := pathsOf(r)
			if err != nil || len(paths) == 0 {
				fail(w, http.StatusBadRequest, "bad_request", "The request names no file.")
				return
			}
			serve(w, r, paths, r.Header.Get(ipc.OpenHeader) == "1")
		}
	}
	mux.HandleFunc("GET /play/index", route(p.ServeIndex))
	mux.HandleFunc("GET /play/data", route(p.ServeData))
	mux.HandleFunc("GET /play/keyframes", route(p.ServeKeyframes))
	mux.HandleFunc("GET /play/thumb", route(p.ServeThumb))
	return mux
}
