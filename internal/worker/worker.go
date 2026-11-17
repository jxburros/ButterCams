// Package worker implements the recorder worker process (spec 4.2 and 6): it
// receives its cameras from the guardian over stdin, records each one in its
// own goroutine into the staging folder, and reports heartbeats, statistics
// and file events over stdout.
package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/restream"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Reporting intervals (spec 4.2).
const (
	HeartbeatInterval = 5 * time.Second
	StatsInterval     = 5 * time.Second
	// ClockJumpThreshold is the wall-versus-monotonic drift treated as a
	// clock change (spec 6.6).
	ClockJumpThreshold = 5 * time.Second
	// shutdownGrace is how long cameras get to finalize their files.
	shutdownGrace = 12 * time.Second
)

// Worker is one recorder worker process.
type Worker struct {
	id  int
	log *slog.Logger
	out *outbox

	mu      sync.Mutex
	cams    map[string]*Camera
	bases   map[string]*statsBase
	staging string

	sessionSeq atomic.Uint64

	// The loopback restream (spec 16.3); nil while it is switched off. Only
	// the message loop changes rs and rsToken.
	rs           atomic.Pointer[restream.Server]
	rsToken      string
	restreamOn   atomic.Bool
	testPumpHook atomic.Pointer[func(cameraID string)]
	statNow      chan string // camera ids whose state just changed

	// now is the wall clock. It is time.Now except in tests, which shift it
	// to bring an hour boundary close.
	now func() time.Time
}

func (w *Worker) nextSessionID() uint64 { return w.sessionSeq.Add(1) }

// send queues a message for the guardian. It never blocks (see outbox). A
// failed write means the guardian is gone; the stdin reader notices that and
// shuts the worker down.
func (w *Worker) send(msg any) {
	w.out.post(msg)
}

// Run executes the worker until the guardian asks it to stop, stdin closes
// (the guardian is gone) or ctx is cancelled. Files are finalized before it
// returns.
func Run(ctx context.Context, id int, in io.Reader, out io.Writer, log *slog.Logger) error {
	return runWithClock(ctx, id, in, out, log, time.Now)
}

func runWithClock(ctx context.Context, id int, in io.Reader, out io.Writer, log *slog.Logger, now func() time.Time) error {
	return newWorker(id, out, log, now).run(ctx, in)
}

func newWorker(id int, out io.Writer, log *slog.Logger, now func() time.Time) *Worker {
	return &Worker{
		now: now,
		id:  id,
		log: log,
		out: newOutbox(ipc.NewWriter(out), func(err error) {
			log.Debug("could not send message to the guardian", "error", err.Error())
		}),
		cams:  map[string]*Camera{},
		bases: map[string]*statsBase{},

		statNow: make(chan string, 256),
	}
}

// reportNow queues an immediate statistics report for one camera.
func (w *Worker) reportNow(id string) {
	select {
	case w.statNow <- id:
	default: // the regular report follows within seconds
	}
}

func (w *Worker) run(ctx context.Context, in io.Reader) error {
	// The last segment_closed messages must reach the guardian before exit.
	defer func() {
		if !w.out.flush(3 * time.Second) {
			w.log.Warn("some messages to the guardian could not be delivered before exit")
		}
	}()
	id, log := w.id, w.log

	msgs := make(chan any, 16)
	readErr := make(chan error, 1)
	go func() {
		r := ipc.NewReader(in)
		for {
			m, err := r.Next()
			if errors.Is(err, ipc.ErrMalformed) {
				log.Warn("ignoring malformed message from the guardian", "error", err.Error())
				continue
			}
			if err != nil {
				// End of input or a broken pipe: the guardian is gone.
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
			msgs <- m
		}
	}()

	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	stats := time.NewTicker(StatsInterval)
	defer stats.Stop()
	clock := time.NewTicker(time.Second)
	defer clock.Stop()
	clockRef := time.Now()

	w.send(&ipc.Heartbeat{WorkerID: id, MemBytes: winsvc.ProcessMemoryBytes()})
	log.Info("worker started", "worker", id)

	for {
		select {
		case <-ctx.Done():
			w.shutdown("context cancelled")
			return nil
		case err := <-readErr:
			// stdin closed: the guardian is gone. Finalize and exit.
			w.shutdown("guardian connection closed")
			return err
		case m := <-msgs:
			if w.handle(m) {
				w.shutdown("shutdown requested")
				return nil
			}
		case <-heartbeat.C:
			w.send(&ipc.Heartbeat{WorkerID: id, MemBytes: winsvc.ProcessMemoryBytes()})
		case now := <-stats.C:
			w.reportStats(now)
		case id := <-w.statNow:
			w.mu.Lock()
			c := w.cams[id]
			w.mu.Unlock()
			if c != nil {
				w.send(c.stats(nil, time.Now()))
			}
		case now := <-clock.C:
			wallDelta := now.Round(0).Sub(clockRef.Round(0))
			monoDelta := now.Sub(clockRef)
			drift := wallDelta - monoDelta
			if drift > ClockJumpThreshold || drift < -ClockJumpThreshold {
				log.Warn("system clock changed", "jumpSeconds", drift.Seconds())
				w.send(&ipc.Event{Severity: ipc.SevWarning, Code: "clock_jump",
					Message: "The system clock changed; new files are started with the corrected time.",
					Data:    map[string]any{"jumpSeconds": drift.Seconds()}})
				clockRef = now // each camera notices the jump itself and starts a new file
			} else if monoDelta > time.Minute {
				clockRef = now // re-anchor so slow NTP slewing never adds up
			}
		}
	}
}

// handle processes one guardian message. It returns true on shutdown.
func (w *Worker) handle(m any) (stop bool) {
	switch msg := m.(type) {
	case *ipc.Assign:
		w.mu.Lock()
		w.staging = msg.StagingPath
		want := map[string]bool{}
		for _, cam := range msg.Cameras {
			want[cam.ID] = true
		}
		var removed []*Camera
		for id, c := range w.cams {
			if !want[id] {
				removed = append(removed, c)
				delete(w.cams, id)
				delete(w.bases, id)
			}
		}
		for _, c := range w.cams {
			c.SetStaging(msg.StagingPath)
		}
		w.mu.Unlock()
		for _, c := range removed {
			c.Stop()
		}
		for _, cam := range msg.Cameras {
			w.upsert(cam, msg.UsedNames[cam.ID])
		}
	case *ipc.UpdateCamera:
		w.upsert(msg.Camera, msg.UsedNames)
	case *ipc.RemoveCamera:
		w.mu.Lock()
		c := w.cams[msg.CameraID]
		delete(w.cams, msg.CameraID)
		delete(w.bases, msg.CameraID)
		w.mu.Unlock()
		if c != nil {
			c.Stop()
		}
	case *ipc.RestartCamera:
		w.mu.Lock()
		c := w.cams[msg.CameraID]
		w.mu.Unlock()
		if c != nil {
			reason := msg.Reason
			if reason == "" {
				reason = "restart requested"
			}
			c.Restart(reason)
		}
	case *ipc.SetStaging:
		w.mu.Lock()
		w.staging = msg.StagingPath
		for _, c := range w.cams {
			c.SetStaging(msg.StagingPath)
		}
		w.mu.Unlock()
	case *ipc.Restream:
		w.setRestream(msg)
	case *ipc.Shutdown:
		return true
	}
	return false
}

func (w *Worker) upsert(cfg config.Camera, usedNames []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c, ok := w.cams[cfg.ID]; ok {
		c.Update(cfg)
		return
	}
	c := newCamera(w, cfg, w.staging, usedNames)
	w.cams[cfg.ID] = c
	w.bases[cfg.ID] = &statsBase{at: time.Now()}
	go c.run()
}

func (w *Worker) reportStats(now time.Time) {
	w.mu.Lock()
	type pair struct {
		c *Camera
		b *statsBase
	}
	list := make([]pair, 0, len(w.cams))
	for id, c := range w.cams {
		list = append(list, pair{c, w.bases[id]})
	}
	w.mu.Unlock()
	for _, p := range list {
		w.send(p.c.stats(p.b, now))
	}
}

// shutdown stops every camera and waits for the files to be finalized.
func (w *Worker) shutdown(why string) {
	w.log.Info("worker stopping", "reason", why)
	w.closeRestream() // viewers first: nothing they do is worth waiting for
	w.mu.Lock()
	cams := make([]*Camera, 0, len(w.cams))
	for _, c := range w.cams {
		cams = append(cams, c)
	}
	w.mu.Unlock()
	for _, c := range cams {
		c.Stop()
	}
	deadline := time.After(shutdownGrace)
	for _, c := range cams {
		select {
		case <-c.done:
		case <-deadline:
			w.log.Warn("some cameras did not finalize their files in time")
			return
		}
	}
	w.log.Info("worker stopped; all files finalized")
}
