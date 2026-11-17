// Package guardian implements the ButterCams service process (spec 4.1): it
// starts, watches and restarts the recorder workers and the mover, owns
// config.json and the SQLite index, serves the web UI and API, and runs the
// health check.
package guardian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/logx"
	"github.com/jxburros/buttercams/internal/version"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Heartbeat limits (spec 9).
const (
	WorkerHeartbeatTimeout = 15 * time.Second
	MoverHeartbeatTimeout  = 30 * time.Second
	// WorkerMemoryLimit is the memory use above which a worker is restarted
	// at the next hour boundary, after the rollover.
	WorkerMemoryLimit = 512 << 20
)

// LockName is the file that marks a running guardian in the data root.
const LockName = "guardian.lock"

// Options configure a guardian.
type Options struct {
	// DataRoot holds config.json, the database, logs and certificates.
	DataRoot string
	// Exe is the executable used to start workers and the mover; empty means
	// this executable.
	Exe string
	// Console also prints the log to stderr (the "console" sub-command).
	Console bool
	// DisableAPI skips the web server (some tests).
	DisableAPI bool
	// Timing overrides for tests; zero values use the real intervals.
	HealthInterval  time.Duration
	CleanupInterval time.Duration
}

type workerState struct {
	id         int
	proc       *Proc
	cams       []string
	memRestart bool
}

type camLive struct {
	stats    ipc.CameraStats
	received time.Time
	workerID int
	// notRecordingSince is when the camera was last seen leaving the
	// recording state (or when it was added); zero while it records.
	notRecordingSince time.Time
	lastState         string

	// on-disk watchdog (spec 10.2)
	watchFile    string
	watchBytes   int64
	watchStrikes int
}

type destLive struct {
	lastError   string
	lastSuccess time.Time
	lastFailed  bool
	reachable   bool
	probed      bool
	free, total uint64
}

// Guardian is the service process.
type Guardian struct {
	opts    Options
	log     *slog.Logger
	logs    *logx.Files
	evlog   winsvc.EventLog
	store   *config.Store
	db      *index.DB
	job     *winsvc.Job
	started time.Time
	exe     string
	unlock  func()

	cfgMu  sync.RWMutex // guards cfg
	cfg    *config.Config
	cfgSeq sync.Mutex // serializes configuration changes

	mu          sync.Mutex
	workers     map[int]*workerState
	camWorker   map[string]int
	cams        map[string]*camLive
	dests       map[string]*destLive
	staging     destLive
	oldStaging  []string
	purged      map[string]time.Time
	pendingDel  map[string]map[int64]bool // deletes sent to the mover, by destination
	mover       *Proc
	media       mediaState
	health      api.Health
	sticky      []api.Reason // reasons that last until the guardian restarts
	rebuild     api.RebuildStatus
	lastCleanup time.Time

	indexMu sync.Mutex // serializes index-changing message handling with rebuilds

	server *api.Server
	wg     sync.WaitGroup
	ctx    context.Context
	ready  chan struct{} // closed once start-up has finished
}

// New prepares a guardian. Nothing runs until Run is called.
func New(opts Options) *Guardian {
	return &Guardian{
		opts:       opts,
		started:    time.Now(),
		workers:    map[int]*workerState{},
		camWorker:  map[string]int{},
		cams:       map[string]*camLive{},
		dests:      map[string]*destLive{},
		purged:     map[string]time.Time{},
		pendingDel: map[string]map[int64]bool{},
		ready:      make(chan struct{}),
		media:      mediaState{restreamToken: newToken(), restreams: map[int]int{}, shares: map[string]time.Time{}},
	}
}

// Run starts the guardian and blocks until ctx is cancelled, then shuts
// everything down in order. It returns an error only when it cannot start.
func Run(ctx context.Context, opts Options) error {
	return New(opts).Run(ctx)
}

// Run starts this guardian; see the package-level Run.
func (g *Guardian) Run(ctx context.Context) (err error) {
	g.ctx = ctx
	// A panic that escapes must end the process with a non-zero exit code so
	// the service manager restarts it (spec 9).
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("guardian panic: %v\n%s", r, debug.Stack())
			if g.log != nil {
				g.log.Error("guardian panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			}
			if g.evlog != nil {
				g.evlog.Error(msg)
			}
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(2)
		}
	}()
	if err := g.start(); err != nil {
		if g.log != nil {
			g.log.Error("guardian could not start", "error", err.Error())
		}
		g.closeResources()
		return err
	}
	close(g.ready)
	<-ctx.Done()
	g.shutdown()
	return nil
}

func (g *Guardian) start() error {
	root := g.opts.DataRoot
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating the data root %s: %w", root, err)
	}
	unlock, err := winsvc.LockFile(filepath.Join(root, LockName))
	if err != nil {
		if errors.Is(err, winsvc.ErrLocked) {
			return fmt.Errorf("another ButterCams service is already running with the data root %s", root)
		}
		return err
	}
	g.unlock = unlock

	var tee *os.File
	if g.opts.Console {
		tee = os.Stderr
	}
	logs, err := logx.OpenFiles(filepath.Join(root, "logs"), config.DefaultLogRetentionDays, teeWriter(tee))
	if err != nil {
		return fmt.Errorf("opening the log folder: %w", err)
	}
	g.logs = logs
	g.log = logx.New(logs, "guardian")
	g.evlog = winsvc.OpenEventLog()
	g.log.Info("ButterCams starting", "version", version.Version, "dataRoot", root)

	g.exe = g.opts.Exe
	if g.exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		g.exe = exe
	}

	// Every child is put in this job as soon as it has started. The job is
	// kill-on-close: when the guardian exits for any reason the operating
	// system closes the handle and terminates whatever is still in the job,
	// so children never outlive the guardian. The guardian itself stays
	// outside the job.
	if job, err := winsvc.NewKillOnCloseJob(); err != nil {
		g.log.Warn("could not create the job object; children may outlive the service", "error", err.Error())
	} else {
		g.job = job
	}

	// Configuration.
	env := config.Env{IsRemoteDrive: winsvc.IsRemoteDrive}
	g.store = config.NewStore(root, env)
	res, err := g.store.Load(filepath.Join(root, "staging"))
	if err != nil {
		g.evlog.Error("ButterCams cannot read its configuration: " + err.Error())
		return err
	}
	g.cfg = res.Config
	switch {
	case res.Created:
		if err := g.store.Save(g.cfg); err != nil {
			return fmt.Errorf("writing the first configuration: %w", err)
		}
		g.log.Info("created a new configuration", "path", g.store.Path())
	case res.RecoveredFrom != "":
		msg := fmt.Sprintf("config.json could not be read (%s). Settings were loaded from the backup %s; check them and save to confirm.",
			res.Problem, res.RecoveredFrom)
		g.log.Error("configuration unreadable; loaded a backup", "problem", res.Problem, "backup", res.RecoveredFrom)
		g.evlog.Error("ButterCams: " + msg)
		g.sticky = append(g.sticky, api.Reason{Severity: ipc.SevCritical, Code: "config_recovered", Message: msg})
		// Keep the unreadable file for inspection and put the good settings
		// in its place so the next start is clean.
		os.Rename(g.store.Path(), g.store.Path()+".unreadable-"+time.Now().Format("20060102-150405"))
		if err := g.store.Save(g.cfg); err != nil {
			g.log.Error("could not rewrite config.json from the backup", "error", err.Error())
		}
	case res.MigratedFrom != 0:
		g.log.Info("configuration migrated", "from", res.MigratedFrom, "to", config.SchemaVersion)
		if err := g.store.Save(g.cfg); err != nil {
			return fmt.Errorf("saving the migrated configuration: %w", err)
		}
	}
	g.logs.SetRetentionDays(g.cfg.Logging.RetentionDays)
	if !config.SecretsProtected {
		g.log.Warn("this is a development build for a non-Windows system: passwords in config.json are NOT encrypted")
	}

	// Index.
	dbPath := filepath.Join(root, index.FileName)
	needRebuild := false
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		needRebuild = true // a new or deleted database: pick up whatever is on disk
	} else if err := index.QuickCheck(dbPath); err != nil {
		bad := dbPath + ".corrupt-" + time.Now().Format("20060102-150405")
		g.log.Error("the database is corrupt; rebuilding the index from the files on disk", "error", err.Error(), "movedTo", bad)
		os.Rename(dbPath, bad)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
		g.sticky = append(g.sticky, api.Reason{Severity: ipc.SevWarning, Code: "index_rebuilt",
			Message: "The database was damaged and has been rebuilt from the recordings on disk. The damaged file was kept as " + filepath.Base(bad) + "."})
		needRebuild = true
	}
	db, err := index.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	g.db = db
	for _, cam := range g.cfg.Cameras {
		g.db.UpsertCamera(cam.ID, cam.Name)
	}
	g.loadOldStaging()

	now := time.Now()
	for _, cam := range g.cfg.Cameras {
		g.cams[cam.ID] = &camLive{notRecordingSince: now}
	}
	for _, d := range g.cfg.Destinations {
		g.dests[d.ID] = &destLive{}
	}
	g.health = api.Health{Status: api.StatusHealthy, Reasons: []api.Reason{}, CheckedAt: now}

	// Web UI and API.
	if !g.opts.DisableAPI {
		srv, err := api.NewServer(g, root, g.log)
		if err != nil {
			return fmt.Errorf("starting the web server: %w", err)
		}
		g.server = srv
		if err := srv.Start(g.cfg.Server); err != nil {
			// A port that is already in use must not stop recording.
			g.log.Error("the web server could not start; recording continues", "error", err.Error())
			g.sticky = append(g.sticky, api.Reason{Severity: ipc.SevCritical, Code: "web_server_failed",
				Message: "The settings page could not start: " + err.Error()})
		}
	}

	// Children.
	g.startMover()
	g.assignAll()
	g.syncMedia()

	g.event(index.Event{Source: "guardian", Type: "service_started", Severity: ipc.SevInfo,
		Data: map[string]any{"message": "ButterCams " + version.Version + " started.", "version": version.Version}})
	g.evlog.Info("ButterCams " + version.Version + " started. Data root: " + root)

	if needRebuild {
		g.startRebuild(false)
	}
	healthEvery := g.opts.HealthInterval
	if healthEvery == 0 {
		healthEvery = HealthInterval
	}
	// The first check runs shortly after start instead of a full interval later.
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		first := 10 * time.Second
		if healthEvery < first {
			first = healthEvery
		}
		select {
		case <-g.ctx.Done():
		case <-time.After(first):
			g.safely("health", g.runHealthCheck)
		}
	}()
	g.goLoop("health", healthEvery, g.runHealthCheck)
	g.goLoop("memory", time.Minute, g.restartHeavyWorkers)
	g.goLoop("media-memory", time.Minute, g.restartHeavyMedia)
	g.goLoop("maintenance", time.Hour, g.maintain)
	g.goLoop("stream", StreamInterval, g.publishLive)
	return nil
}

// teeWriter returns the extra log destination for console mode, or nil.
func teeWriter(f *os.File) io.Writer {
	if f == nil {
		return nil // a nil *os.File inside an interface would not be nil
	}
	return f
}

// goLoop runs fn every interval until the guardian stops. A panic in fn is
// logged and the loop carries on (spec 9: recover wraps every goroutine).
func (g *Guardian) goLoop(name string, every time.Duration, fn func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-t.C:
				g.safely(name, fn)
			}
		}
	}()
}

func (g *Guardian) safely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			g.log.Error("guardian task panicked", "task", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// shutdown stops the children (giving them time to finalize files), then the
// web server, then closes the database.
func (g *Guardian) shutdown() {
	g.log.Info("ButterCams stopping")
	g.mu.Lock()
	procs := []*Proc{}
	for _, w := range g.workers {
		procs = append(procs, w.proc)
	}
	mover := g.mover
	media := g.media.proc
	g.mu.Unlock()

	if media != nil { // viewers first: nothing they do is worth waiting for
		go g.safely("stop-media", func() { media.Stop(2 * time.Second) })
	}
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(p *Proc) { defer wg.Done(); p.Stop(StopGrace) }(p)
	}
	wg.Wait() // workers first: their final segment_closed messages are still handled
	if mover != nil {
		mover.Stop(5 * time.Second)
	}
	if g.server != nil {
		g.server.Close()
	}
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	g.event(index.Event{Source: "guardian", Type: "service_stopped", Severity: ipc.SevInfo,
		Data: map[string]any{"message": "ButterCams stopped."}})
	g.evlog.Info("ButterCams stopped.")
	g.log.Info("ButterCams stopped")
	g.closeResources()
}

func (g *Guardian) closeResources() {
	if g.db != nil {
		g.db.Checkpoint(context.Background())
		g.db.Close()
		g.db = nil
	}
	if g.job != nil {
		g.job.Close()
		g.job = nil
	}
	if g.evlog != nil {
		g.evlog.Close()
	}
	if g.logs != nil {
		g.logs.Close()
	}
	if g.unlock != nil {
		g.unlock()
		g.unlock = nil
	}
}

// childLog stores a line a child wrote to stderr. Children log JSON lines;
// anything else (a Go crash trace) is wrapped so it is not lost.
func (g *Guardian) childLog(name string) func([]byte) {
	return func(line []byte) {
		if len(line) > 0 && line[0] == '{' && json.Valid(line) {
			g.logs.WriteLine(line)
			return
		}
		g.log.Error("child output", "child", name, "line", string(line))
	}
}

// event stores an event, forwards it to connected browsers and, for Critical
// events, writes it to the Windows Event Log.
func (g *Guardian) event(e index.Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	if g.db != nil {
		id, err := g.db.InsertEvent(e)
		if err != nil {
			g.log.Error("could not store event", "type", e.Type, "error", err.Error())
		}
		e.ID = id
	}
	msg, _ := e.Data["message"].(string)
	level := slog.LevelInfo
	switch e.Severity {
	case ipc.SevWarning:
		level = slog.LevelWarn
	case ipc.SevCritical:
		level = slog.LevelError
		g.evlog.Error("ButterCams: " + e.Type + ": " + msg)
	}
	g.log.Log(context.Background(), level, "event", "type", e.Type, "source", e.Source, "camera", e.CameraID, "message", msg)
	if g.server != nil {
		g.server.PublishEvent(e)
	}
}

// ------------------------------------------------------------ api.Backend

// Config returns a copy of the current configuration.
func (g *Guardian) Config() *config.Config {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.cfg.Clone()
}

func (g *Guardian) config() *config.Config {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	return g.cfg
}

// Index returns the database.
func (g *Guardian) Index() *index.DB { return g.db }

// StartedAt returns when the guardian started.
func (g *Guardian) StartedAt() time.Time { return g.started }

// DataRoot returns the data root.
func (g *Guardian) DataRoot() string { return g.opts.DataRoot }

// Workers returns the number of worker processes.
func (g *Guardian) Workers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.workers)
}

// RecordEvent stores an event raised by the API.
func (g *Guardian) RecordEvent(e index.Event) { g.event(e) }

// Health returns the latest health check result.
func (g *Guardian) Health() api.Health {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.health
	h.Reasons = append([]api.Reason{}, h.Reasons...)
	return h
}

// CameraStats returns the live state of every camera that has reported.
func (g *Guardian) CameraStats() map[string]api.CameraStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]api.CameraStats, len(g.cams))
	for id, c := range g.cams {
		if c.received.IsZero() {
			continue
		}
		out[id] = api.CameraStats{CameraStats: c.stats, WorkerID: c.workerID, UpdatedAt: c.received}
	}
	return out
}
