// Package ipc defines the JSON-lines messages exchanged between the guardian
// and its supervised child processes over stdin/stdout (spec 4.4).
//
// Every message is one JSON object on one line and carries "type" and "ts"
// (RFC 3339 with milliseconds). Unknown types and unknown fields are ignored so
// that newer and older processes can talk to each other during an upgrade.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/config"
)

// TimeFormat is RFC 3339 with milliseconds.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Time marshals as RFC 3339 with milliseconds. The zero value marshals as null.
type Time struct{ time.Time }

// Now returns the current time as an ipc.Time.
func Now() Time { return Time{time.Now()} }

// T wraps a time.Time.
func T(t time.Time) Time { return Time{t} }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.Format(TimeFormat) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || string(b) == `""` {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// Message type names.
const (
	// guardian → worker
	TypeAssign        = "assign"
	TypeUpdateCamera  = "update_camera"
	TypeRemoveCamera  = "remove_camera"
	TypeRestartCamera = "restart_camera"
	TypeSetStaging    = "set_staging"
	TypeShutdown      = "shutdown"

	// worker → guardian
	TypeHeartbeat     = "heartbeat"
	TypeCameraStats   = "camera_stats"
	TypeSegmentOpened = "segment_opened"
	TypeSegmentClosed = "segment_closed"
	TypeEvent         = "event"

	// guardian → mover
	TypeDestinations = "destinations"
	TypeCopyJob      = "copy_job"
	TypeCancelCopy   = "cancel_copy"
	TypeRetentionJob = "retention_job"

	// mover → guardian
	TypeCopyResult      = "copy_result"
	TypeDiskStatus      = "disk_status"
	TypeRetentionResult = "retention_result"

	// guardian → worker (spec 16.8)
	TypeRestream = "restream"

	// worker → guardian
	TypeRestreamReady   = "restream_ready"
	TypeRestreamSession = "restream_session"

	// guardian → media
	TypeAssignMedia   = "assign_media"
	TypeSourceChanged = "source_changed"

	// media → guardian (heartbeat and event are shared with the workers)
	TypeMediaReady = "media_ready"
	TypeMediaStats = "media_stats"
)

// Header is embedded in every message.
type Header struct {
	Type string `json:"type"`
	TS   Time   `json:"ts"`
}

func (h *Header) stamp(typ string) {
	h.Type = typ
	if h.TS.IsZero() {
		h.TS = Now()
	}
}

// Assign gives a worker its complete camera list.
type Assign struct {
	Header
	StagingPath string          `json:"stagingPath"`
	Cameras     []config.Camera `json:"cameras"`
	// UsedNames lists, per camera, the recording names of the last hours
	// that are already in the index. A camera never reuses them, even when
	// the files have since left the staging folder.
	UsedNames map[string][]string `json:"usedNames,omitempty"`
}

// UpdateCamera adds a camera or replaces its configuration.
type UpdateCamera struct {
	Header
	Camera    config.Camera `json:"camera"`
	UsedNames []string      `json:"usedNames,omitempty"`
}

// RemoveCamera stops and forgets a camera.
type RemoveCamera struct {
	Header
	CameraID string `json:"cameraId"`
}

// RestartCamera forces a camera to close its file and reconnect. The guardian
// sends it when the on-disk watchdog (spec 10.2) sees a file that is not growing.
type RestartCamera struct {
	Header
	CameraID string `json:"cameraId"`
	Reason   string `json:"reason"`
}

// SetStaging changes the staging folder. Cameras pick it up at their next file.
type SetStaging struct {
	Header
	StagingPath string `json:"stagingPath"`
}

// Shutdown asks a child to finalize its work and exit.
type Shutdown struct {
	Header
}

// Heartbeat is sent by every child every few seconds.
type Heartbeat struct {
	Header
	WorkerID int    `json:"workerId,omitempty"`
	MemBytes uint64 `json:"memBytes"`
}

// Camera states reported by workers (spec 6.1).
const (
	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateRecording  = "recording"
	StateBackoff    = "backoff"
	StateError      = "error"
)

// CameraStats is the periodic per-camera report.
type CameraStats struct {
	Header
	CameraID           string  `json:"cameraId"`
	State              string  `json:"state"`
	StateSince         Time    `json:"stateSince"`
	Message            string  `json:"message,omitempty"`
	LastPacketTS       Time    `json:"lastPacketTs"`
	VideoCodec         string  `json:"videoCodec,omitempty"`
	AudioCodec         string  `json:"audioCodec,omitempty"`
	AudioNote          string  `json:"audioNote,omitempty"`
	Width              int     `json:"width,omitempty"`
	Height             int     `json:"height,omitempty"`
	FPS                float64 `json:"fps,omitempty"`
	KeyframeIntervalMs int     `json:"keyframeIntervalMs,omitempty"`
	BitrateKbps        int     `json:"bitrateKbps,omitempty"`
	CurrentFile        string  `json:"currentFile,omitempty"`
	CurrentFileBytes   int64   `json:"currentFileBytes,omitempty"`
	SingleSession      bool    `json:"singleSession,omitempty"`
	DroppedPackets     uint64  `json:"droppedPackets,omitempty"`
	// RestreamDropped counts packets the loopback restream lost because its
	// client (the media process) was not keeping up (spec 16.3). The recorder
	// itself never waits for it.
	RestreamDropped uint64 `json:"restreamDropped,omitempty"`
}

// SegmentOpened announces a new recording file.
type SegmentOpened struct {
	Header
	CameraID   string `json:"cameraId"`
	Path       string `json:"path"`
	Start      Time   `json:"start"`
	Gap        bool   `json:"gap"`
	VideoCodec string `json:"videoCodec,omitempty"`
	AudioCodec string `json:"audioCodec,omitempty"`
}

// Close reasons (spec 10.5).
const (
	ReasonHourRollover   = "hour_rollover"
	ReasonStall          = "stall"
	ReasonShutdown       = "shutdown"
	ReasonCrashRecovered = "crash_recovered"
	ReasonParamChange    = "param_change"
	ReasonClockJump      = "clock_jump"
)

// SegmentClosed announces a finished recording file. Path is the final
// (renamed) path. Gap is true when the recording was not continuous with the
// previous file of the same camera.
type SegmentClosed struct {
	Header
	CameraID   string `json:"cameraId"`
	Path       string `json:"path"`
	Start      Time   `json:"start"`
	End        Time   `json:"end"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Gap        bool   `json:"gap"`
	Reason     string `json:"reason"`
	VideoCodec string `json:"videoCodec,omitempty"`
	AudioCodec string `json:"audioCodec,omitempty"`
}

// Severities.
const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Event is a noteworthy occurrence reported by a child.
type Event struct {
	Header
	CameraID string         `json:"cameraId,omitempty"`
	Severity string         `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Data     map[string]any `json:"data,omitempty"`
}

// Destinations gives the mover the current storage configuration.
type Destinations struct {
	Header
	Staging config.Staging `json:"staging"`
	// OldStagingPaths are earlier staging folders that may still hold files
	// recorded before the staging path was changed (spec 7.1).
	OldStagingPaths []string             `json:"oldStagingPaths,omitempty"`
	Destinations    []config.Destination `json:"destinations"`
}

// CopyJob asks the mover to copy one closed segment to one destination.
type CopyJob struct {
	Header
	SegmentID     int64  `json:"segmentId"`
	CameraID      string `json:"cameraId"`
	FileName      string `json:"fileName"`
	SourcePath    string `json:"sourcePath"`
	DestinationID string `json:"destinationId"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	Start         Time   `json:"start"`
}

// CancelCopy withdraws queued copies. Empty DestinationID means every
// destination; zero SegmentID means every segment.
type CancelCopy struct {
	Header
	SegmentID     int64  `json:"segmentId,omitempty"`
	DestinationID string `json:"destinationId,omitempty"`
}

// CopyResult reports one copy attempt.
type CopyResult struct {
	Header
	SegmentID     int64  `json:"segmentId"`
	DestinationID string `json:"destinationId"`
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	// SourceMissing is true when the staging file no longer exists; the copy
	// can never succeed and is not retried.
	SourceMissing bool `json:"sourceMissing,omitempty"`
	Attempt       int  `json:"attempt"`
	// NextRetrySec is the delay before the mover tries this destination again.
	NextRetrySec int `json:"nextRetrySec,omitempty"`
}

// StagingID is the destination id used for the staging copy of a segment.
const StagingID = "staging"

// DiskStatus reports the free space of a destination (or of staging). The
// mover sends it every five minutes and after every verified copy; the guardian
// answers with a RetentionJob when something must be deleted.
type DiskStatus struct {
	Header
	DestinationID string `json:"destinationId"`
	Reachable     bool   `json:"reachable"`
	FreeBytes     uint64 `json:"freeBytes"`
	TotalBytes    uint64 `json:"totalBytes"`
	Error         string `json:"error,omitempty"`
	QueueLength   int    `json:"queueLength"`
}

// Delete reasons used by retention.
const (
	DeleteCap           = "cap"
	DeleteMaxAge        = "max_age"
	DeleteDiskFull      = "disk_full"
	DeleteStagingFloor  = "staging_floor"
	DeleteCopied        = "copied"
	DeleteCameraDeleted = "camera_deleted"
)

// DeleteItem is one file the mover must delete.
type DeleteItem struct {
	SegmentID int64  `json:"segmentId"`
	CameraID  string `json:"cameraId"`
	FileName  string `json:"fileName"`
	Bytes     int64  `json:"bytes"`
	Reason    string `json:"reason"`
}

// RetentionJob lists indexed files to delete from one destination.
type RetentionJob struct {
	Header
	DestinationID string       `json:"destinationId"`
	Deletes       []DeleteItem `json:"deletes"`
}

// DeleteResult is the outcome for one DeleteItem.
type DeleteResult struct {
	SegmentID int64  `json:"segmentId"`
	FileName  string `json:"fileName"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Reason    string `json:"reason"`
	Deleted   bool   `json:"deleted"`
	Missing   bool   `json:"missing"`
	Error     string `json:"error,omitempty"`
}

// RetentionResult reports the outcome of a RetentionJob.
type RetentionResult struct {
	Header
	DestinationID string         `json:"destinationId"`
	Results       []DeleteResult `json:"results"`
}

type stamper interface{ stamp(string) }

func typeOf(msg any) (string, error) {
	switch msg.(type) {
	case *Assign:
		return TypeAssign, nil
	case *UpdateCamera:
		return TypeUpdateCamera, nil
	case *RemoveCamera:
		return TypeRemoveCamera, nil
	case *RestartCamera:
		return TypeRestartCamera, nil
	case *SetStaging:
		return TypeSetStaging, nil
	case *Shutdown:
		return TypeShutdown, nil
	case *Heartbeat:
		return TypeHeartbeat, nil
	case *CameraStats:
		return TypeCameraStats, nil
	case *SegmentOpened:
		return TypeSegmentOpened, nil
	case *SegmentClosed:
		return TypeSegmentClosed, nil
	case *Event:
		return TypeEvent, nil
	case *Destinations:
		return TypeDestinations, nil
	case *CopyJob:
		return TypeCopyJob, nil
	case *CancelCopy:
		return TypeCancelCopy, nil
	case *CopyResult:
		return TypeCopyResult, nil
	case *DiskStatus:
		return TypeDiskStatus, nil
	case *RetentionJob:
		return TypeRetentionJob, nil
	case *RetentionResult:
		return TypeRetentionResult, nil
	case *Restream:
		return TypeRestream, nil
	case *RestreamReady:
		return TypeRestreamReady, nil
	case *RestreamSession:
		return TypeRestreamSession, nil
	case *AssignMedia:
		return TypeAssignMedia, nil
	case *SourceChanged:
		return TypeSourceChanged, nil
	case *MediaReady:
		return TypeMediaReady, nil
	case *MediaStats:
		return TypeMediaStats, nil
	}
	return "", fmt.Errorf("ipc: unknown message %T", msg)
}

// Marshal encodes a message as one JSON line (without the newline), filling
// in "type" and "ts".
func Marshal(msg any) ([]byte, error) {
	typ, err := typeOf(msg)
	if err != nil {
		return nil, err
	}
	msg.(stamper).stamp(typ)
	return json.Marshal(msg)
}

// ErrMalformed is wrapped by the error returned for a line that is not a
// valid message. The stream itself is still usable.
var ErrMalformed = errors.New("malformed message")

// Unmarshal decodes one JSON line into its typed message. It returns
// (nil, nil) for a well-formed message of an unknown type.
func Unmarshal(line []byte) (any, error) {
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, fmt.Errorf("ipc: %w: %v", ErrMalformed, err)
	}
	var msg any
	switch h.Type {
	case TypeAssign:
		msg = &Assign{}
	case TypeUpdateCamera:
		msg = &UpdateCamera{}
	case TypeRemoveCamera:
		msg = &RemoveCamera{}
	case TypeRestartCamera:
		msg = &RestartCamera{}
	case TypeSetStaging:
		msg = &SetStaging{}
	case TypeShutdown:
		msg = &Shutdown{}
	case TypeHeartbeat:
		msg = &Heartbeat{}
	case TypeCameraStats:
		msg = &CameraStats{}
	case TypeSegmentOpened:
		msg = &SegmentOpened{}
	case TypeSegmentClosed:
		msg = &SegmentClosed{}
	case TypeEvent:
		msg = &Event{}
	case TypeDestinations:
		msg = &Destinations{}
	case TypeCopyJob:
		msg = &CopyJob{}
	case TypeCancelCopy:
		msg = &CancelCopy{}
	case TypeCopyResult:
		msg = &CopyResult{}
	case TypeDiskStatus:
		msg = &DiskStatus{}
	case TypeRetentionJob:
		msg = &RetentionJob{}
	case TypeRetentionResult:
		msg = &RetentionResult{}
	case TypeRestream:
		msg = &Restream{}
	case TypeRestreamReady:
		msg = &RestreamReady{}
	case TypeRestreamSession:
		msg = &RestreamSession{}
	case TypeAssignMedia:
		msg = &AssignMedia{}
	case TypeSourceChanged:
		msg = &SourceChanged{}
	case TypeMediaReady:
		msg = &MediaReady{}
	case TypeMediaStats:
		msg = &MediaStats{}
	default:
		return nil, nil
	}
	if err := json.Unmarshal(line, msg); err != nil {
		return nil, fmt.Errorf("ipc: %w (%s): %v", ErrMalformed, h.Type, err)
	}
	return msg, nil
}

// Writer writes messages as JSON lines. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Send writes one message.
func (w *Writer) Send(msg any) error {
	b, err := Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(b)
	return err
}

// MaxLine is the largest accepted message. An assign message for a worker
// holds at most a handful of cameras; retention jobs are chunked by the sender.
const MaxLine = 8 << 20

// Reader reads JSON-line messages.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader returns a Reader on r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxLine)
	return &Reader{sc: sc}
}

// Next returns the next message. It skips blank lines and unknown message
// types, returns a non-nil error for a malformed line (the caller may keep
// reading), and io.EOF when the stream ends.
func (r *Reader) Next() (any, error) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, err := Unmarshal(line)
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue
		}
		return msg, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
