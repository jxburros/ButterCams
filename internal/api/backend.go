// Package api is the HTTPS server for the web settings page and the REST API
// (spec 11). It runs inside the guardian process but is isolated from
// recording: every handler has a timeout and panic recovery, and nothing here
// ever waits on a worker.
package api

import (
	"context"
	"os"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
)

// Health statuses.
const (
	StatusHealthy  = "healthy"
	StatusWarning  = "warning"
	StatusCritical = "critical"
)

// Reason is one cause of a non-healthy status.
type Reason struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	CameraID string `json:"cameraId,omitempty"`
}

// Health is the overall status (spec 10.1).
type Health struct {
	Status    string    `json:"status"`
	Reasons   []Reason  `json:"reasons"`
	CheckedAt time.Time `json:"checkedAt"`
}

// CameraStats is the live state of one camera as reported by its worker.
type CameraStats struct {
	ipc.CameraStats
	WorkerID  int       `json:"workerId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DestinationStatus is the live state of one destination (spec 7.3).
type DestinationStatus struct {
	State         string     `json:"state"` // ok | retrying | disabled
	LastError     string     `json:"lastError"`
	QueueLength   int        `json:"queueLength"`
	OldestQueued  *time.Time `json:"oldestQueuedTs"`
	UsedBytes     int64      `json:"usedBytes"`
	FreeBytes     *uint64    `json:"freeBytes"`
	TotalBytes    *uint64    `json:"totalBytes"`
	LastSuccessTS *time.Time `json:"lastSuccessTs"`
}

// StagingStatus is the live state of the staging folder.
type StagingStatus struct {
	Path           string  `json:"path"`
	MinFreePercent float64 `json:"minFreePercent"`
	MinFreeGB      float64 `json:"minFreeGB"`
	FreeBytes      *uint64 `json:"freeBytes"`
	TotalBytes     *uint64 `json:"totalBytes"`
	FloorBytes     *uint64 `json:"floorBytes"`
	UsedBytes      int64   `json:"usedBytes"`
}

// RebuildStatus reports on an index rebuild.
type RebuildStatus struct {
	Running    bool       `json:"running"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Error      string     `json:"error"`
	Segments   int        `json:"segments"`
}

// Backend is what the API needs from the guardian. Every method returns
// promptly from in-memory state or the database; none calls into a worker.
type Backend interface {
	// Config returns a copy of the current configuration.
	Config() *config.Config
	// UpdateConfig applies mutate to a copy of the configuration, validates
	// and saves it, and applies the difference to the running system. A
	// rejected configuration returns a *config.ValidationError.
	UpdateConfig(mutate func(*config.Config) error) (*config.Config, error)

	Health() Health
	CameraStats() map[string]CameraStats
	Storage() (map[string]DestinationStatus, StagingStatus)

	// Index gives read access to recordings and events.
	Index() *index.DB
	// OpenSegment opens the nearest available copy of a segment for reading.
	OpenSegment(ctx context.Context, id int64) (*os.File, index.Segment, error)
	// SegmentPaths returns a segment and the paths of its files, nearest copy
	// first, for the media process to read. For the segment still being
	// recorded it is the file in staging.
	SegmentPaths(ctx context.Context, id int64) (index.Segment, []string, error)
	// Media returns the loopback address of the media process and the token
	// its requests need; ok is false while it is not running.
	Media() (addr, token string, ok bool)
	// MediaStatus reports on the media process.
	MediaStatus() MediaStatus
	// PurgeCamera deletes every indexed recording of a deleted camera.
	PurgeCamera(cameraID string)

	StartRebuild() error
	Rebuild() RebuildStatus

	// Workers returns the number of recorder worker processes.
	Workers() int
	StartedAt() time.Time
	DataRoot() string
	// SetLogRetention applies the log retention setting.
	RecordEvent(e index.Event)
}
