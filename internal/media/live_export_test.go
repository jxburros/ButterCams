package media

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/jxburros/buttercams/internal/ipc"
)

// NewLiveTestServer builds a media server with its real request handler (the
// token check and the routes) for the tests in package media_test, which
// cannot be in this package because the test viewers import it.
func NewLiveTestServer(token string) (*Hub, http.Handler) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{log: log, out: ipc.NewWriter(io.Discard), token: token}
	s.hub = NewHub(log, s.emit)
	s.play = NewPlayback(log)
	return s.hub, s.handler()
}
