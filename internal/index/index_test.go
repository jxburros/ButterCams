package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

var loc = time.FixedZone("", -6*3600)

func closedAt(cam string, hour int, suffix string) ClosedSegment {
	start := time.Date(2026, 11, 14, hour, 0, 0, 212_000_000, loc)
	return ClosedSegment{
		CameraID: cam, FileName: cam + "_2026-11-14_" + start.Format("15") + "00_-0600" + suffix + ".mp4",
		Start: start, End: start.Add(time.Hour - time.Second), Bytes: 1000, SHA256: "abc",
		VideoCodec: "h264", AudioCodec: "aac", CloseReason: ipc.ReasonHourRollover,
	}
}

func TestSchemaAndPragmas(t *testing.T) {
	db := openTest(t)
	var mode string
	db.w.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode = %s, want wal", mode)
	}
	var fk int
	db.w.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign_keys is off")
	}
	var v string
	db.r.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&v)
	if v != "1" {
		t.Fatalf("schema_version = %q", v)
	}
	if err := db.CheckWrite(); err != nil {
		t.Fatal(err)
	}
	if err := QuickCheck(db.Path()); err != nil {
		t.Fatal(err)
	}
	// A foreign key violation must be rejected.
	if _, err := db.w.Exec(`INSERT INTO segments(camera_id, file_name, start_ts) VALUES('ghost', 'x', 'y')`); err == nil {
		t.Fatal("segment for an unknown camera was accepted")
	}
}

func TestQuickCheckDetectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := QuickCheck(path); err != nil {
		t.Fatalf("a missing database is not corrupt: %v", err)
	}
	os.WriteFile(path, []byte("this is not a database, just some text that is long enough"), 0o600)
	if err := QuickCheck(path); err == nil {
		t.Fatal("garbage passed quick_check")
	}
}

func TestSegmentLifecycle(t *testing.T) {
	db := openTest(t)
	c := closedAt("front-door", 9, "")
	openID, err := db.SegmentOpened(c.CameraID, c.FileName, c.Start, false, "h264", "aac")
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.Segment(openID)
	if err != nil || s.Closed() || len(s.Copies) != 0 {
		t.Fatalf("open segment: %+v %v", s, err)
	}

	id, err := db.SegmentClosed(c, []string{"nas-1", "local-d"})
	if err != nil || id != openID {
		t.Fatalf("close: id=%d open=%d err=%v", id, openID, err)
	}
	s, _ = db.Segment(id)
	if !s.Closed() || !s.Start.Equal(c.Start) || !s.End.Equal(c.End) || s.Bytes != 1000 || s.SHA256 != "abc" ||
		s.CloseReason != ipc.ReasonHourRollover || s.GapBefore || s.Protected {
		t.Fatalf("closed segment: %+v", s)
	}
	states := map[string]string{}
	for _, cp := range s.Copies {
		states[cp.DestinationID] = cp.State
	}
	if states["staging"] != CopyVerified || states["nas-1"] != CopyQueued || states["local-d"] != CopyQueued || len(states) != 3 {
		t.Fatalf("copies: %v", states)
	}

	// Reporting the same file again is refused, not silently overwritten.
	if _, err := db.SegmentClosed(c, nil); !errors.Is(err, ErrDuplicateSegment) {
		t.Fatalf("duplicate close: %v", err)
	}

	pending, _ := db.PendingCopies()
	if len(pending) != 2 || pending[0].Segment.ID != id {
		t.Fatalf("pending: %+v", pending)
	}
	if list, _ := db.CopiedEverywhere(); len(list) != 0 {
		t.Fatal("staging copy offered for deletion before the copies are verified")
	}
	db.SetCopyState(id, "nas-1", CopyVerified, "", true)
	db.SetCopyState(id, "local-d", CopyFailed, "share offline", true)
	if list, _ := db.CopiedEverywhere(); len(list) != 0 {
		t.Fatal("staging copy offered for deletion while one destination has not verified")
	}
	if n, oldest, _ := db.Queue("local-d"); n != 1 || !oldest.Equal(c.End) {
		t.Fatalf("queue: %d %v", n, oldest)
	}
	db.SetCopyState(id, "local-d", CopyVerified, "", true)
	list, _ := db.CopiedEverywhere()
	if len(list) != 1 || list[0].SegmentID != id {
		t.Fatalf("CopiedEverywhere: %+v", list)
	}
	if b, n, _ := db.Usage("nas-1"); b != 1000 || n != 1 {
		t.Fatalf("usage: %d bytes, %d files", b, n)
	}
	if pending, _ := db.PendingCopies(); len(pending) != 0 {
		t.Fatalf("pending after verification: %+v", pending)
	}
}

func TestClosedSegmentIsNeverOverwrittenByAReusedName(t *testing.T) {
	db := openTest(t)
	c := closedAt("cam", 9, "")
	id, err := db.SegmentClosed(c, []string{"nas"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SegmentOpened("cam", c.FileName, c.Start.Add(10*time.Minute), true, "h265", ""); !errors.Is(err, ErrDuplicateSegment) {
		t.Fatalf("opening a file under a closed recording's name: %v", err)
	}
	s, _ := db.Segment(id)
	if !s.Start.Equal(c.Start) || !s.Closed() || s.VideoCodec != "h264" {
		t.Fatalf("the closed recording was changed: %+v", s)
	}
	names, _ := db.RecentNames("cam", c.Start.Add(-time.Hour))
	if len(names) != 1 || names[0] != c.FileName {
		t.Fatalf("RecentNames = %v", names)
	}
	if names, _ := db.RecentNames("cam", c.Start.Add(time.Hour)); len(names) != 0 {
		t.Fatalf("RecentNames after the start = %v", names)
	}
}

func TestCameraWithoutDestinationsStaysInStaging(t *testing.T) {
	db := openTest(t)
	if _, err := db.SegmentClosed(closedAt("lobby", 9, ""), nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.CopiedEverywhere(); len(list) != 0 {
		t.Fatal("a file with no destination must stay in staging")
	}
	files, _ := db.StoredFiles("staging")
	if len(files) != 1 {
		t.Fatalf("staging files: %+v", files)
	}
}

func TestAbandonedOpenRowsAreDropped(t *testing.T) {
	db := openTest(t)
	a, b := closedAt("cam", 9, ""), closedAt("cam", 9, "_b")
	db.SegmentOpened("cam", a.FileName, a.Start, true, "h264", "")
	db.SegmentOpened("cam", b.FileName, b.Start, true, "h264", "") // the worker restarted
	all, _ := db.AllSegments()
	if len(all) != 1 || all[0].FileName != b.FileName {
		t.Fatalf("segments: %+v", all)
	}
	// The abandoned file is recovered later and reported closed.
	a.CloseReason, a.Gap = ipc.ReasonCrashRecovered, true
	if _, err := db.SegmentClosed(a, []string{"nas-1"}); err != nil {
		t.Fatal(err)
	}
	all, _ = db.AllSegments()
	if len(all) != 2 || !all[0].Closed() || all[0].CloseReason != ipc.ReasonCrashRecovered || !all[0].GapBefore {
		t.Fatalf("segments: %+v", all)
	}
}

func TestListSegmentsByLocalDateAcrossFallBack(t *testing.T) {
	db := openTest(t)
	// 01:00 happens twice on the fall-back night: first at -0500, then -0600.
	first := ClosedSegment{CameraID: "cam", FileName: "cam_2026-11-01_0100_-0500.mp4",
		Start: time.Date(2026, 11, 1, 1, 0, 0, 0, time.FixedZone("", -5*3600)), Bytes: 1}
	first.End = first.Start.Add(time.Hour)
	second := ClosedSegment{CameraID: "cam", FileName: "cam_2026-11-01_0100_-0600.mp4",
		Start: time.Date(2026, 11, 1, 1, 0, 0, 0, time.FixedZone("", -6*3600)), Bytes: 1}
	second.End = second.Start.Add(time.Hour)
	other := closedAt("cam", 9, "")
	for _, c := range []ClosedSegment{second, other, first} {
		if _, err := db.SegmentClosed(c, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ListSegments("cam", "2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].FileName != first.FileName || got[1].FileName != second.FileName {
		t.Fatalf("wrong order or content across the repeated hour: %+v", got)
	}
	if got, _ := db.ListSegments("cam", "2026-11-14"); len(got) != 1 {
		t.Fatalf("other day: %+v", got)
	}
	if got, _ := db.ListSegments("ca", "2026-11-01"); len(got) != 0 {
		t.Fatal("a prefix of a camera id matched")
	}
}

func TestStoredFilesOldestFirstAcrossCameras(t *testing.T) {
	db := openTest(t)
	for _, c := range []ClosedSegment{closedAt("b", 11, ""), closedAt("a", 9, ""), closedAt("c", 10, "")} {
		id, _ := db.SegmentClosed(c, []string{"nas"})
		db.SetCopyState(id, "nas", CopyVerified, "", true)
	}
	files, _ := db.StoredFiles("nas")
	if len(files) != 3 || files[0].CameraID != "a" || files[1].CameraID != "c" || files[2].CameraID != "b" {
		t.Fatalf("order: %+v", files)
	}
}

func TestEventsFilterAndPrune(t *testing.T) {
	db := openTest(t)
	now := time.Now()
	old, _ := db.InsertEvent(Event{TS: now.Add(-100 * 24 * time.Hour), Source: "guardian", Type: "status_changed", Severity: ipc.SevInfo})
	db.InsertEvent(Event{TS: now.Add(-time.Hour), Source: "camera:front-door", Type: "stream_stalled", Severity: ipc.SevWarning,
		CameraID: "front-door", Data: map[string]any{"message": "no video"}})
	db.InsertEvent(Event{TS: now, Source: "mover", Type: "staging_floor", Severity: ipc.SevCritical})

	all, _ := db.ListEvents(EventFilter{})
	if len(all) != 3 || all[0].Type != "staging_floor" {
		t.Fatalf("events newest first: %+v", all)
	}
	if got, _ := db.ListEvents(EventFilter{Severity: ipc.SevWarning}); len(got) != 1 || got[0].Data["message"] != "no video" {
		t.Fatalf("severity filter: %+v", got)
	}
	if got, _ := db.ListEvents(EventFilter{CameraID: "front-door"}); len(got) != 1 {
		t.Fatalf("camera filter: %+v", got)
	}
	if got, _ := db.ListEvents(EventFilter{Since: now.Add(-2 * time.Hour)}); len(got) != 2 {
		t.Fatalf("since filter: %+v", got)
	}
	if got, _ := db.ListEvents(EventFilter{Before: all[0].ID, Limit: 1}); len(got) != 1 || got[0].Type != "stream_stalled" {
		t.Fatalf("paging: %+v", got)
	}
	n, err := db.PruneEvents(now.Add(-EventRetention))
	if err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if got, _ := db.ListEvents(EventFilter{}); len(got) != 2 || got[1].ID == old {
		t.Fatalf("after prune: %+v", got)
	}
}

func TestPruneGoneOnlyRemovesFullyDeletedSegments(t *testing.T) {
	db := openTest(t)
	gone, _ := db.SegmentClosed(closedAt("cam", 9, ""), []string{"nas"})
	kept, _ := db.SegmentClosed(closedAt("cam", 10, ""), []string{"nas"})
	db.SetCopyState(gone, "staging", CopyDeleted, "", false)
	db.SetCopyState(gone, "nas", CopyDeleted, "", false)
	db.SetCopyState(kept, "staging", CopyDeleted, "", false)
	db.SetCopyState(kept, "nas", CopyVerified, "", false)
	n, err := db.PruneGone(time.Now().Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	all, _ := db.AllSegments()
	if len(all) != 1 || all[0].ID != kept {
		t.Fatalf("remaining: %+v", all)
	}
	// Recently deleted rows are kept for a while.
	db.SetCopyState(kept, "nas", CopyDeleted, "", false)
	if n, _ := db.PruneGone(time.Now().Add(-time.Hour)); n != 0 {
		t.Fatal("a segment deleted moments ago was pruned")
	}
}
