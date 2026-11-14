package mover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
)

type moverHarness struct {
	t    *testing.T
	enc  *ipc.Writer
	in   *io.PipeWriter
	done chan error
	m    *Mover

	mu   sync.Mutex
	cond *sync.Cond
	msgs []any
}

func startMover(t *testing.T, tune func(*Mover)) *moverHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &moverHarness{t: t, enc: ipc.NewWriter(inW), in: inW, done: make(chan error, 1)}
	h.cond = sync.NewCond(&h.mu)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.m = newMover(outW, log)
	if tune != nil {
		tune(h.m)
	}
	go func() {
		err := h.m.run(context.Background(), inR)
		outW.Close()
		h.done <- err
	}()
	go func() {
		r := ipc.NewReader(outR)
		for {
			msg, err := r.Next()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.msgs = append(h.msgs, msg)
			h.cond.Broadcast()
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		h.enc.Send(&ipc.Shutdown{})
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
			t.Error("mover did not stop")
		}
		inW.Close()
	})
	return h
}

func (h *moverHarness) send(msg any) {
	h.t.Helper()
	if err := h.enc.Send(msg); err != nil {
		h.t.Fatal(err)
	}
}

func (h *moverHarness) wait(timeout time.Duration, what string, pred func(any) bool) any {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() { h.mu.Lock(); h.cond.Broadcast(); h.mu.Unlock() })
	defer timer.Stop()
	h.mu.Lock()
	defer h.mu.Unlock()
	i := 0
	for {
		for ; i < len(h.msgs); i++ {
			if pred(h.msgs[i]) {
				return h.msgs[i]
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		h.cond.Wait()
	}
}

func (h *moverHarness) results() []*ipc.CopyResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*ipc.CopyResult
	for _, m := range h.msgs {
		if r, ok := m.(*ipc.CopyResult); ok {
			out = append(out, r)
		}
	}
	return out
}

var hour = time.Date(2026, 11, 14, 9, 0, 0, 0, time.FixedZone("", -6*3600))

// stagedFile creates a recording-shaped file in staging and returns its job.
func stagedFile(t *testing.T, staging string, id int64, cam string, n int, content string) *ipc.CopyJob {
	t.Helper()
	name := naming.FileName(cam, hour, n)
	rel, _ := naming.RelPath(name)
	path := filepath.Join(staging, rel)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return &ipc.CopyJob{SegmentID: id, CameraID: cam, FileName: name, SourcePath: path,
		Bytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), Start: ipc.T(hour)}
}

func localDest(id, path string) config.Destination {
	return config.Destination{ID: id, Name: id, Type: config.DestLocal, Path: path, CapGB: 1, Enabled: true}
}

func TestCopyVerifyAndSameLayout(t *testing.T) {
	staging, destDir := t.TempDir(), filepath.Join(t.TempDir(), "recordings")
	h := startMover(t, nil)
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("d1", destDir)}})
	job := stagedFile(t, staging, 7, "front-door", 0, strings.Repeat("video", 50000))
	job.DestinationID = "d1"
	h.send(job)

	res := h.wait(10*time.Second, "copy_result", func(m any) bool { _, ok := m.(*ipc.CopyResult); return ok }).(*ipc.CopyResult)
	if !res.OK || res.SHA256 != job.SHA256 || res.Bytes != job.Bytes || res.SegmentID != 7 || res.DestinationID != "d1" {
		t.Fatalf("result %+v", res)
	}
	rel, _ := naming.RelPath(job.FileName)
	got, err := os.ReadFile(filepath.Join(destDir, rel))
	if err != nil {
		t.Fatalf("copy not at the same relative path as in staging: %v", err)
	}
	if len(got) != int(job.Bytes) {
		t.Fatalf("copy is %d bytes", len(got))
	}
	if left, _ := filepath.Glob(filepath.Join(destDir, "*", "*", "*"+PartSuffix)); len(left) != 0 {
		t.Fatalf("temporary copy left behind: %v", left)
	}
	if _, err := os.Stat(job.SourcePath); err != nil {
		t.Fatal("the mover must not delete the staging file on its own")
	}
	// Free space is reported after a verified copy, which is what triggers retention.
	ds := h.wait(5*time.Second, "disk_status for d1", func(m any) bool {
		d, ok := m.(*ipc.DiskStatus)
		return ok && d.DestinationID == "d1" && d.Reachable
	}).(*ipc.DiskStatus)
	if ds.TotalBytes == 0 || ds.FreeBytes == 0 {
		t.Fatalf("disk status %+v", ds)
	}
	h.wait(5*time.Second, "disk_status for staging", func(m any) bool {
		d, ok := m.(*ipc.DiskStatus)
		return ok && d.DestinationID == ipc.StagingID && d.Reachable
	})
}

func TestCopyRejectsCorruptedStagingFile(t *testing.T) {
	staging, destDir := t.TempDir(), t.TempDir()
	h := startMover(t, func(m *Mover) { m.retry = func(int) time.Duration { return time.Hour } })
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("d1", destDir)}})
	job := stagedFile(t, staging, 1, "cam", 0, "original content")
	job.DestinationID = "d1"
	os.WriteFile(job.SourcePath, []byte("tampered content"), 0o644) // same length, different bytes
	h.send(job)
	res := h.wait(10*time.Second, "copy_result", func(m any) bool { _, ok := m.(*ipc.CopyResult); return ok }).(*ipc.CopyResult)
	if res.OK || !strings.Contains(res.Error, "SHA-256") {
		t.Fatalf("a corrupted staging file was accepted: %+v", res)
	}
	rel, _ := naming.RelPath(job.FileName)
	if _, err := os.Stat(filepath.Join(destDir, rel)); !os.IsNotExist(err) {
		t.Fatal("an unverified copy was given its final name")
	}
}

func TestMissingStagingFileIsReportedOnce(t *testing.T) {
	staging, destDir := t.TempDir(), t.TempDir()
	h := startMover(t, nil)
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("d1", destDir)}})
	job := stagedFile(t, staging, 1, "cam", 0, "x")
	job.DestinationID = "d1"
	os.Remove(job.SourcePath)
	h.send(job)
	res := h.wait(10*time.Second, "copy_result", func(m any) bool { _, ok := m.(*ipc.CopyResult); return ok }).(*ipc.CopyResult)
	if res.OK || !res.SourceMissing {
		t.Fatalf("result %+v", res)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := len(h.results()); n != 1 {
		t.Fatalf("%d results: a copy that can never succeed must not be retried", n)
	}
}

// A destination that is offline keeps its queue and is retried on a schedule;
// when it returns, the backlog copies (spec 7.3).
func TestOfflineDestinationRetriesThenCopiesBacklog(t *testing.T) {
	staging := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "share")
	os.WriteFile(blocker, []byte("a file where the folder should be"), 0o644) // makes MkdirAll fail
	destDir := filepath.Join(blocker, "recordings")

	var mu sync.Mutex
	var asked []int
	h := startMover(t, func(m *Mover) {
		m.retry = func(n int) time.Duration {
			mu.Lock()
			asked = append(asked, n)
			mu.Unlock()
			return 300 * time.Millisecond
		}
	})
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("nas", destDir)}})
	for i := 0; i < 3; i++ {
		job := stagedFile(t, staging, int64(i+1), "cam", i, strings.Repeat("x", 1000+i))
		job.DestinationID = "nas"
		h.send(job)
	}
	first := h.wait(10*time.Second, "failed copy", func(m any) bool { r, ok := m.(*ipc.CopyResult); return ok && !r.OK }).(*ipc.CopyResult)
	if first.Error == "" || first.Attempt != 1 || first.NextRetrySec < 0 {
		t.Fatalf("failure report %+v", first)
	}
	time.Sleep(1200 * time.Millisecond) // a few retries while "offline"
	for _, r := range h.results() {
		if r.OK {
			t.Fatal("a copy succeeded while the destination was unreachable")
		}
	}
	// While failing, only one probe copy runs at a time.
	h.m.mu.Lock()
	active := len(h.m.dests["nas"].active)
	queued := len(h.m.dests["nas"].queue)
	h.m.mu.Unlock()
	if active > 1 || active+queued != 3 {
		t.Fatalf("active=%d queued=%d; want the whole backlog kept and at most one attempt in flight", active, queued)
	}

	// The share comes back.
	os.Remove(blocker)
	ok := map[int64]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for len(ok) < 3 && time.Now().Before(deadline) {
		for _, r := range h.results() {
			if r.OK {
				ok[r.SegmentID] = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(ok) != 3 {
		t.Fatalf("only %d of 3 backlog copies completed after the destination returned", len(ok))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 || asked[0] != 1 || asked[1] != 2 {
		t.Fatalf("retry schedule asked for failures %v; want 1, 2, 3 …", asked)
	}
}

func TestRetrySchedule(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := RetryDelay(i + 1); got != w {
			t.Fatalf("delay after failure %d = %v, want %v", i+1, got, w)
		}
	}
}

// Retention deletes exactly the files it is told to and nothing else.
func TestDeletesOnlyIndexedFiles(t *testing.T) {
	staging, destDir := t.TempDir(), t.TempDir()
	h := startMover(t, nil)
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("d1", destDir)}})

	a := stagedFile(t, destDir, 1, "cam", 0, "aaaa")
	b := stagedFile(t, destDir, 2, "cam", 1, "bbbb")
	foreign := filepath.Join(filepath.Dir(a.SourcePath), "family-video.mp4")
	os.WriteFile(foreign, []byte("not ours"), 0o644)
	gone := naming.FileName("cam", hour, 5)

	h.send(&ipc.RetentionJob{DestinationID: "d1", Deletes: []ipc.DeleteItem{
		{SegmentID: 1, CameraID: "cam", FileName: a.FileName, Bytes: 4, Reason: ipc.DeleteCap},
		{SegmentID: 9, CameraID: "cam", FileName: gone, Bytes: 4, Reason: ipc.DeleteCap},
		{SegmentID: 10, CameraID: "cam", FileName: "family-video.mp4", Bytes: 8, Reason: ipc.DeleteCap},
		{SegmentID: 11, CameraID: "cam", FileName: "../../../etc/passwd", Bytes: 1, Reason: ipc.DeleteCap},
	}})
	res := h.wait(10*time.Second, "retention_result", func(m any) bool { _, ok := m.(*ipc.RetentionResult); return ok }).(*ipc.RetentionResult)
	if len(res.Results) != 4 || res.DestinationID != "d1" {
		t.Fatalf("result %+v", res)
	}
	if r := res.Results[0]; !r.Deleted || r.Missing || r.Error != "" || r.Path != a.SourcePath {
		t.Fatalf("delete of an indexed file: %+v", r)
	}
	if r := res.Results[1]; r.Deleted || !r.Missing {
		t.Fatalf("a file that is already gone must be reported missing: %+v", r)
	}
	for _, r := range res.Results[2:] {
		if r.Deleted || r.Missing || r.Error == "" {
			t.Fatalf("a name outside the ButterCams pattern was acted on: %+v", r)
		}
	}
	if _, err := os.Stat(a.SourcePath); !os.IsNotExist(err) {
		t.Fatal("selected file still exists")
	}
	if _, err := os.Stat(b.SourcePath); err != nil {
		t.Fatal("a file that was not selected was deleted")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("a file ButterCams did not write was deleted")
	}

	// Removing the last recording of a day removes the empty folders, but a
	// folder that still holds someone else's file stays.
	h.send(&ipc.RetentionJob{DestinationID: "d1", Deletes: []ipc.DeleteItem{{SegmentID: 2, CameraID: "cam", FileName: b.FileName, Bytes: 4, Reason: ipc.DeleteMaxAge}}})
	h.wait(10*time.Second, "second retention_result", func(m any) bool {
		r, ok := m.(*ipc.RetentionResult)
		return ok && len(r.Results) == 1 && r.Results[0].SegmentID == 2
	})
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("the folder with a foreign file was removed")
	}
}

func TestStagingDeletesLookInOldStagingFolders(t *testing.T) {
	oldStaging, newStaging := t.TempDir(), t.TempDir()
	h := startMover(t, nil)
	h.send(&ipc.Destinations{Staging: config.Staging{Path: newStaging}, OldStagingPaths: []string{oldStaging}})
	job := stagedFile(t, oldStaging, 1, "cam", 0, "old")
	h.send(&ipc.RetentionJob{DestinationID: ipc.StagingID, Deletes: []ipc.DeleteItem{
		{SegmentID: 1, CameraID: "cam", FileName: job.FileName, Bytes: 3, Reason: ipc.DeleteCopied}}})
	res := h.wait(10*time.Second, "retention_result", func(m any) bool { _, ok := m.(*ipc.RetentionResult); return ok }).(*ipc.RetentionResult)
	if !res.Results[0].Deleted {
		t.Fatalf("file in the previous staging folder not deleted: %+v", res.Results[0])
	}
	if _, err := os.Stat(filepath.Join(oldStaging, "cam")); !os.IsNotExist(err) {
		t.Fatal("empty camera folder left behind")
	}
}

func TestCancelCopyDropsQueuedJobs(t *testing.T) {
	staging := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "share")
	os.WriteFile(blocker, []byte("x"), 0o644)
	h := startMover(t, func(m *Mover) { m.retry = func(int) time.Duration { return time.Hour } })
	h.send(&ipc.Destinations{Staging: config.Staging{Path: staging}, Destinations: []config.Destination{localDest("nas", filepath.Join(blocker, "r"))}})
	for i := 0; i < 3; i++ {
		job := stagedFile(t, staging, int64(i+1), "cam", i, "x")
		job.DestinationID = "nas"
		h.send(job)
	}
	h.wait(10*time.Second, "failed copy", func(m any) bool { r, ok := m.(*ipc.CopyResult); return ok && !r.OK })
	h.send(&ipc.CancelCopy{DestinationID: "nas"})
	h.send(&ipc.Heartbeat{}) // any later message proves the cancel was processed
	time.Sleep(300 * time.Millisecond)
	h.m.mu.Lock()
	n := len(h.m.dests["nas"].queue)
	h.m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d jobs still queued after cancel", n)
	}
}

func TestDestinationTest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "folder")
	res, err := TestDestination(localDest("d", dir), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalBytes == 0 || res.FreeBytes == 0 || res.FreeBytes > res.TotalBytes {
		t.Fatalf("space %+v", res)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("test file left behind: %v", entries)
	}
	// A path that cannot be created reports the operating system's error.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	if _, err := TestDestination(localDest("d", filepath.Join(blocker, "sub")), ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHeartbeat(t *testing.T) {
	h := startMover(t, nil)
	hb := h.wait(5*time.Second, "heartbeat", func(m any) bool { _, ok := m.(*ipc.Heartbeat); return ok }).(*ipc.Heartbeat)
	if hb.MemBytes == 0 {
		t.Fatalf("heartbeat without memory usage: %+v", hb)
	}
}
