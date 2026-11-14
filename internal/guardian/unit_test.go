package guardian

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/logx"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// bareGuardian is a guardian with its state but without child processes, for
// testing decisions in isolation.
func bareGuardian(t *testing.T, cams int) *Guardian {
	t.Helper()
	root := t.TempDir()
	g := New(Options{DataRoot: root})
	g.ctx = context.Background()
	g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	logs, err := logx.OpenFiles(filepath.Join(root, "logs"), 14, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.logs = logs
	g.evlog = winsvc.OpenEventLog()
	g.store = config.NewStore(root, config.Env{})
	db, err := index.Open(filepath.Join(root, index.FileName))
	if err != nil {
		t.Fatal(err)
	}
	g.db = db
	t.Cleanup(func() { db.Close(); logs.Close() })

	cfg := config.Default(filepath.Join(root, "staging"))
	cfg.Staging.MinFreePercent, cfg.Staging.MinFreeGB = 0, 0
	cfg.Destinations = []config.Destination{{ID: "nas", Name: "NAS", Type: config.DestLocal, Path: filepath.Join(root, "nas"), CapGB: 1, Enabled: true}}
	for i := 1; i <= cams; i++ {
		cfg.Cameras = append(cfg.Cameras, config.Camera{ID: fmt.Sprintf("cam-%02d", i), Name: fmt.Sprintf("Camera %d", i), Enabled: true,
			Host: "10.0.0.1", Port: 554, Path: "/", Transport: "tcp", RecordingMode: "continuous", DestinationIDs: []string{"nas"}})
	}
	cfg.Normalize()
	if err := g.store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	g.cfg = cfg
	g.health = api.Health{Status: api.StatusHealthy}
	now := time.Now()
	for _, c := range cfg.Cameras {
		g.cams[c.ID] = &camLive{notRecordingSince: now}
		g.db.UpsertCamera(c.ID, c.Name)
	}
	g.dests["nas"] = &destLive{}
	return g
}

// fakeProc makes a worker entry look alive and captures what is sent to it.
func fakeProc(w *workerState) chan any {
	ch := make(chan any, 64)
	w.proc.send = ch
	w.proc.running.Store(true)
	w.proc.lastBeat.Store(time.Now().UnixNano())
	return ch
}

func drain(ch chan any) []any {
	var out []any
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// Six cameras per worker, filled in camera order; a change only touches the
// worker concerned (spec 4.1).
func TestAssignmentSixPerWorkerAndOnlyAffectedWorkerChanges(t *testing.T) {
	g := bareGuardian(t, 14)
	started := g.planAssignments()
	if len(started) != 3 || len(g.workers) != 3 {
		t.Fatalf("%d workers for 14 cameras, want 3", len(g.workers))
	}
	if n1, n2, n3 := len(g.workers[1].cams), len(g.workers[2].cams), len(g.workers[3].cams); n1 != 6 || n2 != 6 || n3 != 2 {
		t.Fatalf("cameras per worker: %d %d %d, want 6 6 2", n1, n2, n3)
	}
	if g.workers[1].cams[0] != "cam-01" || g.workers[2].cams[0] != "cam-07" || g.workers[3].cams[1] != "cam-14" {
		t.Fatal("workers are not filled in camera order")
	}
	chans := map[int]chan any{}
	for id, w := range g.workers {
		chans[id] = fakeProc(w)
	}

	// Removing a camera only tells its own worker.
	if _, err := g.UpdateConfig(func(c *config.Config) error { c.Cameras = append(c.Cameras[:1], c.Cameras[2:]...); return nil }); err != nil {
		t.Fatal(err)
	}
	m1 := drain(chans[1])
	if len(m1) != 1 {
		t.Fatalf("worker 1 got %d messages, want one remove_camera", len(m1))
	}
	if rm, ok := m1[0].(*ipc.RemoveCamera); !ok || rm.CameraID != "cam-02" {
		t.Fatalf("worker 1 got %#v", m1[0])
	}
	if len(drain(chans[2])) != 0 || len(drain(chans[3])) != 0 {
		t.Fatal("other workers were told about a camera that is not theirs")
	}

	// A new camera takes the free slot; nobody else moves.
	if _, err := g.UpdateConfig(func(c *config.Config) error {
		c.Cameras = append(c.Cameras, config.Camera{ID: "new", Name: "New", Enabled: true, Host: "10.0.0.9", Port: 554, Path: "/", Transport: "tcp"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if g.camWorker["new"] != 1 || len(g.workers[1].cams) != 6 {
		t.Fatalf("new camera went to worker %d", g.camWorker["new"])
	}
	m1 = drain(chans[1])
	if up, ok := m1[0].(*ipc.UpdateCamera); len(m1) != 1 || !ok || up.Camera.ID != "new" {
		t.Fatalf("worker 1 got %#v", m1)
	}
	if g.camWorker["cam-07"] != 2 || g.camWorker["cam-13"] != 3 || len(drain(chans[2])) != 0 {
		t.Fatal("existing assignments moved")
	}

	// Editing a camera reaches only its worker.
	if _, err := g.UpdateConfig(func(c *config.Config) error {
		for i := range c.Cameras {
			if c.Cameras[i].ID == "cam-08" {
				c.Cameras[i].Host = "10.0.0.88"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if up, ok := drain(chans[2])[0].(*ipc.UpdateCamera); !ok || up.Camera.Host != "10.0.0.88" {
		t.Fatal("the camera's worker was not told about the change")
	}
	if len(drain(chans[1])) != 0 || len(drain(chans[3])) != 0 {
		t.Fatal("unaffected workers were disturbed")
	}

	// A worker left without cameras goes away.
	if _, err := g.UpdateConfig(func(c *config.Config) error {
		keep := c.Cameras[:0]
		for _, cam := range c.Cameras {
			if cam.ID != "cam-13" && cam.ID != "cam-14" {
				keep = append(keep, cam)
			}
		}
		c.Cameras = keep
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(g.workers) != 2 || g.workers[3] != nil {
		t.Fatalf("%d workers after emptying worker 3", len(g.workers))
	}
}

func reasonsOf(h api.Health, code string) []api.Reason {
	var out []api.Reason
	for _, r := range h.Reasons {
		if r.Code == code {
			out = append(out, r)
		}
	}
	return out
}

func (g *Guardian) setCam(id string, state, msg string, notRecordingFor, packetAge time.Duration) {
	now := time.Now()
	live := &camLive{received: now, workerID: 1, lastState: state}
	live.stats = ipc.CameraStats{CameraID: id, State: state, Message: msg}
	live.stats.TS = ipc.T(now)
	if state == ipc.StateRecording {
		live.stats.LastPacketTS = ipc.T(now.Add(-packetAge))
	} else {
		live.notRecordingSince = now.Add(-notRecordingFor)
	}
	g.mu.Lock()
	g.cams[id] = live
	g.mu.Unlock()
}

// Health checks 1 and 2: packet age of recording cameras, and how long a
// camera may be out of the recording state (spec 10.1).
func TestHealthCameraChecks(t *testing.T) {
	g := bareGuardian(t, 2)
	g.setCam("cam-01", ipc.StateRecording, "", 0, time.Second)
	g.setCam("cam-02", ipc.StateBackoff, "connection refused", 90*time.Second, 0)
	g.runHealthCheck()
	if h := g.Health(); h.Status != api.StatusHealthy {
		t.Fatalf("under two minutes out of recording must not change the status: %+v", h)
	}

	g.setCam("cam-02", ipc.StateBackoff, "connection refused", 3*time.Minute, 0)
	g.runHealthCheck()
	h := g.Health()
	r := reasonsOf(h, "camera_not_recording")
	if h.Status != api.StatusWarning || len(r) != 1 || r[0].CameraID != "cam-02" || r[0].Severity != ipc.SevWarning ||
		!strings.Contains(r[0].Message, "Camera 2") || !strings.Contains(r[0].Message, "connection refused") {
		t.Fatalf("after 3 minutes: %+v", h)
	}

	g.setCam("cam-02", ipc.StateError, "Authentication failed", 20*time.Minute, 0)
	g.runHealthCheck()
	h = g.Health()
	if r := reasonsOf(h, "camera_not_recording"); h.Status != api.StatusCritical || len(r) != 1 || r[0].Severity != ipc.SevCritical ||
		!strings.Contains(r[0].Message, "Authentication failed") {
		t.Fatalf("after 20 minutes: %+v", h)
	}

	g.setCam("cam-02", ipc.StateRecording, "", 0, 45*time.Second)
	g.runHealthCheck()
	h = g.Health()
	if r := reasonsOf(h, "camera_no_packets"); h.Status != api.StatusWarning || len(r) != 1 || r[0].CameraID != "cam-02" {
		t.Fatalf("recording with a 45 s old last packet: %+v", h)
	}

	g.setCam("cam-02", ipc.StateRecording, "", 0, time.Second)
	g.runHealthCheck()
	if h := g.Health(); h.Status != api.StatusHealthy || len(h.Reasons) != 0 {
		t.Fatalf("everything recording: %+v", h)
	}

	// A disabled camera is never a problem.
	g.UpdateConfig(func(c *config.Config) error { c.Cameras[1].Enabled = false; return nil })
	g.setCam("cam-02", ipc.StateDisabled, "", 24*time.Hour, 0)
	g.runHealthCheck()
	if h := g.Health(); h.Status != api.StatusHealthy {
		t.Fatalf("a disabled camera changed the status: %+v", h)
	}

	// A camera whose worker went silent is reported, not ignored.
	g.UpdateConfig(func(c *config.Config) error { c.Cameras[1].Enabled = true; return nil })
	g.mu.Lock()
	g.cams["cam-02"] = &camLive{received: time.Now().Add(-5 * time.Minute), lastState: ipc.StateRecording,
		stats: ipc.CameraStats{State: ipc.StateRecording}}
	g.mu.Unlock()
	g.runHealthCheck()
	if r := reasonsOf(g.Health(), "camera_not_recording"); len(r) != 1 || !strings.Contains(r[0].Message, "not reporting") {
		t.Fatalf("silent recorder: %+v", g.Health())
	}

	// Every change of the overall status is stored as an event.
	events, _ := g.db.ListEvents(index.EventFilter{})
	var changes []string
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "status_changed" {
			changes = append(changes, fmt.Sprintf("%v>%v", events[i].Data["from"], events[i].Data["to"]))
		}
	}
	want := "healthy>warning critical>warning warning>critical warning>healthy healthy>warning"
	got := strings.Join(changes, " ")
	for _, w := range strings.Fields(want) {
		if !strings.Contains(got, w) {
			t.Fatalf("status transitions stored: %s (missing %s)", got, w)
		}
	}
}

// Health check 5: a failing destination is tolerated while its queue is
// younger than two hours (spec 10.1).
func TestHealthDestinationQueueAge(t *testing.T) {
	g := bareGuardian(t, 1)
	g.setCam("cam-01", ipc.StateRecording, "", 0, time.Second)
	queue := func(age time.Duration) {
		start := time.Now().Add(-age - time.Hour)
		name := naming.FileName("cam-01", start, 0)
		id, err := g.db.SegmentClosed(index.ClosedSegment{CameraID: "cam-01", FileName: name, Start: start,
			End: time.Now().Add(-age), Bytes: 10}, []string{"nas"})
		if err != nil {
			t.Fatal(err)
		}
		g.db.SetCopyState(id, "nas", index.CopyFailed, "the network path was not found", true)
	}
	g.mu.Lock()
	g.dests["nas"].lastFailed, g.dests["nas"].lastError = true, "the network path was not found"
	g.mu.Unlock()

	queue(time.Hour)
	g.runHealthCheck()
	if h := g.Health(); h.Status != api.StatusHealthy {
		t.Fatalf("queue 1 h old: %+v", h)
	}
	st, _ := g.Storage()
	if st["nas"].State != "retrying" || st["nas"].QueueLength != 1 || st["nas"].LastError == "" {
		t.Fatalf("destination status: %+v", st["nas"])
	}

	queue(3 * time.Hour)
	g.runHealthCheck()
	h := g.Health()
	r := reasonsOf(h, "destination_failing")
	if h.Status != api.StatusWarning || len(r) != 1 || !strings.Contains(r[0].Message, "NAS") ||
		!strings.Contains(r[0].Message, "the network path was not found") {
		t.Fatalf("queue 3 h old: %+v", h)
	}
}

// The on-disk watchdog (spec 10.2): a recording camera whose file does not
// grow is reconnected; if that does not help its worker is restarted.
func TestWatchdogFileNotGrowing(t *testing.T) {
	g := bareGuardian(t, 1)
	g.planAssignments()
	ch := fakeProc(g.workers[1])
	now := time.Now()
	part := naming.PartName(naming.FileName("cam-01", now, 0))
	dir := filepath.Join(g.cfg.Staging.Path, "cam-01", naming.HourStart(now).Format("2006-01-02"))
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, part)
	os.WriteFile(path, make([]byte, 1000), 0o644)

	report := func() {
		g.setCam("cam-01", ipc.StateRecording, "", 0, time.Second)
		g.mu.Lock()
		g.cams["cam-01"].stats.CurrentFile = part
		g.cams["cam-01"].watchFile, g.cams["cam-01"].watchBytes, g.cams["cam-01"].watchStrikes = prev.file, prev.bytes, prev.strikes
		g.mu.Unlock()
		g.runHealthCheck()
		g.mu.Lock()
		c := g.cams["cam-01"]
		prev.file, prev.bytes, prev.strikes = c.watchFile, c.watchBytes, c.watchStrikes
		g.mu.Unlock()
	}

	report() // first sight of the file: nothing to compare with
	if len(drain(ch)) != 0 {
		t.Fatal("watchdog acted on the first check")
	}
	os.WriteFile(path, make([]byte, 2000), 0o644)
	report() // it grew
	if len(drain(ch)) != 0 {
		t.Fatal("watchdog acted although the file grew")
	}
	report() // it did not grow: reconnect the camera
	msgs := drain(ch)
	if rc, ok := msgs[0].(*ipc.RestartCamera); len(msgs) != 1 || !ok || rc.CameraID != "cam-01" {
		t.Fatalf("expected restart_camera, got %#v", msgs)
	}
	events, _ := g.db.ListEvents(index.EventFilter{CameraID: "cam-01"})
	if len(events) != 1 || events[0].Type != "file_not_growing" || events[0].Severity != ipc.SevWarning {
		t.Fatalf("events: %+v", events)
	}
	report() // still not growing: the worker itself is restarted
	events, _ = g.db.ListEvents(index.EventFilter{CameraID: "cam-01"})
	if len(events) != 2 || events[0].Severity != ipc.SevCritical {
		t.Fatalf("events after the second strike: %+v", events)
	}
	if g.workers[1].proc.killWhy == "" {
		t.Fatal("the worker was not killed after two strikes")
	}
}

var prev struct {
	file    string
	bytes   int64
	strikes int
}

// A child that keeps crashing is restarted forever, with a backoff, and is
// flagged as a crash loop after five restarts in ten minutes (spec 9).
func TestSupervisorCrashLoopBackoff(t *testing.T) {
	var restarts []string
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := newProc(ProcSpec{
		Name: "crasher", Exe: exe,
		Env:          []string{"BUTTERCAMS_TEST_EXIT=1"},
		RestartDelay: 20 * time.Millisecond, MaxRestartDelay: 160 * time.Millisecond,
		OnRestart: func(p *Proc, reason string, loop bool) { restarts = append(restarts, fmt.Sprint(loop)) },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	p.Start()
	time.Sleep(2500 * time.Millisecond)
	p.Stop(time.Second)
	if len(restarts) < 6 {
		t.Fatalf("only %d restarts: the supervisor gave up", len(restarts))
	}
	for i, loop := range restarts {
		want := i >= CrashLoopRestarts-1
		if (loop == "true") != want {
			t.Fatalf("restart %d: crash loop = %s, want %v (%v)", i+1, loop, want, restarts)
		}
	}
	if !p.CrashLoop() {
		t.Fatal("crash loop not flagged")
	}
	// With backoff up to 160 ms, 2.5 s allows far fewer restarts than the
	// base delay of 20 ms would.
	if len(restarts) > 40 {
		t.Fatalf("%d restarts in 2.5 s: no backoff", len(restarts))
	}
}
