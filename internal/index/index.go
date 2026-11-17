// Package index is the SQLite index of recordings, their copies and the event
// log (spec 10.5). Only the guardian opens it; workers and the mover never do.
package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/jxburros/buttercams/internal/ipc"
)

// FileName is the database file inside the data root.
const FileName = "buttercams.db"

// SchemaVersion is stored in meta.schema_version.
const SchemaVersion = 1

// Copy states.
const (
	CopyQueued   = "queued"
	CopyCopying  = "copying"
	CopyVerified = "verified"
	CopyFailed   = "failed"
	CopyDeleted  = "deleted"
)

// TimeFormat is how timestamps are stored: RFC 3339 with milliseconds and the
// local UTC offset.
const TimeFormat = ipc.TimeFormat

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS cameras (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  last_state TEXT,
  last_state_ts TEXT
);

CREATE TABLE IF NOT EXISTS segments (
  id INTEGER PRIMARY KEY,
  camera_id TEXT NOT NULL REFERENCES cameras(id),
  file_name TEXT NOT NULL,
  start_ts TEXT NOT NULL,
  end_ts TEXT,
  bytes INTEGER,
  sha256 TEXT,
  video_codec TEXT,
  audio_codec TEXT,
  gap_before INTEGER NOT NULL DEFAULT 0,
  close_reason TEXT,
  protected INTEGER NOT NULL DEFAULT 0,
  UNIQUE (camera_id, file_name)
);
CREATE INDEX IF NOT EXISTS idx_segments_camera_start ON segments(camera_id, start_ts);

CREATE TABLE IF NOT EXISTS copies (
  segment_id INTEGER NOT NULL REFERENCES segments(id),
  destination_id TEXT NOT NULL,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  updated_ts TEXT NOT NULL,
  PRIMARY KEY (segment_id, destination_id)
);

CREATE INDEX IF NOT EXISTS idx_copies_destination_state ON copies(destination_id, state);

CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  ts TEXT NOT NULL,
  source TEXT NOT NULL,
  type TEXT NOT NULL,
  severity TEXT NOT NULL,
  camera_id TEXT,
  data TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`

// DB is the open index.
type DB struct {
	path string
	w    *sql.DB // single writer connection
	r    *sql.DB // read-only pool
	mu   sync.Mutex
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	return "file:" + path + "?" + q.Encode()
}

// QuickCheck opens the database file and runs PRAGMA quick_check. A missing
// file is not an error.
func QuickCheck(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

// Open opens (creating if needed) the index in WAL mode with foreign keys on.
func Open(path string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("creating the index schema: %w", err)
	}
	if _, err := w.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES('schema_version', ?)`, fmt.Sprint(SchemaVersion)); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	return &DB{path: path, w: w, r: r}, nil
}

// Close closes the index.
func (d *DB) Close() {
	d.r.Close()
	d.w.Close()
}

// Path returns the database file.
func (d *DB) Path() string { return d.path }

func fmtTime(t time.Time) string { return t.Format(TimeFormat) }

func parseTime(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

// CheckWrite proves that the database accepts writes (health check 6).
func (d *DB) CheckWrite() error {
	_, err := d.w.Exec(`INSERT INTO meta(key, value) VALUES('last_health_check', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, fmtTime(time.Now()))
	return err
}

// ------------------------------------------------------------------ cameras

// CameraRow is a row of the cameras table.
type CameraRow struct {
	ID          string
	Name        string
	LastState   string
	LastStateTS time.Time
}

// UpsertCamera records a camera's id and display name.
func (d *DB) UpsertCamera(id, name string) error {
	_, err := d.w.Exec(`INSERT INTO cameras(id, name) VALUES(?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name`, id, name)
	return err
}

// SetCameraState stores the latest reported state of a camera.
func (d *DB) SetCameraState(id, state string, ts time.Time) error {
	_, err := d.w.Exec(`UPDATE cameras SET last_state = ?, last_state_ts = ? WHERE id = ?`, state, fmtTime(ts), id)
	return err
}

// Cameras lists every camera the index knows, including deleted ones whose
// recordings were kept.
func (d *DB) Cameras() ([]CameraRow, error) {
	rows, err := d.r.Query(`SELECT id, name, COALESCE(last_state, ''), last_state_ts FROM cameras ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CameraRow
	for rows.Next() {
		var c CameraRow
		var ts sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.LastState, &ts); err != nil {
			return nil, err
		}
		c.LastStateTS = parseTime(ts)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CamerasWithRecordings returns the cameras that still have at least one
// recording with a copy that is not deleted.
func (d *DB) CamerasWithRecordings() ([]CameraRow, error) {
	rows, err := d.r.Query(`SELECT c.id, c.name, COALESCE(c.last_state, ''), c.last_state_ts FROM cameras c
		WHERE EXISTS (SELECT 1 FROM segments s JOIN copies cp ON cp.segment_id = s.id
		              WHERE s.camera_id = c.id AND cp.state <> 'deleted')
		ORDER BY c.name, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CameraRow
	for rows.Next() {
		var c CameraRow
		var ts sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.LastState, &ts); err != nil {
			return nil, err
		}
		c.LastStateTS = parseTime(ts)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------- segments

// Copy is one row of the copies table.
type Copy struct {
	SegmentID     int64
	DestinationID string
	State         string
	Attempts      int
	LastError     string
	Updated       time.Time
}

// Segment is one row of the segments table with its copies.
type Segment struct {
	ID          int64
	CameraID    string
	FileName    string
	Start       time.Time
	End         time.Time // zero while the file is being written
	Bytes       int64
	SHA256      string
	VideoCodec  string
	AudioCodec  string
	GapBefore   bool
	CloseReason string
	Protected   bool
	Copies      []Copy
}

// Closed reports whether the segment's file has been finished.
func (s Segment) Closed() bool { return !s.End.IsZero() }

func (d *DB) ensureCamera(tx *sql.Tx, id string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO cameras(id, name) VALUES(?, ?)`, id, id)
	return err
}

// ErrDuplicateSegment means a different, already closed file with the same
// name is indexed for the camera.
var ErrDuplicateSegment = errors.New("a closed segment with this name already exists")

// SegmentOpened records a file that a worker just started. Other files of
// the same camera that are still marked open were abandoned (their worker
// crashed); their rows are dropped and come back when the file is recovered.
func (d *DB) SegmentOpened(cameraID, fileName string, start time.Time, gap bool, videoCodec, audioCodec string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.w.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := d.ensureCamera(tx, cameraID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM copies WHERE segment_id IN
		(SELECT id FROM segments WHERE camera_id = ? AND end_ts IS NULL AND file_name <> ?)`, cameraID, fileName); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM segments WHERE camera_id = ? AND end_ts IS NULL AND file_name <> ?`, cameraID, fileName); err != nil {
		return 0, err
	}
	// A closed recording with this name must never be overwritten.
	var closed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM segments WHERE camera_id = ? AND file_name = ? AND end_ts IS NOT NULL`,
		cameraID, fileName).Scan(&closed); err != nil {
		return 0, err
	}
	if closed > 0 {
		return 0, ErrDuplicateSegment
	}
	if _, err := tx.Exec(`INSERT INTO segments(camera_id, file_name, start_ts, gap_before, video_codec, audio_codec)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(camera_id, file_name) DO UPDATE SET start_ts = excluded.start_ts, gap_before = excluded.gap_before,
			video_codec = excluded.video_codec, audio_codec = excluded.audio_codec`,
		cameraID, fileName, fmtTime(start), gap, videoCodec, audioCodec); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM segments WHERE camera_id = ? AND file_name = ?`, cameraID, fileName).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// RecentNames returns the file names of a camera's recordings that started
// at or after since. Workers are given them so that a name is never reused.
func (d *DB) RecentNames(cameraID string, since time.Time) ([]string, error) {
	rows, err := d.r.Query(`SELECT file_name FROM segments WHERE camera_id = ? AND julianday(start_ts) >= julianday(?)`,
		cameraID, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClosedSegment is the information a worker reports when it finishes a file.
type ClosedSegment struct {
	CameraID    string
	FileName    string
	Start       time.Time
	End         time.Time
	Bytes       int64
	SHA256      string
	VideoCodec  string
	AudioCodec  string
	Gap         bool
	CloseReason string
}

// SegmentClosed records a finished file, marks its staging copy as present
// and queues one copy per destination (spec 7.2). It returns the segment id.
func (d *DB) SegmentClosed(c ClosedSegment, destinationIDs []string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.w.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := d.ensureCamera(tx, c.CameraID); err != nil {
		return 0, err
	}
	var id int64
	var end sql.NullString
	err = tx.QueryRow(`SELECT id, end_ts FROM segments WHERE camera_id = ? AND file_name = ?`, c.CameraID, c.FileName).Scan(&id, &end)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.Exec(`INSERT INTO segments(camera_id, file_name, start_ts, end_ts, bytes, sha256, video_codec, audio_codec, gap_before, close_reason)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.CameraID, c.FileName, fmtTime(c.Start), fmtTime(c.End), c.Bytes, nullString(c.SHA256),
			c.VideoCodec, c.AudioCodec, c.Gap, c.CloseReason)
		if err != nil {
			return 0, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	case end.Valid:
		return id, ErrDuplicateSegment
	default:
		if _, err := tx.Exec(`UPDATE segments SET start_ts = ?, end_ts = ?, bytes = ?, sha256 = ?, video_codec = ?,
			audio_codec = ?, gap_before = ?, close_reason = ? WHERE id = ?`,
			fmtTime(c.Start), fmtTime(c.End), c.Bytes, nullString(c.SHA256), c.VideoCodec, c.AudioCodec, c.Gap, c.CloseReason, id); err != nil {
			return 0, err
		}
	}
	now := fmtTime(time.Now())
	if _, err := tx.Exec(`INSERT OR REPLACE INTO copies(segment_id, destination_id, state, updated_ts) VALUES(?, ?, ?, ?)`,
		id, ipc.StagingID, CopyVerified, now); err != nil {
		return 0, err
	}
	for _, dest := range destinationIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO copies(segment_id, destination_id, state, updated_ts) VALUES(?, ?, ?, ?)`,
			id, dest, CopyQueued, now); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const segmentCols = `id, camera_id, file_name, start_ts, end_ts, COALESCE(bytes, 0), COALESCE(sha256, ''),
	COALESCE(video_codec, ''), COALESCE(audio_codec, ''), gap_before, COALESCE(close_reason, ''), protected`

func scanSegment(sc interface{ Scan(...any) error }) (Segment, error) {
	var s Segment
	var start, end sql.NullString
	if err := sc.Scan(&s.ID, &s.CameraID, &s.FileName, &start, &end, &s.Bytes, &s.SHA256,
		&s.VideoCodec, &s.AudioCodec, &s.GapBefore, &s.CloseReason, &s.Protected); err != nil {
		return s, err
	}
	s.Start, s.End = parseTime(start), parseTime(end)
	return s, nil
}

func (d *DB) loadCopies(segs []Segment) error {
	if len(segs) == 0 {
		return nil
	}
	byID := make(map[int64]*Segment, len(segs))
	ids := make([]string, 0, len(segs))
	for i := range segs {
		byID[segs[i].ID] = &segs[i]
		ids = append(ids, fmt.Sprint(segs[i].ID))
	}
	rows, err := d.r.Query(`SELECT segment_id, destination_id, state, attempts, COALESCE(last_error, ''), updated_ts
		FROM copies WHERE segment_id IN (` + strings.Join(ids, ",") + `) ORDER BY destination_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Copy
		var ts sql.NullString
		if err := rows.Scan(&c.SegmentID, &c.DestinationID, &c.State, &c.Attempts, &c.LastError, &ts); err != nil {
			return err
		}
		c.Updated = parseTime(ts)
		if s := byID[c.SegmentID]; s != nil {
			s.Copies = append(s.Copies, c)
		}
	}
	return rows.Err()
}

// Segment returns one segment with its copies.
func (d *DB) Segment(id int64) (Segment, error) {
	s, err := scanSegment(d.r.QueryRow(`SELECT `+segmentCols+` FROM segments WHERE id = ?`, id))
	if err != nil {
		return s, err
	}
	list := []Segment{s}
	if err := d.loadCopies(list); err != nil {
		return s, err
	}
	return list[0], nil
}

func (d *DB) querySegments(query string, args ...any) ([]Segment, error) {
	rows, err := d.r.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, d.loadCopies(out)
}

// ListSegments returns a camera's segments for a local date (YYYY-MM-DD),
// oldest first. The date is the one in the file names, so it is the server's
// local date regardless of the offset in force at the time.
func (d *DB) ListSegments(cameraID, date string) ([]Segment, error) {
	return d.querySegments(`SELECT `+segmentCols+` FROM segments
		WHERE camera_id = ? AND file_name LIKE ? ESCAPE '\'
		ORDER BY julianday(start_ts), id`, cameraID, cameraID+`\_`+date+`\_%`)
}

// TimelineSegments returns a camera's segments that overlap [from, to),
// oldest first, including the one still being recorded (spec 16.5). Copies
// are not loaded: the timeline only draws spans.
func (d *DB) TimelineSegments(cameraID string, from, to time.Time) ([]Segment, error) {
	rows, err := d.r.Query(`SELECT `+segmentCols+` FROM segments
		WHERE camera_id = ? AND julianday(start_ts) < julianday(?)
		AND (end_ts IS NULL OR julianday(end_ts) > julianday(?))
		ORDER BY julianday(start_ts), id`, cameraID, fmtTime(to), fmtTime(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AllSegments returns every segment, oldest first. Used by tests and by the
// rebuild comparison.
func (d *DB) AllSegments() ([]Segment, error) {
	return d.querySegments(`SELECT ` + segmentCols + ` FROM segments ORDER BY camera_id, julianday(start_ts), file_name`)
}

// CameraSegments returns every segment of one camera.
func (d *DB) CameraSegments(cameraID string) ([]Segment, error) {
	return d.querySegments(`SELECT `+segmentCols+` FROM segments WHERE camera_id = ? ORDER BY julianday(start_ts), id`, cameraID)
}

// RenameSegmentFile changes the stored file name of a segment.
func (d *DB) RenameSegmentFile(id int64, fileName string) error {
	_, err := d.w.Exec(`UPDATE segments SET file_name = ? WHERE id = ?`, fileName, id)
	return err
}

// NameTaken reports whether the camera already has a segment with this name.
func (d *DB) NameTaken(cameraID, fileName string) (bool, error) {
	var n int
	err := d.r.QueryRow(`SELECT COUNT(*) FROM segments WHERE camera_id = ? AND file_name = ?`, cameraID, fileName).Scan(&n)
	return n > 0, err
}

// ------------------------------------------------------------------- copies

// SetCopyState updates one copy. attempt, when true, increments the attempt
// counter.
func (d *DB) SetCopyState(segmentID int64, destinationID, state, lastError string, attempt bool) error {
	inc := 0
	if attempt {
		inc = 1
	}
	_, err := d.w.Exec(`UPDATE copies SET state = ?, last_error = ?, attempts = attempts + ?, updated_ts = ?
		WHERE segment_id = ? AND destination_id = ?`,
		state, nullString(lastError), inc, fmtTime(time.Now()), segmentID, destinationID)
	return err
}

// AddCopy inserts a copy row if it does not exist yet.
func (d *DB) AddCopy(segmentID int64, destinationID, state string) error {
	_, err := d.w.Exec(`INSERT OR IGNORE INTO copies(segment_id, destination_id, state, updated_ts) VALUES(?, ?, ?, ?)`,
		segmentID, destinationID, state, fmtTime(time.Now()))
	return err
}

// PendingCopy is a copy that still has to be made.
type PendingCopy struct {
	Segment       Segment
	DestinationID string
	State         string
	Attempts      int
}

// PendingCopies returns every copy that is queued, was being copied, or
// failed, for segments whose staging file still exists. The guardian hands
// them to the mover when it (re)starts, so the queue survives restarts.
func (d *DB) PendingCopies() ([]PendingCopy, error) {
	rows, err := d.r.Query(`SELECT c.destination_id, c.state, c.attempts, ` + prefixCols("s") + `
		FROM copies c JOIN segments s ON s.id = c.segment_id
		WHERE c.state IN ('queued', 'copying', 'failed') AND c.destination_id <> 'staging' AND s.end_ts IS NOT NULL
		  AND EXISTS (SELECT 1 FROM copies st WHERE st.segment_id = s.id AND st.destination_id = 'staging' AND st.state = 'verified')
		ORDER BY julianday(s.start_ts), s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingCopy
	for rows.Next() {
		var p PendingCopy
		var start, end sql.NullString
		s := &p.Segment
		if err := rows.Scan(&p.DestinationID, &p.State, &p.Attempts, &s.ID, &s.CameraID, &s.FileName, &start, &end,
			&s.Bytes, &s.SHA256, &s.VideoCodec, &s.AudioCodec, &s.GapBefore, &s.CloseReason, &s.Protected); err != nil {
			return nil, err
		}
		s.Start, s.End = parseTime(start), parseTime(end)
		out = append(out, p)
	}
	return out, rows.Err()
}

func prefixCols(alias string) string {
	return alias + `.id, ` + alias + `.camera_id, ` + alias + `.file_name, ` + alias + `.start_ts, ` + alias + `.end_ts,
		COALESCE(` + alias + `.bytes, 0), COALESCE(` + alias + `.sha256, ''), COALESCE(` + alias + `.video_codec, ''),
		COALESCE(` + alias + `.audio_codec, ''), ` + alias + `.gap_before, COALESCE(` + alias + `.close_reason, ''), ` + alias + `.protected`
}

// StoredFile is an indexed file present on a destination (or in staging).
type StoredFile struct {
	SegmentID int64
	CameraID  string
	FileName  string
	Start     time.Time
	Bytes     int64
	Protected bool
}

// StoredFiles returns the closed, verified files on a destination, oldest
// first. These are the only files retention may ever delete (spec rule 7).
func (d *DB) StoredFiles(destinationID string) ([]StoredFile, error) {
	rows, err := d.r.Query(`SELECT s.id, s.camera_id, s.file_name, s.start_ts, COALESCE(s.bytes, 0), s.protected
		FROM copies c JOIN segments s ON s.id = c.segment_id
		WHERE c.destination_id = ? AND c.state = 'verified' AND s.end_ts IS NOT NULL
		ORDER BY julianday(s.start_ts), s.id`, destinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredFile
	for rows.Next() {
		var f StoredFile
		var start sql.NullString
		if err := rows.Scan(&f.SegmentID, &f.CameraID, &f.FileName, &start, &f.Bytes, &f.Protected); err != nil {
			return nil, err
		}
		f.Start = parseTime(start)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Usage returns the bytes and number of verified files on a destination.
func (d *DB) Usage(destinationID string) (bytes int64, files int, err error) {
	err = d.r.QueryRow(`SELECT COALESCE(SUM(s.bytes), 0), COUNT(*) FROM copies c JOIN segments s ON s.id = c.segment_id
		WHERE c.destination_id = ? AND c.state = 'verified' AND s.end_ts IS NOT NULL`, destinationID).Scan(&bytes, &files)
	return
}

// Queue returns how many copies are waiting for a destination and when the
// oldest one was queued.
func (d *DB) Queue(destinationID string) (n int, oldest time.Time, err error) {
	var ts sql.NullString
	err = d.r.QueryRow(`SELECT COUNT(*), MIN(s.end_ts) FROM copies c JOIN segments s ON s.id = c.segment_id
		WHERE c.destination_id = ? AND c.state IN ('queued', 'copying', 'failed')
		  AND EXISTS (SELECT 1 FROM copies st WHERE st.segment_id = s.id AND st.destination_id = 'staging' AND st.state = 'verified')`,
		destinationID).Scan(&n, &ts)
	return n, parseTime(ts), err
}

// CopiedEverywhere returns staging files whose every destination copy is
// verified, so the staging copy can go (spec 7.2 step 3). Segments without
// any destination stay in staging.
func (d *DB) CopiedEverywhere() ([]StoredFile, error) {
	rows, err := d.r.Query(`SELECT s.id, s.camera_id, s.file_name, s.start_ts, COALESCE(s.bytes, 0), s.protected
		FROM segments s JOIN copies st ON st.segment_id = s.id AND st.destination_id = 'staging' AND st.state = 'verified'
		WHERE s.end_ts IS NOT NULL
		  AND EXISTS (SELECT 1 FROM copies c WHERE c.segment_id = s.id AND c.destination_id <> 'staging')
		  AND NOT EXISTS (SELECT 1 FROM copies c WHERE c.segment_id = s.id AND c.destination_id <> 'staging' AND c.state <> 'verified')
		ORDER BY julianday(s.start_ts), s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredFile
	for rows.Next() {
		var f StoredFile
		var start sql.NullString
		if err := rows.Scan(&f.SegmentID, &f.CameraID, &f.FileName, &start, &f.Bytes, &f.Protected); err != nil {
			return nil, err
		}
		f.Start = parseTime(start)
		out = append(out, f)
	}
	return out, rows.Err()
}

// DropDestination forgets every copy on a destination that was removed from
// the configuration. The files themselves are left alone.
func (d *DB) DropDestination(destinationID string) error {
	_, err := d.w.Exec(`DELETE FROM copies WHERE destination_id = ?`, destinationID)
	return err
}

// CancelPending removes copies that are still waiting for a destination, for
// example because it was disabled.
func (d *DB) CancelPending(destinationID string) error {
	_, err := d.w.Exec(`DELETE FROM copies WHERE destination_id = ? AND state IN ('queued', 'copying', 'failed')`, destinationID)
	return err
}

// PruneGone deletes segments that no longer have any copy anywhere and were
// last touched before the cutoff. It keeps the database from growing forever.
func (d *DB) PruneGone(before time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.w.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	const gone = `SELECT s.id FROM segments s WHERE s.end_ts IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM copies c WHERE c.segment_id = s.id AND c.state <> 'deleted')
		AND NOT EXISTS (SELECT 1 FROM copies c WHERE c.segment_id = s.id AND julianday(c.updated_ts) >= julianday(?))`
	if _, err := tx.Exec(`DELETE FROM copies WHERE segment_id IN (`+gone+`)`, fmtTime(before)); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM segments WHERE end_ts IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM copies c WHERE c.segment_id = segments.id)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// ------------------------------------------------------------------- events

// Event is one row of the events table.
type Event struct {
	ID       int64
	TS       time.Time
	Source   string
	Type     string
	Severity string
	CameraID string
	Data     map[string]any
}

// InsertEvent appends an event and returns its id.
func (d *DB) InsertEvent(e Event) (int64, error) {
	var data any
	if len(e.Data) > 0 {
		b, err := json.Marshal(e.Data)
		if err != nil {
			return 0, err
		}
		data = string(b)
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	res, err := d.w.Exec(`INSERT INTO events(ts, source, type, severity, camera_id, data) VALUES(?, ?, ?, ?, ?, ?)`,
		fmtTime(e.TS), e.Source, e.Type, e.Severity, nullString(e.CameraID), data)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EventFilter selects events.
type EventFilter struct {
	Since    time.Time
	Severity string
	CameraID string
	Before   int64 // only events with a smaller id; 0 = newest
	Limit    int
	// Until, when set, keeps only events before it.
	Until time.Time
	// OnlyCameras, when not nil, keeps only events that belong to one of
	// these cameras; events without a camera are left out (spec 16.6).
	OnlyCameras []string
	// Oldest returns the events oldest first instead of newest first.
	Oldest bool
}

// ListEvents returns events newest first.
func (d *DB) ListEvents(f EventFilter) ([]Event, error) {
	where := []string{"1=1"}
	var args []any
	if !f.Since.IsZero() {
		where = append(where, "julianday(ts) >= julianday(?)")
		args = append(args, fmtTime(f.Since))
	}
	if f.Severity != "" {
		where = append(where, "severity = ?")
		args = append(args, f.Severity)
	}
	if f.CameraID != "" {
		where = append(where, "camera_id = ?")
		args = append(args, f.CameraID)
	}
	if f.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	if !f.Until.IsZero() {
		where = append(where, "julianday(ts) < julianday(?)")
		args = append(args, fmtTime(f.Until))
	}
	if f.OnlyCameras != nil {
		if len(f.OnlyCameras) == 0 {
			return nil, nil
		}
		marks := make([]string, len(f.OnlyCameras))
		for i, id := range f.OnlyCameras {
			marks[i] = "?"
			args = append(args, id)
		}
		where = append(where, "camera_id IN ("+strings.Join(marks, ",")+")")
	}
	order := "DESC"
	if f.Oldest {
		order = "ASC"
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	args = append(args, limit)
	rows, err := d.r.Query(`SELECT id, ts, source, type, severity, COALESCE(camera_id, ''), COALESCE(data, '')
		FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY id `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts sql.NullString
		var data string
		if err := rows.Scan(&e.ID, &ts, &e.Source, &e.Type, &e.Severity, &e.CameraID, &data); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		if data != "" {
			json.Unmarshal([]byte(data), &e.Data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventRetention is how long events are kept (spec 10.5).
const EventRetention = 90 * 24 * time.Hour

// PruneEvents deletes events older than the cutoff.
func (d *DB) PruneEvents(before time.Time) (int64, error) {
	res, err := d.w.Exec(`DELETE FROM events WHERE julianday(ts) < julianday(?)`, fmtTime(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Checkpoint folds the write-ahead log back into the database file.
func (d *DB) Checkpoint(ctx context.Context) error {
	_, err := d.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Meta returns a value from the meta table.
func (d *DB) Meta(key string) (string, bool) {
	var v string
	if err := d.r.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// SetMeta stores a value in the meta table.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.w.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// FailPendingCopies marks every copy of a segment that is still waiting as
// failed with the given reason, because its staging file is gone.
func (d *DB) FailPendingCopies(segmentID int64, reason string) error {
	_, err := d.w.Exec(`UPDATE copies SET state = 'failed', last_error = ?, updated_ts = ?
		WHERE segment_id = ? AND destination_id <> 'staging' AND state IN ('queued', 'copying', 'failed')`,
		reason, fmtTime(time.Now()), segmentID)
	return err
}
