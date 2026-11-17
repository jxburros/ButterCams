package guardian

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mover"
	"github.com/jxburros/buttercams/internal/naming"
)

// Health check thresholds (spec 10.1).
const (
	HealthInterval = 60 * time.Second
	// StreamInterval is how often live state is pushed to browsers.
	StreamInterval = 5 * time.Second
	// PacketAgeLimit is the oldest allowed last packet of a recording camera.
	PacketAgeLimit = 30 * time.Second
	// NotRecordingWarning and NotRecordingCritical are how long an enabled
	// camera may be out of the recording state.
	NotRecordingWarning  = 2 * time.Minute
	NotRecordingCritical = 15 * time.Minute
	// QueueAgeLimit is how old a failing destination's queue may get.
	QueueAgeLimit = 2 * time.Hour
	// StatsAgeLimit is how old a camera's last report may be before the
	// camera is considered not reporting.
	StatsAgeLimit = 20 * time.Second
)

func roundDuration(d time.Duration) string {
	switch {
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
}

// runHealthCheck performs the six self-checks and the on-disk watchdog, and
// records a change of the overall status as an event.
func (g *Guardian) runHealthCheck() {
	now := time.Now()
	cfg := g.config()
	var reasons []api.Reason
	add := func(sev, code, msg, cam string) {
		reasons = append(reasons, api.Reason{Severity: sev, Code: code, Message: msg, CameraID: cam})
	}

	// 1 and 2: cameras.
	type watch struct {
		id, file string
		bytes    int64
		worker   int
	}
	var watching []watch
	g.mu.Lock()
	for _, cam := range cfg.Cameras {
		if !cam.Enabled {
			continue
		}
		live := g.cams[cam.ID]
		if live == nil {
			continue
		}
		reporting := !live.received.IsZero() && now.Sub(live.received) <= StatsAgeLimit
		if reporting && live.stats.State == ipc.StateRecording {
			if age := now.Sub(live.stats.LastPacketTS.Time); live.stats.LastPacketTS.IsZero() || age > PacketAgeLimit {
				add(ipc.SevWarning, "camera_no_packets",
					fmt.Sprintf("%s reports recording but its last packet arrived %s ago.", cam.Name, roundDuration(age)), cam.ID)
			}
			if live.stats.CurrentFile != "" {
				watching = append(watching, watch{cam.ID, live.stats.CurrentFile, live.stats.CurrentFileBytes, live.workerID})
			}
			continue
		}
		since := live.notRecordingSince
		if since.IsZero() {
			since = live.received // stopped reporting while it was recording
		}
		down := now.Sub(since)
		why := "its recorder is not reporting"
		if reporting {
			why = live.stats.State
			if live.stats.Message != "" {
				why += ": " + live.stats.Message
			}
		}
		switch {
		case down >= NotRecordingCritical:
			add(ipc.SevCritical, "camera_not_recording",
				fmt.Sprintf("%s has not recorded for %s (%s).", cam.Name, roundDuration(down), why), cam.ID)
		case down >= NotRecordingWarning:
			add(ipc.SevWarning, "camera_not_recording",
				fmt.Sprintf("%s has not recorded for %s (%s).", cam.Name, roundDuration(down), why), cam.ID)
		}
	}

	// 3: heartbeats and crash loops.
	ids := make([]int, 0, len(g.workers))
	for id := range g.workers {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		w := g.workers[id]
		names := g.cameraNamesLocked(cfg, w.cams)
		switch {
		case w.proc.CrashLoop():
			add(ipc.SevCritical, "worker_crash_loop",
				fmt.Sprintf("Recorder worker %d keeps restarting. Cameras affected: %s.", id, names), "")
		case w.proc.Unresponsive(now):
			add(ipc.SevCritical, "worker_not_responding",
				fmt.Sprintf("Recorder worker %d is not responding and is being restarted. Cameras affected: %s.", id, names), "")
		}
	}
	if m := g.mover; m != nil {
		switch {
		case m.CrashLoop():
			add(ipc.SevCritical, "mover_crash_loop", "The copy process keeps restarting. Recordings stay in staging until it recovers.", "")
		case m.Unresponsive(now):
			add(ipc.SevWarning, "mover_not_responding", "The copy process is not responding and is being restarted. Recording is not affected.", "")
		}
	}

	// 4: staging free space.
	if g.staging.probed && g.staging.reachable {
		floor := mover.StagingFloor(cfg.Staging, g.staging.total)
		if g.staging.free < floor {
			add(ipc.SevCritical, "staging_low",
				fmt.Sprintf("Staging has %s free, below the floor of %s. The oldest staging files are being deleted so recording can continue.",
					humanBytes(g.staging.free), humanBytes(floor)), "")
		}
	} else if g.staging.probed && g.staging.lastError != "" {
		add(ipc.SevCritical, "staging_unreadable", "The staging folder cannot be read: "+g.staging.lastError, "")
	}
	type failing struct{ id, name, err string }
	var failingDests []failing
	for _, d := range cfg.Destinations {
		if live := g.dests[d.ID]; d.Enabled && live != nil && live.lastFailed {
			failingDests = append(failingDests, failing{d.ID, d.Name, live.lastError})
		}
	}
	sticky := append([]api.Reason(nil), g.sticky...)
	g.mu.Unlock()

	// 5: destinations. A failing destination is tolerated while its queue is
	// younger than two hours.
	for _, f := range failingDests {
		n, oldest, err := g.db.Queue(f.id)
		if err != nil || n == 0 || oldest.IsZero() {
			continue
		}
		if age := now.Sub(oldest); age >= QueueAgeLimit {
			add(ipc.SevWarning, "destination_failing",
				fmt.Sprintf("Destination %s has been failing for %s; %d files are waiting in staging. Last error: %s",
					f.name, roundDuration(age), n, f.err), "")
		}
	}

	// 6: database writes.
	if err := g.db.CheckWrite(); err != nil {
		add(ipc.SevCritical, "database_write_failed", "The database does not accept writes: "+err.Error(), "")
	}

	// Live view (spec 16.2): a Warning at most, recording is not affected.
	if r, down := g.mediaHealth(now); down {
		reasons = append(reasons, r)
	}

	reasons = append(reasons, sticky...)

	// 10.2: the recording itself. The file on disk must have grown since the
	// last check even when packets seem to arrive.
	for _, w := range watching {
		g.checkFileGrowth(w.id, w.file, w.worker)
	}

	status := api.StatusHealthy
	for _, r := range reasons {
		if r.Severity == ipc.SevCritical {
			status = api.StatusCritical
			break
		}
		if r.Severity == ipc.SevWarning {
			status = api.StatusWarning
		}
	}
	if reasons == nil {
		reasons = []api.Reason{}
	}
	h := api.Health{Status: status, Reasons: reasons, CheckedAt: now}
	g.mu.Lock()
	prev := g.health.Status
	g.health = h
	g.mu.Unlock()

	if prev != status {
		sev := ipc.SevInfo
		switch status {
		case api.StatusWarning:
			sev = ipc.SevWarning
		case api.StatusCritical:
			sev = ipc.SevCritical
		}
		msgs := make([]string, len(reasons))
		for i, r := range reasons {
			msgs[i] = r.Message
		}
		text := "Status changed from " + prev + " to " + status + "."
		if len(msgs) > 0 {
			text += " " + msgs[0]
			if len(msgs) > 1 {
				text += fmt.Sprintf(" (and %d more)", len(msgs)-1)
			}
		}
		g.event(index.Event{Source: "guardian", Type: "status_changed", Severity: sev,
			Data: map[string]any{"message": text, "from": prev, "to": status, "reasons": msgs}})
		if prev == api.StatusCritical && status != api.StatusCritical {
			g.evlog.Info("ButterCams status is no longer critical: " + status)
		}
	}
	if g.server != nil {
		g.server.PublishHealth(h)
	}
}

func (g *Guardian) cameraNamesLocked(cfg *config.Config, ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		if cam, ok := cfg.Camera(id); ok {
			out += cam.Name
		} else {
			out += id
		}
	}
	return out
}

// checkFileGrowth is the watchdog of spec 10.2: a recording camera whose file
// did not grow since the previous check is treated as stalled; if that does
// not help, its worker is restarted.
func (g *Guardian) checkFileGrowth(camID, file string, workerID int) {
	rel, ok := naming.RelPath(naming.FinalName(file))
	if !ok {
		return
	}
	size := int64(-1)
	for _, root := range g.stagingRoots() {
		if st, err := os.Stat(filepath.Join(root, filepath.Dir(rel), file)); err == nil {
			size = st.Size()
			break
		}
	}
	g.mu.Lock()
	live := g.cams[camID]
	if live == nil {
		g.mu.Unlock()
		return
	}
	grew := live.watchFile != file || size > live.watchBytes
	live.watchFile, live.watchBytes = file, size
	if grew {
		live.watchStrikes = 0
		g.mu.Unlock()
		return
	}
	live.watchStrikes++
	strikes := live.watchStrikes
	w := g.workers[workerID]
	g.mu.Unlock()
	if w == nil {
		return
	}
	name := camID
	if cam, ok := g.config().Camera(camID); ok {
		name = cam.Name
	}
	if strikes == 1 {
		g.event(index.Event{Source: "guardian", Type: "file_not_growing", Severity: ipc.SevWarning, CameraID: camID,
			Data: map[string]any{"message": name + " reports recording but its file on disk is not growing. The camera is being reconnected.", "file": file}})
		w.proc.Send(&ipc.RestartCamera{CameraID: camID, Reason: "the recording file is not growing on disk"})
		return
	}
	g.event(index.Event{Source: "guardian", Type: "file_not_growing", Severity: ipc.SevCritical, CameraID: camID,
		Data: map[string]any{"message": name + "'s file on disk is still not growing. Recorder worker " + strconv.Itoa(workerID) + " is being restarted.", "file": file}})
	g.mu.Lock()
	live.watchStrikes = 0
	g.mu.Unlock()
	w.proc.Kill("a recording file is not growing on disk")
}

// publishLive pushes live state to connected browsers.
func (g *Guardian) publishLive() {
	if g.server != nil {
		g.server.PublishLive()
	}
}

// maintain runs hourly housekeeping: old events, rows of recordings that are
// gone everywhere, old logs and the write-ahead log.
func (g *Guardian) maintain() {
	now := time.Now()
	if n, err := g.db.PruneEvents(now.Add(-index.EventRetention)); err == nil && n > 0 {
		g.log.Info("pruned old events", "count", n)
	}
	if n, err := g.db.PruneGone(now.Add(-24 * time.Hour)); err == nil && n > 0 {
		g.log.Info("pruned index rows of deleted recordings", "count", n)
	}
	g.logs.Prune()
	g.db.Checkpoint(g.ctx)
	g.mu.Lock()
	for id, at := range g.purged {
		if now.Sub(at) > purgeMemory {
			delete(g.purged, id)
		}
	}
	g.mu.Unlock()
}
