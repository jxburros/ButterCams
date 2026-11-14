package guardian

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/test/simulator"
)

// The integration tests run the real thing: a guardian in this process that
// starts real worker and mover processes from a freshly built executable,
// recording from the camera simulator.

var testExe string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "buttercams-test-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testExe = filepath.Join(dir, "buttercams")
	if runtime.GOOS == "windows" {
		testExe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", testExe, "github.com/jxburros/buttercams/cmd/buttercams").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the test executable failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type system struct {
	t      *testing.T
	g      *Guardian
	root   string
	cancel context.CancelFunc
	done   chan error
	sim    *simulator.Server
}

func simCam(sim *simulator.Server, id, path string, dests ...string) config.Camera {
	host, portStr, _ := net.SplitHostPort(sim.Addr())
	port, _ := strconv.Atoi(portStr)
	return config.Camera{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Enabled: true, Host: host, Port: port, Path: path,
		Transport: "tcp", RecordAudio: true, RecordingMode: "continuous", DestinationIDs: dests}
}

// newSystem writes a configuration into a fresh data root. start runs it.
func newSystem(t *testing.T, streams []simulator.Stream, mutate func(cfg *config.Config, sim *simulator.Server, root string)) *system {
	t.Helper()
	sim, err := simulator.Start("127.0.0.1:0", streams)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	root := t.TempDir()
	cfg := config.Default(filepath.Join(root, "staging"))
	cfg.Staging.MinFreePercent, cfg.Staging.MinFreeGB = 0, 0
	if mutate != nil {
		mutate(cfg, sim, root)
	}
	cfg.Normalize()
	if err := config.NewStore(root, config.Env{}).Save(cfg); err != nil {
		t.Fatal(err)
	}
	return &system{t: t, root: root, sim: sim}
}

func (s *system) start() {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan error, 1)
	s.g = New(Options{DataRoot: s.root, Exe: testExe, DisableAPI: true, HealthInterval: time.Hour})
	go func() { s.done <- s.g.Run(ctx) }()
	select {
	case <-s.g.ready:
	case err := <-s.done:
		s.t.Fatalf("guardian did not start: %v", err)
	case <-time.After(30 * time.Second):
		s.t.Fatal("guardian did not start in time")
	}
	s.t.Cleanup(s.stop)
}

func (s *system) stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	s.cancel = nil
	select {
	case <-s.done:
	case <-time.After(40 * time.Second):
		s.t.Error("guardian did not stop")
	}
}

func (s *system) until(timeout time.Duration, what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out after %v waiting for %s\n%s", timeout, what, s.logTail())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *system) logTail() string {
	files, _ := filepath.Glob(filepath.Join(s.root, "logs", "*.log"))
	if len(files) == 0 {
		return ""
	}
	data, _ := os.ReadFile(files[len(files)-1])
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return "--- last log lines ---\n" + strings.Join(lines, "\n")
}

func (s *system) recording(cam string) bool {
	st, ok := s.g.CameraStats()[cam]
	return ok && st.State == ipc.StateRecording && st.CurrentFile != ""
}

func (s *system) waitRecording(cams ...string) {
	s.t.Helper()
	s.until(30*time.Second, "cameras recording", func() bool {
		for _, c := range cams {
			if !s.recording(c) {
				return false
			}
		}
		return true
	})
}

// cut makes a camera close its current file and start a new one.
func (s *system) cut(cam string) {
	s.t.Helper()
	before := s.closedCount(cam)
	if _, err := s.g.UpdateConfig(func(c *config.Config) error {
		for i := range c.Cameras {
			if c.Cameras[i].ID == cam {
				c.Cameras[i].RecordAudio = !c.Cameras[i].RecordAudio
			}
		}
		return nil
	}); err != nil {
		s.t.Fatal(err)
	}
	s.until(30*time.Second, "file of "+cam+" to close", func() bool { return s.closedCount(cam) > before })
	s.waitRecording(cam)
}

func (s *system) segments(cam string) []index.Segment {
	segs, err := s.g.db.CameraSegments(cam)
	if err != nil {
		s.t.Fatal(err)
	}
	return segs
}

func (s *system) closedCount(cam string) int {
	n := 0
	for _, seg := range s.segments(cam) {
		if seg.Closed() {
			n++
		}
	}
	return n
}

func copyState(seg index.Segment, dest string) string {
	for _, c := range seg.Copies {
		if c.DestinationID == dest {
			return c.State
		}
	}
	return ""
}

func (s *system) events(typ string) []index.Event {
	all, _ := s.g.db.ListEvents(index.EventFilter{Limit: 1000})
	var out []index.Event
	for _, e := range all {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func relOf(t *testing.T, name string) string {
	t.Helper()
	rel, ok := naming.RelPath(name)
	if !ok {
		t.Fatalf("%s is not a recording name", name)
	}
	return rel
}

// Copy, verify, then remove the staging copy once every destination has the
// file (spec 7.2); a camera without destinations keeps its files in staging.
func TestCopyVerifyAndStagingCleanup(t *testing.T) {
	t.Parallel()
	var destA, destB string
	s := newSystem(t, []simulator.Stream{{Path: "/a", Audio: "aac"}, {Path: "/b"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		destA, destB = filepath.Join(root, "dest-a"), filepath.Join(root, "dest-b")
		cfg.Destinations = []config.Destination{
			{ID: "dest-a", Name: "A", Type: config.DestLocal, Path: destA, CapGB: 10, Enabled: true},
			{ID: "dest-b", Name: "B", Type: config.DestLocal, Path: destB, CapGB: 10, Enabled: true},
		}
		cfg.Cameras = []config.Camera{simCam(sim, "front", "/a", "dest-a", "dest-b"), simCam(sim, "lobby", "/b")}
	})
	s.start()
	s.waitRecording("front", "lobby")
	time.Sleep(2 * time.Second)
	s.cut("front")
	s.cut("lobby")

	var seg index.Segment
	s.until(30*time.Second, "copies to verify and the staging copy to be removed", func() bool {
		seg = s.segments("front")[0]
		return copyState(seg, "dest-a") == index.CopyVerified && copyState(seg, "dest-b") == index.CopyVerified &&
			copyState(seg, ipc.StagingID) == index.CopyDeleted
	})
	rel := relOf(t, seg.FileName)
	for _, dest := range []string{destA, destB} {
		sum, size, err := mp4meta.HashFile(filepath.Join(dest, rel))
		if err != nil {
			t.Fatalf("copy missing on %s: %v", dest, err)
		}
		if sum != seg.SHA256 || size != seg.Bytes {
			t.Fatalf("copy on %s does not match the index (size %d vs %d)", dest, size, seg.Bytes)
		}
	}
	if exists(filepath.Join(s.root, "staging", rel)) {
		t.Fatal("staging copy still exists after every destination verified")
	}
	if left, _ := filepath.Glob(filepath.Join(destA, "*", "*", "*.part")); len(left) != 0 {
		t.Fatalf("temporary copies left on the destination: %v", left)
	}

	// The camera with no destinations keeps its file in staging.
	time.Sleep(2 * time.Second)
	lobby := s.segments("lobby")[0]
	if !lobby.Closed() || copyState(lobby, ipc.StagingID) != index.CopyVerified || len(lobby.Copies) != 1 {
		t.Fatalf("lobby segment: %+v", lobby)
	}
	if !exists(filepath.Join(s.root, "staging", relOf(t, lobby.FileName))) {
		t.Fatal("a camera without destinations lost its staging file")
	}
	st, _ := s.g.Storage()
	if st["dest-a"].State != "ok" || st["dest-a"].UsedBytes != seg.Bytes || st["dest-a"].QueueLength != 0 || st["dest-a"].FreeBytes == nil {
		t.Fatalf("destination status %+v", st["dest-a"])
	}
}

// At the cap, the oldest files across all cameras are deleted until usage is
// under 95% of it, and only files in the index are ever touched (spec 8).
func TestRetentionAtCap(t *testing.T) {
	t.Parallel()
	var dest string
	const capBytes = 400_000
	s := newSystem(t, []simulator.Stream{{Path: "/a"}, {Path: "/b"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		dest = filepath.Join(root, "dest")
		cfg.Destinations = []config.Destination{{ID: "dest", Name: "D", Type: config.DestLocal, Path: dest,
			CapGB: float64(capBytes) / (1 << 30), Enabled: true}}
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a", "dest"), simCam(sim, "b", "/b", "dest")}
	})
	// A file that is not ButterCams' sits in the destination from the start.
	foreign := filepath.Join(dest, "a", time.Now().Format("2006-01-02"), "wedding.mp4")
	os.MkdirAll(filepath.Dir(foreign), 0o755)
	os.WriteFile(foreign, make([]byte, 600_000), 0o644) // bigger than the whole cap
	s.start()
	s.waitRecording("a", "b")

	// Close files (each roughly 70–100 KB) until well past the cap.
	for i := 0; i < 5; i++ {
		time.Sleep(3 * time.Second)
		s.cut("a")
		s.cut("b")
	}
	deleted := func() (n int, all []index.Segment) {
		all = append(s.segments("a"), s.segments("b")...)
		for _, seg := range all {
			if copyState(seg, "dest") == index.CopyDeleted {
				n++
			}
		}
		return
	}
	s.until(40*time.Second, "retention to delete the oldest files", func() bool { n, _ := deleted(); return n >= 2 })
	time.Sleep(3 * time.Second) // let any further retention settle

	_, all := deleted()
	var used int64
	var newestDeleted, oldestKept time.Time
	for _, seg := range all {
		path := filepath.Join(dest, relOf(t, seg.FileName))
		switch copyState(seg, "dest") {
		case index.CopyDeleted:
			if exists(path) {
				t.Fatalf("%s is marked deleted but still on disk", seg.FileName)
			}
			if seg.Start.After(newestDeleted) {
				newestDeleted = seg.Start
			}
		case index.CopyVerified:
			if !exists(path) {
				t.Fatalf("%s is marked present but missing", seg.FileName)
			}
			used += seg.Bytes
			if oldestKept.IsZero() || seg.Start.Before(oldestKept) {
				oldestKept = seg.Start
			}
		}
	}
	if used > capBytes {
		t.Fatalf("usage %d is over the cap %d", used, capBytes)
	}
	if !newestDeleted.Before(oldestKept) {
		t.Fatalf("a newer file (%v) was deleted while an older one (%v) was kept", newestDeleted, oldestKept)
	}
	if got, _, _ := s.g.db.Usage("dest"); got != used {
		t.Fatalf("index says %d bytes used, files add up to %d", got, used)
	}
	if st, err := os.Stat(foreign); err != nil || st.Size() != 600_000 {
		t.Fatal("a file ButterCams did not write was deleted or changed by retention")
	}
}

// A killed worker is back within two seconds; its cameras resume with a new
// file and the interrupted file is recovered (spec 9).
func TestWorkerCrashRecovery(t *testing.T) {
	t.Parallel()
	s := newSystem(t, []simulator.Stream{{Path: "/a", Audio: "pcmu"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a")}
	})
	s.start()
	s.waitRecording("a")
	time.Sleep(3 * time.Second)

	proc := s.g.workerFor("a")
	proc.mu.Lock()
	pid := proc.cmd.Process.Pid
	cmd := proc.cmd
	proc.mu.Unlock()
	killed := time.Now()
	cmd.Process.Kill()

	s.until(10*time.Second, "worker to be restarted", func() bool {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.cmd != nil && proc.cmd.Process.Pid != pid
	})
	if d := time.Since(killed); d > 2500*time.Millisecond {
		t.Fatalf("worker restarted after %v, want within 2 s", d)
	}
	s.until(20*time.Second, "interrupted file to be recovered and recording to resume", func() bool {
		segs := s.segments("a")
		return len(segs) >= 2 && segs[0].CloseReason == ipc.ReasonCrashRecovered && s.recording("a")
	})
	segs := s.segments("a")
	first, second := segs[0], segs[1]
	if !first.GapBefore || !second.GapBefore {
		t.Fatalf("gap flags: recovered=%v next=%v, want both", first.GapBefore, second.GapBefore)
	}
	if p, _ := naming.Parse(second.FileName); p.HourStart.Equal(naming.HourStart(first.Start)) && p.Suffix != "b" {
		t.Fatalf("file after the crash is %s, want the _b suffix", second.FileName)
	}
	if lost := second.Start.Sub(first.End); lost > 6*time.Second {
		t.Fatalf("%v between the recovered file and the new one", lost)
	}
	path := filepath.Join(s.root, "staging", relOf(t, first.FileName))
	info, err := mp4meta.Read(path)
	if err != nil || info.CloseReason != ipc.ReasonCrashRecovered || info.Dur90k == 0 {
		t.Fatalf("recovered file metadata: %+v %v", info, err)
	}
	if sum, _, _ := mp4meta.HashFile(path); sum != first.SHA256 {
		t.Fatal("recovered file hash does not match the index")
	}
	if ev := s.events("worker_restarted"); len(ev) != 1 || ev[0].Severity != ipc.SevWarning {
		t.Fatalf("worker_restarted events: %+v", ev)
	}
}

// A destination that is unreachable does not affect recording; its files wait
// in staging, the mover can crash and restart, and the backlog is copied when
// the destination returns (spec 7.3, 9).
func TestDestinationOutageAndMoverCrash(t *testing.T) {
	t.Parallel()
	var blocker, dest string
	s := newSystem(t, []simulator.Stream{{Path: "/a"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		blocker = filepath.Join(root, "share")
		dest = filepath.Join(blocker, "recordings")
		cfg.Destinations = []config.Destination{{ID: "nas", Name: "NAS", Type: config.DestLocal, Path: dest, CapGB: 10, Enabled: true}}
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a", "nas")}
	})
	os.WriteFile(blocker, []byte("the share is offline"), 0o644) // a file where the folder should be
	s.start()
	s.waitRecording("a")
	time.Sleep(2 * time.Second)
	s.cut("a")
	s.cut("a")

	s.until(20*time.Second, "copy failure to be recorded", func() bool {
		st, _ := s.g.Storage()
		return st["nas"].State == "retrying" && st["nas"].LastError != "" && st["nas"].QueueLength == 2
	})
	if !s.recording("a") {
		t.Fatal("recording stopped while the destination was unreachable")
	}
	for _, seg := range s.segments("a") {
		if seg.Closed() && !exists(filepath.Join(s.root, "staging", relOf(t, seg.FileName))) {
			t.Fatalf("%s left staging before it was copied", seg.FileName)
		}
	}
	if ev := s.events("destination_unreachable"); len(ev) != 1 {
		t.Fatalf("%d destination_unreachable events, want exactly one", len(ev))
	}

	// The mover dies and comes back; the queue is rebuilt from the database.
	s.g.mu.Lock()
	mover := s.g.mover
	s.g.mu.Unlock()
	mover.mu.Lock()
	pid := mover.cmd.Process.Pid
	mover.cmd.Process.Kill()
	mover.mu.Unlock()
	s.until(10*time.Second, "mover to be restarted", func() bool {
		mover.mu.Lock()
		defer mover.mu.Unlock()
		return mover.cmd != nil && mover.cmd.Process.Pid != pid
	})

	// The share comes back. Nudging the configuration clears the retry wait.
	os.Remove(blocker)
	if _, err := s.g.UpdateConfig(func(c *config.Config) error { c.Destinations[0].Name = "NAS (back)"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.until(30*time.Second, "backlog to be copied", func() bool {
		n := 0
		for _, seg := range s.segments("a") {
			if copyState(seg, "nas") == index.CopyVerified {
				n++
			}
		}
		return n >= 2
	})
	for _, seg := range s.segments("a") {
		if copyState(seg, "nas") == index.CopyVerified && !exists(filepath.Join(dest, relOf(t, seg.FileName))) {
			t.Fatalf("%s is marked copied but is not on the destination", seg.FileName)
		}
	}
	st, _ := s.g.Storage()
	if st["nas"].State != "ok" || st["nas"].LastError != "" {
		t.Fatalf("destination status after recovery: %+v", st["nas"])
	}
	if len(s.events("destination_recovered")) != 1 || len(s.events("mover_restarted")) != 1 {
		t.Fatal("recovery events missing")
	}
}

// When staging drops below its floor the oldest staging files are deleted,
// copied or not, a Critical event names them, and recording carries on
// (spec 8).
func TestStagingFloorEmergency(t *testing.T) {
	t.Parallel()
	s := newSystem(t, []simulator.Stream{{Path: "/a"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a")}
	})
	s.start()
	s.waitRecording("a")
	for i := 0; i < 3; i++ {
		time.Sleep(1500 * time.Millisecond)
		s.cut("a")
	}
	closed := s.segments("a")[:3]
	for _, seg := range closed {
		if !exists(filepath.Join(s.root, "staging", relOf(t, seg.FileName))) {
			t.Fatalf("%s should be in staging", seg.FileName)
		}
	}

	// Demand more free space than the disk has: the floor is now violated.
	if _, err := s.g.UpdateConfig(func(c *config.Config) error { c.Staging.MinFreeGB = 1e7; return nil }); err != nil {
		t.Fatal(err)
	}
	s.until(30*time.Second, "emergency deletion of staging files", func() bool {
		for _, seg := range s.segments("a")[:3] {
			if copyState(seg, ipc.StagingID) != index.CopyDeleted {
				return false
			}
		}
		return true
	})
	for _, seg := range closed {
		if exists(filepath.Join(s.root, "staging", relOf(t, seg.FileName))) {
			t.Fatalf("%s still in staging", seg.FileName)
		}
	}
	ev := s.events("staging_floor")
	if len(ev) == 0 || ev[len(ev)-1].Severity != ipc.SevCritical {
		t.Fatalf("no Critical staging_floor event: %+v", ev)
	}
	msg := fmt.Sprint(ev[len(ev)-1].Data["message"])
	if !strings.Contains(msg, closed[0].FileName) {
		t.Fatalf("the event does not name the deleted files: %s", msg)
	}
	// Recording never stops for lack of space: the open file is untouched.
	if !s.recording("a") {
		t.Fatal("recording stopped")
	}
	cur := s.g.CameraStats()["a"].CurrentFile
	if !exists(filepath.Join(s.root, "staging", "a", time.Now().Format("2006-01-02"), cur)) {
		t.Fatalf("the file being recorded (%s) was deleted", cur)
	}
	s.g.runHealthCheck()
	h := s.g.Health()
	if h.Status != api.StatusCritical {
		t.Fatalf("status %s with staging below its floor: %+v", h.Status, h.Reasons)
	}
}

type segKey struct {
	cam, file, reason, video, audio string
	start, end                      string
	bytes                           int64
	gap                             bool
	places                          string
}

func snapshot(t *testing.T, db *index.DB) map[string]segKey {
	t.Helper()
	all, err := db.AllSegments()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]segKey{}
	for _, s := range all {
		if !s.Closed() {
			continue
		}
		var places []string
		for _, c := range s.Copies {
			if c.State == index.CopyVerified {
				places = append(places, c.DestinationID)
			}
		}
		if len(places) == 0 {
			continue // no file left anywhere: not expected to survive a rebuild
		}
		out[s.CameraID+"/"+s.FileName] = segKey{s.CameraID, s.FileName, s.CloseReason, s.VideoCodec, s.AudioCodec,
			s.Start.UTC().Format(time.RFC3339Nano), s.End.UTC().Format(time.RFC3339Nano), s.Bytes, s.GapBefore, strings.Join(places, ",")}
	}
	return out
}

// Deleting the database and rebuilding gives an identical segment list
// (spec 10.4, 14.3).
func TestIndexRebuildEquality(t *testing.T) {
	t.Parallel()
	s := newSystem(t, []simulator.Stream{{Path: "/a", Audio: "aac"}, {Path: "/b", Video: "h265"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		cfg.Destinations = []config.Destination{{ID: "dest", Name: "D", Type: config.DestLocal, Path: filepath.Join(root, "dest"), CapGB: 10, Enabled: true}}
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a", "dest"), simCam(sim, "b", "/b")}
	})
	s.start()
	s.waitRecording("a", "b")
	for i := 0; i < 2; i++ {
		time.Sleep(1500 * time.Millisecond)
		s.cut("a")
		s.cut("b")
	}
	// A worker crash adds a crash-recovered file to the mix.
	proc := s.g.workerFor("a")
	proc.mu.Lock()
	proc.cmd.Process.Kill()
	proc.mu.Unlock()
	s.until(30*time.Second, "crash recovery", func() bool {
		for _, seg := range s.segments("a") {
			if seg.CloseReason == ipc.ReasonCrashRecovered {
				return true
			}
		}
		return false
	})
	s.until(30*time.Second, "copies to settle", func() bool {
		for _, seg := range s.segments("a") {
			if seg.Closed() && copyState(seg, "dest") != index.CopyVerified {
				return false
			}
		}
		return true
	})
	time.Sleep(2 * time.Second)
	s.stop() // the last files are closed with reason "shutdown"

	dbPath := filepath.Join(s.root, index.FileName)
	db, err := index.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, db)
	db.Close()
	if len(before) < 6 {
		t.Fatalf("only %d segments to compare", len(before))
	}
	reasons := map[string]bool{}
	for _, k := range before {
		reasons[k.reason] = true
	}
	if !reasons[ipc.ReasonShutdown] || !reasons[ipc.ReasonCrashRecovered] {
		t.Fatalf("test data lacks variety: %v", reasons)
	}

	// The database is deleted and rebuilt with the offline command.
	for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		os.Remove(f)
	}
	out, err := exec.Command(testExe, "rebuild-index", "--data-root", s.root).CombinedOutput()
	if err != nil {
		t.Fatalf("rebuild-index failed: %v\n%s", err, out)
	}
	db, err = index.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := snapshot(t, db)
	if len(after) != len(before) {
		t.Fatalf("%d segments after the rebuild, %d before\n%s", len(after), len(before), out)
	}
	for key, want := range before {
		if got := after[key]; got != want {
			t.Fatalf("segment %s differs after the rebuild:\n before %+v\n after  %+v", key, want, got)
		}
	}
	// Files that were waiting to be copied are queued again.
	pending, _ := db.PendingCopies()
	for _, p := range pending {
		if p.Segment.CameraID != "a" || p.DestinationID != "dest" {
			t.Fatalf("unexpected pending copy %+v", p)
		}
	}
}

// config.json unreadable: the newest good backup is loaded and the status is
// Critical until someone confirms the settings (spec 5.3, 9).
func TestUnreadableConfigFallsBackToBackup(t *testing.T) {
	t.Parallel()
	s := newSystem(t, nil, nil)
	store := config.NewStore(s.root, config.Env{})
	res, _ := store.Load("")
	res.Config.Server.Port = 9123
	if err := store.Save(res.Config); err != nil { // creates config.json.1
		t.Fatal(err)
	}
	os.WriteFile(store.Path(), []byte(`{"schemaVersion": 1, "cameras": [`), 0o600)
	s.start()
	s.g.runHealthCheck()
	h := s.g.Health()
	if h.Status != api.StatusCritical || len(h.Reasons) != 1 || h.Reasons[0].Code != "config_recovered" {
		t.Fatalf("health %+v", h)
	}
	if kept, _ := filepath.Glob(store.Path() + ".unreadable-*"); len(kept) != 1 {
		t.Fatal("the unreadable file was not kept for inspection")
	}
	// Saving the settings confirms them.
	if _, err := s.g.UpdateConfig(func(c *config.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s.g.runHealthCheck()
	if h := s.g.Health(); h.Status != api.StatusHealthy {
		t.Fatalf("still %s after the settings were confirmed: %+v", h.Status, h.Reasons)
	}
}

// A corrupt database is moved aside and the index is rebuilt from the files
// on disk, with a Warning (spec 9).
func TestCorruptDatabaseIsRebuiltFromDisk(t *testing.T) {
	t.Parallel()
	s := newSystem(t, []simulator.Stream{{Path: "/a"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		cfg.Cameras = []config.Camera{simCam(sim, "a", "/a")}
	})
	s.start()
	s.waitRecording("a")
	time.Sleep(1500 * time.Millisecond)
	s.cut("a")
	want := s.segments("a")[0]
	s.stop()

	dbPath := filepath.Join(s.root, index.FileName)
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	os.WriteFile(dbPath, []byte(strings.Repeat("this is not a database ", 200)), 0o600)

	s.start()
	s.until(20*time.Second, "index to be rebuilt", func() bool {
		segs := s.segments("a")
		return len(segs) > 0 && segs[0].FileName == want.FileName && segs[0].Closed()
	})
	got := s.segments("a")[0]
	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.Bytes != want.Bytes {
		t.Fatalf("rebuilt segment %+v, want %+v", got, want)
	}
	s.g.runHealthCheck()
	h := s.g.Health()
	found := false
	for _, r := range h.Reasons {
		found = found || (r.Code == "index_rebuilt" && r.Severity == ipc.SevWarning)
	}
	if !found {
		t.Fatalf("no index_rebuilt warning: %+v", h)
	}
	if kept, _ := filepath.Glob(dbPath + ".corrupt-*"); len(kept) != 1 {
		t.Fatal("the damaged database was not kept")
	}
}

// Deleting a camera with "keep recordings" off removes its indexed files
// everywhere and nothing else.
func TestDeleteCameraWithRecordings(t *testing.T) {
	t.Parallel()
	var dest string
	s := newSystem(t, []simulator.Stream{{Path: "/a"}, {Path: "/b"}}, func(cfg *config.Config, sim *simulator.Server, root string) {
		dest = filepath.Join(root, "dest")
		cfg.Destinations = []config.Destination{{ID: "dest", Name: "D", Type: config.DestLocal, Path: dest, CapGB: 10, Enabled: true}}
		cfg.Cameras = []config.Camera{simCam(sim, "gone", "/a", "dest"), simCam(sim, "stays", "/b", "dest")}
	})
	s.start()
	s.waitRecording("gone", "stays")
	time.Sleep(1500 * time.Millisecond)
	s.cut("gone")
	s.cut("stays")
	s.until(30*time.Second, "copies", func() bool {
		return copyState(s.segments("gone")[0], "dest") == index.CopyVerified && copyState(s.segments("stays")[0], "dest") == index.CopyVerified
	})
	if _, err := s.g.UpdateConfig(func(c *config.Config) error { c.Cameras = c.Cameras[1:]; return nil }); err != nil {
		t.Fatal(err)
	}
	s.g.PurgeCamera("gone")
	s.until(30*time.Second, "the deleted camera's files to be removed", func() bool {
		left, _ := filepath.Glob(filepath.Join(dest, "gone", "*", "*.mp4"))
		staged, _ := filepath.Glob(filepath.Join(s.root, "staging", "gone", "*", "*.mp4"))
		return len(left) == 0 && len(staged) == 0
	})
	if kept, _ := filepath.Glob(filepath.Join(dest, "stays", "*", "*.mp4")); len(kept) == 0 {
		t.Fatal("another camera's recordings were deleted")
	}
	if !s.recording("stays") {
		t.Fatal("deleting one camera disturbed another")
	}
	if n := s.g.Workers(); n != 1 {
		t.Fatalf("%d workers", n)
	}
}
