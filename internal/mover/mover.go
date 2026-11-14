// Package mover implements the copy-and-retention process (spec 4.3, 7, 8).
// It receives closed segments from the guardian, copies each one to its
// destinations with verification, measures free space, and deletes the files
// the guardian selects. It never opens the database.
package mover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Concurrency is the number of copies that run at the same time, bounded so
// copying never saturates the disk the recorders write to.
const Concurrency = 4

// HeartbeatInterval is how often the mover reports that it is alive.
const HeartbeatInterval = 5 * time.Second

// retrySteps is the retry schedule for a failing destination (spec 7.3):
// 30 s, 1, 2, 5, 10 minutes, then every 10 minutes forever.
var retrySteps = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// RetryDelay returns the wait after the n-th consecutive failure (n ≥ 1).
func RetryDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > len(retrySteps) {
		n = len(retrySteps)
	}
	return retrySteps[n-1]
}

type destState struct {
	cfg       config.Destination
	queue     []*ipc.CopyJob // oldest first
	active    map[int64]bool // segment ids being copied
	cancelled map[int64]bool // copies in flight that were withdrawn
	failures  int            // consecutive failed attempts
	nextTry   time.Time
	connected bool
	probing   bool
	deleting  bool
}

// Mover is the mover process.
type Mover struct {
	log *slog.Logger
	out *ipc.Writer

	mu          sync.Mutex
	staging     config.Staging
	oldStaging  []string
	dests       map[string]*destState
	running     int
	stagingBusy bool

	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// retry lets tests shorten the schedule.
	retry func(failures int) time.Duration
	// probeInterval lets tests shorten the free-space interval.
	probeInterval time.Duration
}

func (m *Mover) send(msg any) {
	if err := m.out.Send(msg); err != nil {
		m.log.Debug("could not send message to the guardian", "error", err.Error())
	}
}

func (m *Mover) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run executes the mover until the guardian asks it to stop or stdin closes.
func Run(ctx context.Context, in io.Reader, out io.Writer, log *slog.Logger) error {
	return newMover(out, log).run(ctx, in)
}

func newMover(out io.Writer, log *slog.Logger) *Mover {
	return &Mover{
		log:           log,
		out:           ipc.NewWriter(out),
		dests:         map[string]*destState{},
		wake:          make(chan struct{}, 1),
		retry:         RetryDelay,
		probeInterval: RetentionInterval,
	}
}

func (m *Mover) run(parent context.Context, in io.Reader) error {
	m.ctx, m.cancel = context.WithCancel(parent)
	defer m.cancel()

	msgs := make(chan any, 64)
	readErr := make(chan error, 1)
	go func() {
		r := ipc.NewReader(in)
		for {
			msg, err := r.Next()
			if errors.Is(err, ipc.ErrMalformed) {
				m.log.Warn("ignoring malformed message from the guardian", "error", err.Error())
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
			msgs <- msg
		}
	}()

	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	probe := time.NewTicker(m.probeInterval)
	defer probe.Stop()
	retry := time.NewTicker(time.Second)
	defer retry.Stop()

	m.send(&ipc.Heartbeat{MemBytes: winsvc.ProcessMemoryBytes()})
	m.log.Info("mover started")

	for {
		select {
		case <-m.ctx.Done():
			m.stop("context cancelled")
			return nil
		case err := <-readErr:
			m.stop("guardian connection closed")
			return err
		case msg := <-msgs:
			if m.handle(msg) {
				m.stop("shutdown requested")
				return nil
			}
		case <-heartbeat.C:
			m.send(&ipc.Heartbeat{MemBytes: winsvc.ProcessMemoryBytes()})
		case <-probe.C:
			m.probeAll()
		case <-retry.C:
			m.dispatch()
		case <-m.wake:
			m.dispatch()
		}
	}
}

func (m *Mover) stop(why string) {
	m.log.Info("mover stopping", "reason", why)
	m.cancel() // interrupts copies in flight; their .part files are overwritten on the next attempt
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// handle processes one guardian message. It returns true on shutdown.
func (m *Mover) handle(msg any) (stop bool) {
	switch v := msg.(type) {
	case *ipc.Destinations:
		m.setDestinations(v)
		m.probeAll()
	case *ipc.CopyJob:
		m.enqueue(v)
	case *ipc.CancelCopy:
		m.cancelCopies(v)
	case *ipc.RetentionJob:
		m.startDeletes(v)
	case *ipc.Shutdown:
		return true
	}
	return false
}

func (m *Mover) setDestinations(v *ipc.Destinations) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.staging = v.Staging
	m.oldStaging = v.OldStagingPaths
	seen := map[string]bool{}
	for _, d := range v.Destinations {
		seen[d.ID] = true
		st := m.dests[d.ID]
		if st == nil {
			m.dests[d.ID] = &destState{cfg: d, active: map[int64]bool{}}
			continue
		}
		if st.cfg.Path != d.Path || st.cfg.Username != d.Username || st.cfg.PasswordEnc != d.PasswordEnc ||
			st.cfg.Enabled != d.Enabled || st.cfg.Type != d.Type {
			// The connection details changed: start over.
			st.failures, st.connected = 0, false
		}
		// Someone just saved the storage settings, most likely to fix
		// something: try again now instead of sitting out the retry wait.
		st.nextTry = time.Time{}
		st.cfg = d
	}
	for id := range m.dests {
		if !seen[id] {
			delete(m.dests, id)
		}
	}
	m.pokeLocked()
}

func (m *Mover) pokeLocked() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Mover) enqueue(job *ipc.CopyJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.dests[job.DestinationID]
	if st == nil {
		return // the destination was removed; the guardian drops the copy
	}
	if st.active[job.SegmentID] {
		return
	}
	for _, q := range st.queue {
		if q.SegmentID == job.SegmentID {
			return
		}
	}
	st.queue = append(st.queue, job)
	sort.SliceStable(st.queue, func(i, j int) bool { return st.queue[i].Start.Before(st.queue[j].Start.Time) })
	m.pokeLocked()
}

func (m *Mover) cancelCopies(v *ipc.CancelCopy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, st := range m.dests {
		if v.DestinationID != "" && v.DestinationID != id {
			continue
		}
		keep := st.queue[:0]
		for _, q := range st.queue {
			if v.SegmentID != 0 && q.SegmentID != v.SegmentID {
				keep = append(keep, q)
			}
		}
		st.queue = keep
		// Copies already in flight finish, but are not retried or reported.
		for id := range st.active {
			if v.SegmentID == 0 || v.SegmentID == id {
				if st.cancelled == nil {
					st.cancelled = map[int64]bool{}
				}
				st.cancelled[id] = true
			}
		}
	}
}

// dispatch starts as many copies as the pool and the retry schedule allow.
func (m *Mover) dispatch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	ids := make([]string, 0, len(m.dests))
	for id := range m.dests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for progress := true; progress && m.running < Concurrency; {
		progress = false
		for _, id := range ids {
			st := m.dests[id]
			if m.running >= Concurrency {
				break
			}
			if !st.cfg.Enabled || len(st.queue) == 0 || now.Before(st.nextTry) {
				continue
			}
			// A destination that is failing gets one probe copy at a time.
			if st.failures > 0 && len(st.active) > 0 {
				continue
			}
			job := st.queue[0]
			st.queue = st.queue[1:]
			st.active[job.SegmentID] = true
			m.running++
			progress = true
			m.wg.Add(1)
			go m.copyOne(st, job)
		}
	}
}

func (m *Mover) copyOne(st *destState, job *ipc.CopyJob) {
	defer m.wg.Done()
	m.mu.Lock()
	dest := st.cfg
	needConnect := !st.connected
	m.mu.Unlock()

	var sum string
	var size int64
	err := error(nil)
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New("internal error while copying")
				m.log.Error("copy panicked", "panic", r)
			}
		}()
		if needConnect {
			if err = connect(dest); err != nil {
				return
			}
		}
		sum, size, err = copyFile(m.ctx, job, dest)
	}()

	m.mu.Lock()
	delete(st.active, job.SegmentID)
	m.running--
	res := &ipc.CopyResult{SegmentID: job.SegmentID, DestinationID: job.DestinationID}
	cancelled := m.ctx.Err() != nil
	if st.cancelled[job.SegmentID] {
		delete(st.cancelled, job.SegmentID)
		m.pokeLocked()
		m.mu.Unlock()
		return // withdrawn by the guardian while it was being copied
	}
	switch {
	case err == nil:
		st.failures, st.nextTry, st.connected = 0, time.Time{}, true
		res.OK, res.SHA256, res.Bytes = true, sum, size
	case errors.Is(err, errSourceMissing):
		res.Error, res.SourceMissing = err.Error(), true
	default:
		st.failures++
		st.connected = false
		delay := m.retry(st.failures)
		st.nextTry = time.Now().Add(delay)
		res.Error = config.Redact(err.Error())
		res.Attempt = st.failures
		res.NextRetrySec = int(delay / time.Second)
		// Keep the job for the retry, oldest first.
		st.queue = append([]*ipc.CopyJob{job}, st.queue...)
	}
	stillKnown := m.dests[job.DestinationID] == st
	m.pokeLocked()
	m.mu.Unlock()

	if cancelled {
		return // shutting down: the guardian re-queues from the database
	}
	if err == nil {
		m.log.Info("copied", "file", job.FileName, "destination", job.DestinationID, "bytes", size)
	} else {
		m.log.Warn("copy failed", "file", job.FileName, "destination", job.DestinationID, "error", res.Error,
			"retryInSec", res.NextRetrySec)
	}
	m.send(res)
	if err == nil && stillKnown {
		m.probe(job.DestinationID) // retention runs after each verified copy
		m.probe(ipc.StagingID)
	}
}

// probeAll measures free space everywhere.
func (m *Mover) probeAll() {
	m.mu.Lock()
	ids := []string{ipc.StagingID}
	for id := range m.dests {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.probe(id)
	}
}

// probe measures the free space of one root in the background and reports
// it. A share that does not answer cannot hold anything else up.
func (m *Mover) probe(id string) {
	m.mu.Lock()
	var path string
	var st *destState
	queue := 0
	if id == ipc.StagingID {
		if m.stagingBusy || m.staging.Path == "" {
			m.mu.Unlock()
			return
		}
		m.stagingBusy = true
		path = m.staging.Path
	} else {
		st = m.dests[id]
		if st == nil || st.probing || !st.cfg.Enabled {
			m.mu.Unlock()
			return
		}
		st.probing = true
		path = st.cfg.Path
		queue = len(st.queue) + len(st.active)
	}
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			recover()
			m.mu.Lock()
			if st != nil {
				st.probing = false
			} else {
				m.stagingBusy = false
			}
			m.mu.Unlock()
		}()
		msg := &ipc.DiskStatus{DestinationID: id, QueueLength: queue}
		if st != nil {
			if err := connect(st.cfg); err != nil {
				msg.Error = config.Redact(err.Error())
				m.send(msg)
				return
			}
		}
		if _, err := os.Stat(path); err != nil && st != nil {
			// A destination folder that does not exist yet is created by the
			// first copy; only a path that cannot be reached is an error.
			if !errors.Is(err, os.ErrNotExist) {
				msg.Error = config.Redact(err.Error())
				m.send(msg)
				return
			}
		}
		free, total, err := winsvc.DiskFree(path)
		if err != nil {
			msg.Error = config.Redact(err.Error())
		} else {
			msg.Reachable, msg.FreeBytes, msg.TotalBytes = true, free, total
		}
		m.send(msg)
	}()
}

// startDeletes removes the files of a retention job in the background.
func (m *Mover) startDeletes(job *ipc.RetentionJob) {
	m.mu.Lock()
	var roots []string
	if job.DestinationID == ipc.StagingID {
		roots = append([]string{m.staging.Path}, m.oldStaging...)
	} else if st := m.dests[job.DestinationID]; st != nil {
		roots = []string{st.cfg.Path}
	}
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		res := &ipc.RetentionResult{DestinationID: job.DestinationID}
		for _, item := range job.Deletes {
			res.Results = append(res.Results, m.deleteOne(roots, item))
		}
		m.send(res)
		m.probe(job.DestinationID)
	}()
}

// deleteOne deletes one indexed file. Only names that follow the ButterCams
// pattern are ever touched (spec rule 7). A file that is already gone counts
// as deleted.
func (m *Mover) deleteOne(roots []string, item ipc.DeleteItem) ipc.DeleteResult {
	r := ipc.DeleteResult{SegmentID: item.SegmentID, FileName: item.FileName, Bytes: item.Bytes, Reason: item.Reason}
	rel, ok := naming.RelPath(item.FileName)
	if !ok {
		r.Error = "not a ButterCams recording name"
		return r
	}
	if len(roots) == 0 {
		r.Error = "unknown destination"
		return r
	}
	var lastErr error
	for _, root := range roots {
		path := filepath.Join(root, rel)
		r.Path = path
		err := os.Remove(path)
		if err == nil {
			r.Deleted = true
			m.log.Info("deleted recording", "path", path, "bytes", item.Bytes, "reason", item.Reason)
			// Tidy up folders that became empty; a non-empty folder stays.
			os.Remove(filepath.Dir(path))
			os.Remove(filepath.Dir(filepath.Dir(path)))
			return r
		}
		if !errors.Is(err, os.ErrNotExist) {
			lastErr = err
		}
	}
	if lastErr != nil {
		r.Error = config.Redact(lastErr.Error())
		m.log.Warn("could not delete recording", "path", r.Path, "reason", item.Reason, "error", r.Error)
		return r
	}
	r.Missing = true
	m.log.Info("recording already gone", "path", r.Path, "bytes", item.Bytes, "reason", item.Reason)
	return r
}
