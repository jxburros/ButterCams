package ipc

import "github.com/jxburros/buttercams/internal/config"

// Messages of live view and playback (spec 16.8).

// Restream switches a worker's loopback restream on or off. With Enabled the
// worker listens on 127.0.0.1 and answers with RestreamReady; clients must
// present Token. The guardian sends it after every Assign and whenever the
// setting changes.
type Restream struct {
	Header
	Enabled bool   `json:"enabled"`
	Token   string `json:"token,omitempty"`
}

// RestreamUser is the RTSP user name restream clients sign in with; the
// password is the token of the Restream message.
const RestreamUser = "buttercams"

// RestreamReady reports the loopback port of a worker's restream. Port 0
// means the restream is off (switched off, or it could not listen).
type RestreamReady struct {
	Header
	WorkerID int    `json:"workerId"`
	Port     int    `json:"port"`
	Error    string `json:"error,omitempty"`
}

// RestreamSession reports that a camera has a new RTSP session playing, so
// that rtsp://127.0.0.1:<port>/<camera> now describes that session. It is
// sent for every session: the first one, the second one of the hourly
// rollover, and the one after a reconnect.
type RestreamSession struct {
	Header
	CameraID string `json:"cameraId"`
	Session  uint64 `json:"session"`
}

// AssignMedia gives the media process everything it needs: the token every
// HTTP request must carry, the cameras (with stored, still protected
// passwords, for their sub streams) and where each camera's main stream can
// be read. The guardian sends it when the media process starts and whenever
// any of it changes.
type AssignMedia struct {
	Header
	// Token must be in the TokenHeader of every request to the media process.
	Token   string          `json:"token"`
	Cameras []config.Camera `json:"cameras"`
	// Restreams maps a camera id to "127.0.0.1:port" of the restream of the
	// worker that records it. A camera that is missing has no main stream
	// available (live.mainStream is off, or its worker has not reported).
	Restreams map[string]string `json:"restreams,omitempty"`
	// RestreamToken is the password for the workers' restream.
	RestreamToken string `json:"restreamToken,omitempty"`
	// StagingRoots are the folders whose reads count against the read budget.
	StagingRoots []string `json:"stagingRoots,omitempty"`
}

// Headers on requests from the guardian to the media process.
const (
	// TokenHeader carries the media token.
	TokenHeader = "X-ButterCams-Token"
	// PathsHeader carries the candidate file paths of a segment for the
	// /play/ routes, nearest copy first: a JSON array of strings, base64
	// (URL alphabet, no padding) so that any path survives as a header.
	PathsHeader = "X-ButterCams-Paths"
	// OpenHeader is "1" on /play/ requests for the segment still being
	// recorded.
	OpenHeader = "X-ButterCams-Open"
)

// SourceChanged tells the media process that a camera's main stream has a
// new session to offer (the forwarded RestreamSession).
type SourceChanged struct {
	Header
	CameraID string `json:"cameraId"`
	Session  uint64 `json:"session"`
}

// MediaReady reports the loopback port the media process listens on.
type MediaReady struct {
	Header
	Port int `json:"port"`
}

// MediaSource is one open stream source in MediaStats.
type MediaSource struct {
	CameraID string `json:"cameraId"`
	Stream   string `json:"stream"` // "main" or "sub"
	Viewers  int    `json:"viewers"`
	State    string `json:"state"` // "playing", "connecting" or "waiting"
}

// MediaStats is the media process's periodic report.
type MediaStats struct {
	Header
	Viewers   int           `json:"viewers"`
	Sources   []MediaSource `json:"sources"`
	Overflows uint64        `json:"overflows"` // viewer queues emptied because a viewer was slow
	OpenFiles int           `json:"openFiles"`
}
