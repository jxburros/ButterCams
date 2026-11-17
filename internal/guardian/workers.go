package guardian

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// assignAll distributes the configured cameras over workers at start-up: up
// to CamerasPerWorker each, filling workers in camera order (spec 4.1).
func (g *Guardian) assignAll() {
	for _, w := range g.planAssignments() {
		w.proc.Start()
	}
}

// planAssignments fills the worker table and returns the workers to start.
func (g *Guardian) planAssignments() []*workerState {
	cfg := g.config()
	g.mu.Lock()
	var start []*workerState
	for i, cam := range cfg.Cameras {
		id := i/config.CamerasPerWorker + 1
		w := g.workers[id]
		if w == nil {
			w = g.newWorkerLocked(id)
			start = append(start, w)
		}
		w.cams = append(w.cams, cam.ID)
		g.camWorker[cam.ID] = id
	}
	g.mu.Unlock()
	return start
}

func (g *Guardian) newWorkerLocked(id int) *workerState {
	name := "worker:" + strconv.Itoa(id)
	w := &workerState{id: id}
	w.proc = newProc(ProcSpec{
		Name:             name,
		Exe:              g.exe,
		Args:             []string{"worker", "--id", strconv.Itoa(id)},
		Env:              []string{winsvc.EnvDataRoot + "=" + g.opts.DataRoot},
		HeartbeatTimeout: WorkerHeartbeatTimeout,
		OnStart:          func(p *Proc) { g.sendAssign(id); g.sendRestream(p) },
		OnMessage:        func(p *Proc, msg any) { g.onWorkerMessage(id, msg) },
		OnRestart:        func(p *Proc, reason string, loop bool) { g.onWorkerRestart(id, reason, loop) },
	}, g.log, g.childLog(name))
	w.proc.job = g.job
	g.workers[id] = w
	return w
}

// sendAssign gives a worker its complete camera list.
func (g *Guardian) sendAssign(id int) {
	cfg := g.config()
	g.mu.Lock()
	w := g.workers[id]
	if w == nil {
		g.mu.Unlock()
		return
	}
	ids := append([]string(nil), w.cams...)
	g.mu.Unlock()
	msg := &ipc.Assign{StagingPath: cfg.Staging.Path, UsedNames: map[string][]string{}}
	for _, camID := range ids {
		if cam, ok := cfg.Camera(camID); ok {
			msg.Cameras = append(msg.Cameras, cam)
			if names := g.usedNames(camID); len(names) > 0 {
				msg.UsedNames[camID] = names
			}
		}
	}
	w.proc.Send(msg)
}

// usedNames returns the recording names of a camera's last hours. The index
// is the one place that remembers them after the files have been copied away
// from staging, deleted by retention, or lost with a crashed worker's memory.
func (g *Guardian) usedNames(camID string) []string {
	names, err := g.db.RecentNames(camID, time.Now().Add(-3*time.Hour))
	if err != nil {
		g.log.Error("could not read the recent recording names", "camera", camID, "error", err.Error())
	}
	return names
}

// placeCamera assigns a new camera to the first worker with a free slot, or
// to a new worker. Existing assignments never move, so a change only touches
// the worker concerned.
func (g *Guardian) placeCamera(cam config.Camera) {
	g.mu.Lock()
	ids := make([]int, 0, len(g.workers))
	for id := range g.workers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var target *workerState
	for _, id := range ids {
		if len(g.workers[id].cams) < config.CamerasPerWorker {
			target = g.workers[id]
			break
		}
	}
	isNew := false
	if target == nil {
		next := 1
		for g.workers[next] != nil {
			next++
		}
		target = g.newWorkerLocked(next)
		isNew = true
	}
	target.cams = append(target.cams, cam.ID)
	g.camWorker[cam.ID] = target.id
	g.cams[cam.ID] = &camLive{notRecordingSince: time.Now()}
	g.mu.Unlock()

	if isNew {
		target.proc.Start() // OnStart sends the assignment
	} else {
		target.proc.Send(&ipc.UpdateCamera{Camera: cam, UsedNames: g.usedNames(cam.ID)})
	}
}

// removeCamera takes a camera away from its worker. A worker left without
// cameras is stopped.
func (g *Guardian) removeCamera(id string) {
	g.mu.Lock()
	wid, ok := g.camWorker[id]
	delete(g.camWorker, id)
	delete(g.cams, id)
	var w *workerState
	var empty bool
	if ok {
		w = g.workers[wid]
	}
	if w != nil {
		keep := w.cams[:0]
		for _, c := range w.cams {
			if c != id {
				keep = append(keep, c)
			}
		}
		w.cams = keep
		empty = len(w.cams) == 0
		if empty {
			delete(g.workers, wid)
		}
	}
	g.mu.Unlock()
	if w == nil {
		return
	}
	if empty {
		g.forgetRestream(wid)
		go g.safely("stop-worker", func() { w.proc.Stop(StopGrace) })
		return
	}
	w.proc.Send(&ipc.RemoveCamera{CameraID: id})
}

func (g *Guardian) workerFor(camID string) *Proc {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w := g.workers[g.camWorker[camID]]; w != nil {
		return w.proc
	}
	return nil
}

func (g *Guardian) allWorkers() []*workerState {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*workerState, 0, len(g.workers))
	for _, w := range g.workers {
		out = append(out, w)
	}
	return out
}

func (g *Guardian) cameraNames(ids []string) string {
	cfg := g.config()
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if cam, ok := cfg.Camera(id); ok {
			names = append(names, cam.Name)
		} else {
			names = append(names, id)
		}
	}
	return strings.Join(names, ", ")
}

func (g *Guardian) onWorkerRestart(id int, reason string, loop bool) {
	g.forgetRestream(id) // its restream is gone until the new process reports one
	g.mu.Lock()
	w := g.workers[id]
	var cams []string
	if w != nil {
		cams = append(cams, w.cams...)
		w.memRestart = false
	}
	now := time.Now()
	for _, c := range cams {
		if live := g.cams[c]; live != nil && live.notRecordingSince.IsZero() {
			live.notRecordingSince = now
		}
	}
	g.mu.Unlock()
	names := g.cameraNames(cams)
	source := "worker:" + strconv.Itoa(id)
	if loop {
		g.event(index.Event{Source: source, Type: "worker_crash_loop", Severity: ipc.SevCritical,
			Data: map[string]any{"message": fmt.Sprintf("Recorder worker %d keeps restarting (%s). Cameras affected: %s.", id, reason, names),
				"cameras": cams, "reason": reason}})
		return
	}
	g.event(index.Event{Source: source, Type: "worker_restarted", Severity: ipc.SevWarning,
		Data: map[string]any{"message": fmt.Sprintf("Recorder worker %d was restarted (%s). Cameras: %s.", id, reason, names),
			"cameras": cams, "reason": reason}})
	g.evlog.Warning(fmt.Sprintf("ButterCams restarted recorder worker %d (%s).", id, reason))
}

// restartHeavyWorkers restarts workers whose memory grew past the limit, but
// only in the minute after the hourly rollover so no extra gap is added
// mid-hour (spec 9).
func (g *Guardian) restartHeavyWorkers() {
	now := time.Now()
	for _, w := range g.allWorkers() {
		if w.proc.MemBytes() > WorkerMemoryLimit {
			g.mu.Lock()
			if !w.memRestart {
				w.memRestart = true
				g.log.Warn("worker memory is above the limit; it will be restarted after the next hourly rollover",
					"worker", w.id, "memBytes", w.proc.MemBytes())
			}
			g.mu.Unlock()
		}
		g.mu.Lock()
		due := w.memRestart
		g.mu.Unlock()
		since := now.Sub(naming.HourStart(now))
		if due && since >= 30*time.Second && since < 3*time.Minute {
			w.proc.Restart("memory use above 512 MB", StopGrace)
		}
	}
}

// onWorkerMessage handles everything a worker reports.
func (g *Guardian) onWorkerMessage(workerID int, msg any) {
	switch m := msg.(type) {
	case *ipc.CameraStats:
		g.onCameraStats(workerID, m)
	case *ipc.SegmentOpened:
		g.indexMu.Lock()
		defer g.indexMu.Unlock()
		g.noteStagingRoot(m.Path)
		name := naming.FinalName(filepath.Base(m.Path))
		if _, err := g.db.SegmentOpened(m.CameraID, name, m.Start.Time, m.Gap, m.VideoCodec, m.AudioCodec); err != nil {
			// A duplicate name is resolved when the file is closed: it is
			// renamed to the next free name before it is indexed.
			g.log.Error("could not index the new recording", "file", name, "error", err.Error())
		}
	case *ipc.SegmentClosed:
		g.indexMu.Lock()
		defer g.indexMu.Unlock()
		g.onSegmentClosed(m)
	case *ipc.RestreamReady:
		g.onRestreamReady(workerID, m)
	case *ipc.RestreamSession:
		g.onRestreamSession(m)
	case *ipc.Event:
		source := "worker:" + strconv.Itoa(workerID)
		if m.CameraID != "" {
			source = "camera:" + m.CameraID
		}
		data := map[string]any{"message": m.Message}
		for k, v := range m.Data {
			data[k] = v
		}
		g.event(index.Event{TS: m.TS.Time, Source: source, Type: m.Code, Severity: m.Severity, CameraID: m.CameraID, Data: data})
	}
}

func (g *Guardian) onCameraStats(workerID int, m *ipc.CameraStats) {
	now := time.Now()
	g.mu.Lock()
	live := g.cams[m.CameraID]
	if live == nil {
		g.mu.Unlock()
		return // a camera that was just deleted
	}
	// Rates are only measured in the periodic report; an immediate
	// state-change report must not blank them.
	if m.FPS == 0 && m.State == ipc.StateRecording && live.stats.State == ipc.StateRecording {
		m.FPS, m.BitrateKbps = live.stats.FPS, live.stats.BitrateKbps
	}
	changed := live.lastState != m.State
	live.stats, live.received, live.workerID, live.lastState = *m, now, workerID, m.State
	if m.State == ipc.StateRecording {
		live.notRecordingSince = time.Time{}
	} else if live.notRecordingSince.IsZero() {
		live.notRecordingSince = now
	}
	g.mu.Unlock()
	if changed {
		g.db.SetCameraState(m.CameraID, m.State, m.StateSince.Time)
	}
}

// noteStagingRoot remembers staging folders other than the current one that
// hold recordings, so files written before a staging change can still be
// found (spec 7.1).
func (g *Guardian) noteStagingRoot(filePath string) {
	root := filepath.Dir(filepath.Dir(filepath.Dir(filePath)))
	cur := g.config().Staging.Path
	if samePath(root, cur) {
		return
	}
	g.mu.Lock()
	for _, p := range g.oldStaging {
		if samePath(p, root) {
			g.mu.Unlock()
			return
		}
	}
	g.oldStaging = append(g.oldStaging, root)
	g.mu.Unlock()
	g.saveOldStaging()
	g.sendDestinations()
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// stagingRoots returns the current staging folder followed by older ones.
func (g *Guardian) stagingRoots() []string {
	roots := []string{g.config().Staging.Path}
	g.mu.Lock()
	roots = append(roots, g.oldStaging...)
	g.mu.Unlock()
	return roots
}

// findStaging returns the path of a staging file, looking in the current and
// earlier staging folders.
func (g *Guardian) findStaging(fileName string) (string, bool) {
	rel, ok := naming.RelPath(fileName)
	if !ok {
		return "", false
	}
	for _, root := range g.stagingRoots() {
		p := filepath.Join(root, rel)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// copyTargets returns the enabled destinations a camera's recordings go to.
func copyTargets(cfg *config.Config, cameraID string) []string {
	cam, ok := cfg.Camera(cameraID)
	if !ok {
		return nil
	}
	var out []string
	for _, id := range cam.DestinationIDs {
		if d, ok := cfg.Destination(id); ok && d.Enabled {
			out = append(out, id)
		}
	}
	return out
}

// onSegmentClosed indexes a finished file and queues its copies (spec 7.2).
func (g *Guardian) onSegmentClosed(m *ipc.SegmentClosed) {
	cfg := g.config()
	name := filepath.Base(m.Path)
	g.noteStagingRoot(m.Path)

	g.mu.Lock()
	_, purged := g.purged[m.CameraID]
	g.mu.Unlock()
	var targets []string
	if !purged {
		targets = copyTargets(cfg, m.CameraID)
	}

	closed := index.ClosedSegment{
		CameraID: m.CameraID, FileName: name, Start: m.Start.Time, End: m.End.Time, Bytes: m.Bytes, SHA256: m.SHA256,
		VideoCodec: m.VideoCodec, AudioCodec: m.AudioCodec, Gap: m.Gap, CloseReason: m.Reason,
	}
	id, err := g.db.SegmentClosed(closed, targets)
	path := m.Path
	if errors.Is(err, index.ErrDuplicateSegment) {
		// A closed file with this name is already indexed (its staging copy
		// was removed and the name got reused). Give the new file the next
		// free name so nothing is ever overwritten.
		if newPath, newName, rerr := g.renameToFree(m.Path, m.CameraID, m.Start.Time); rerr == nil {
			g.log.Warn("recording name was already used; renamed", "from", name, "to", newName)
			closed.FileName, path, name = newName, newPath, newName
			id, err = g.db.SegmentClosed(closed, targets)
		} else {
			err = rerr
		}
	}
	if err != nil {
		g.log.Error("could not index the finished recording", "file", m.Path, "error", err.Error())
		g.event(index.Event{Source: "guardian", Type: "index_failed", Severity: ipc.SevCritical, CameraID: m.CameraID,
			Data: map[string]any{"message": "A finished recording could not be added to the index: " + err.Error(), "file": m.Path}})
		return
	}
	if purged {
		g.sendDeletes(ipc.StagingID, []ipc.DeleteItem{{SegmentID: id, CameraID: m.CameraID, FileName: name, Bytes: m.Bytes, Reason: ipc.DeleteCameraDeleted}})
		return
	}
	for _, dest := range targets {
		g.sendMover(&ipc.CopyJob{
			SegmentID: id, CameraID: m.CameraID, FileName: name, SourcePath: path, DestinationID: dest,
			Bytes: m.Bytes, SHA256: m.SHA256, Start: m.Start,
		})
	}
}

// renameToFree renames a staging file to the first name of its hour that is
// neither indexed nor on disk.
func (g *Guardian) renameToFree(path, cameraID string, start time.Time) (newPath, newName string, err error) {
	dir := filepath.Dir(path)
	newName = naming.NextFree(cameraID, start, func(final string) bool {
		if taken, err := g.db.NameTaken(cameraID, final); err != nil || taken {
			return true
		}
		if _, err := os.Stat(filepath.Join(dir, final)); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(dir, naming.PartName(final))); err == nil {
			return true
		}
		return false
	})
	newPath = filepath.Join(dir, newName)
	if err := os.Rename(path, newPath); err != nil {
		return "", "", err
	}
	return newPath, newName, nil
}
