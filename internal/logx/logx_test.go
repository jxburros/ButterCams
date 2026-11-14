package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesRotateDailyAndRedact(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 23, 59, 0, 0, time.Local)
	fs, err := OpenFiles(dir, 14, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs.now = func() time.Time { return now }
	fs.day = "" // force a rotation with the fake clock
	log := New(fs, "guardian")
	log.Info("connecting", "url", "rtsp://admin:hunter2@192.168.1.50:554/stream")
	now = now.Add(2 * time.Minute)
	log.Info("next day")
	fs.Close()

	d1, err := os.ReadFile(filepath.Join(dir, "buttercams-2026-11-14.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(d1), "hunter2") || !strings.Contains(string(d1), "rtsp://admin:***@192.168.1.50") {
		t.Fatalf("credentials not redacted: %s", d1)
	}
	if !strings.Contains(string(d1), `"proc":"guardian"`) {
		t.Fatalf("missing proc attribute: %s", d1)
	}
	d2, err := os.ReadFile(filepath.Join(dir, "buttercams-2026-11-15.log"))
	if err != nil || !strings.Contains(string(d2), "next day") {
		t.Fatalf("no file for the next day: %v %s", err, d2)
	}
}

func TestPruneByAgeAndTotalSize(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	mk := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, make([]byte, size), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	mk("buttercams-2026-10-01.log", 10, 44*24*time.Hour) // too old
	mk("buttercams-2026-11-10.log", 600, 4*24*time.Hour)
	mk("buttercams-2026-11-12.log", 600, 2*24*time.Hour)
	mk("buttercams-2026-11-13.log", 600, 24*time.Hour)
	mk("notes.txt", 5000, 400*24*time.Hour) // not ours: must survive

	fs := &Files{dir: dir, retentionDays: 14, maxTotal: 1300, now: func() time.Time { return now }}
	fs.Prune()

	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	if exists("buttercams-2026-10-01.log") {
		t.Error("file older than the retention period was kept")
	}
	if exists("buttercams-2026-11-10.log") {
		t.Error("oldest file should be removed to get under the size cap")
	}
	if !exists("buttercams-2026-11-12.log") || !exists("buttercams-2026-11-13.log") {
		t.Error("newer files were removed")
	}
	if !exists("notes.txt") {
		t.Error("a file ButterCams did not write was deleted")
	}
}
