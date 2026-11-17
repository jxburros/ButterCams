package guardian

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mover"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

const (
	metaOldStaging = "old_staging_paths"
	// deleteChunk bounds the size of one retention message.
	deleteChunk = 500
	// purgeMemory is how long a deleted camera's late files are still removed.
	purgeMemory = 10 * time.Minute
)

// stagingHold is how long after the start of its hour a copied file stays in
// staging. Zero: a file leaves staging as soon as every destination has it.
// (Names are protected against reuse by the index, not by keeping files.)
var stagingHold time.Duration

// OldStagingPaths returns the earlier staging folders remembered in the
// index, which may still hold recordings (spec 7.1).
func OldStagingPaths(db *index.DB) []string {
	var paths []string
	if v, ok := db.Meta(metaOldStaging); ok {
		json.Unmarshal([]byte(v), &paths)
	}
	return paths
}

func (g *Guardian) loadOldStaging() {
	if v, ok := g.db.Meta(metaOldStaging); ok {
		var paths []string
		if json.Unmarshal([]byte(v), &paths) == nil {
			g.oldStaging = paths
		}
	}
}

func (g *Guardian) saveOldStaging() {
	g.mu.Lock()
	b, _ := json.Marshal(g.oldStaging)
	g.mu.Unlock()
	if err := g.db.SetMeta(metaOldStaging, string(b)); err != nil {
		g.log.Error("could not remember the previous staging folder", "error", err.Error())
	}
}

// isPurged reports whether a recording belongs to a camera that was deleted
// together with its recordings: one that started before the deletion, or any
// file while the camera is still deleted (its worker may start one more file
// before the removal reaches it). A camera added again under the same id
// keeps what it records.
func (g *Guardian) isPurged(cameraID string, start time.Time) bool {
	g.mu.Lock()
	at, ok := g.purged[cameraID]
	g.mu.Unlock()
	if !ok {
		return false
	}
	if start.Before(at) {
		return true
	}
	_, configured := g.config().Camera(cameraID)
	return !configured
}

func (g *Guardian) startMover() {
	p := newProc(ProcSpec{
		Name:             "mover",
		Exe:              g.exe,
		Args:             []string{"mover"},
		Env:              []string{winsvc.EnvDataRoot + "=" + g.opts.DataRoot},
		HeartbeatTimeout: MoverHeartbeatTimeout,
		OnStart:          func(p *Proc) { g.onMoverStart() },
		OnMessage:        func(p *Proc, msg any) { g.onMoverMessage(msg) },
		OnRestart: func(p *Proc, reason string, loop bool) {
			sev, typ := ipc.SevWarning, "mover_restarted"
			if loop {
				sev, typ = ipc.SevCritical, "mover_crash_loop"
			}
			g.event(index.Event{Source: "mover", Type: typ, Severity: sev,
				Data: map[string]any{"message": "The copy process was restarted (" + reason + "). Recording is not affected.", "reason": reason}})
		},
	}, g.log, g.childLog("mover"))
	p.job = g.job
	g.mu.Lock()
	g.mover = p
	g.mu.Unlock()
	p.Start()
}

func (g *Guardian) sendMover(msg any) {
	g.mu.Lock()
	p := g.mover
	g.mu.Unlock()
	if p != nil {
		p.Send(msg)
	}
}

// sendDestinations gives the mover the current storage configuration.
func (g *Guardian) sendDestinations() {
	cfg := g.config()
	g.mu.Lock()
	old := append([]string(nil), g.oldStaging...)
	g.mu.Unlock()
	g.sendMover(&ipc.Destinations{Staging: cfg.Staging, OldStagingPaths: old, Destinations: cfg.Destinations})
}

// onMoverStart brings a (re)started mover up to date: the destinations, then
// every copy still owed according to the database. This is how the queue
// survives a mover crash, a guardian restart or a reboot (spec 9).
func (g *Guardian) onMoverStart() {
	g.mu.Lock()
	g.pendingDel = map[string]map[int64]int64{} // deletes in flight were lost with the old process
	g.mu.Unlock()
	g.sendDestinations()
	g.requeueCopies()
}

// requeueCopies hands every pending copy in the database to the mover.
func (g *Guardian) requeueCopies() {
	pending, err := g.db.PendingCopies()
	if err != nil {
		g.log.Error("could not read the copy queue", "error", err.Error())
		return
	}
	cfg := g.config()
	for _, pc := range pending {
		d, ok := cfg.Destination(pc.DestinationID)
		if !ok {
			continue
		}
		if !d.Enabled {
			continue
		}
		src, found := g.findStaging(pc.Segment.FileName)
		if !found {
			// The staging file is gone: this copy can never be made.
			g.db.SetCopyState(pc.Segment.ID, ipc.StagingID, index.CopyDeleted, "", false)
			g.db.FailPendingCopies(pc.Segment.ID, "the staging file no longer exists")
			continue
		}
		g.sendMover(&ipc.CopyJob{
			SegmentID: pc.Segment.ID, CameraID: pc.Segment.CameraID, FileName: pc.Segment.FileName, SourcePath: src,
			DestinationID: pc.DestinationID, Bytes: pc.Segment.Bytes, SHA256: pc.Segment.SHA256, Start: ipc.T(pc.Segment.Start),
		})
	}
}

func (g *Guardian) onMoverMessage(msg any) {
	switch m := msg.(type) {
	case *ipc.CopyResult:
		g.indexMu.Lock()
		defer g.indexMu.Unlock()
		g.onCopyResult(m)
	case *ipc.DiskStatus:
		g.indexMu.Lock()
		defer g.indexMu.Unlock()
		g.onDiskStatus(m)
	case *ipc.RetentionResult:
		g.indexMu.Lock()
		defer g.indexMu.Unlock()
		g.onRetentionResult(m)
	case *ipc.Event:
		data := map[string]any{"message": m.Message}
		for k, v := range m.Data {
			data[k] = v
		}
		g.event(index.Event{TS: m.TS.Time, Source: "mover", Type: m.Code, Severity: m.Severity, CameraID: m.CameraID, Data: data})
	}
}

func (g *Guardian) destLive(id string) *destLive {
	d := g.dests[id]
	if d == nil {
		d = &destLive{}
		g.dests[id] = d
	}
	return d
}

func (g *Guardian) onCopyResult(m *ipc.CopyResult) {
	now := time.Now()
	if m.OK {
		if err := g.db.SetCopyState(m.SegmentID, m.DestinationID, index.CopyVerified, "", true); err != nil {
			g.log.Error("could not record the verified copy", "error", err.Error())
		}
		g.mu.Lock()
		d := g.destLive(m.DestinationID)
		recovered := d.lastFailed
		d.lastFailed, d.lastError, d.lastSuccess = false, "", now
		g.mu.Unlock()
		if recovered {
			g.event(index.Event{Source: "mover", Type: "destination_recovered", Severity: ipc.SevInfo,
				Data: map[string]any{"message": "Destination " + g.destName(m.DestinationID) + " is reachable again; the backlog is being copied.",
					"destinationId": m.DestinationID}})
		}
		// A recording of a camera that was deleted meanwhile is removed again.
		if seg, err := g.db.Segment(m.SegmentID); err == nil {
			if g.isPurged(seg.CameraID, seg.Start) {
				g.sendDeletes(m.DestinationID, []ipc.DeleteItem{{SegmentID: seg.ID, CameraID: seg.CameraID,
					FileName: seg.FileName, Bytes: seg.Bytes, Reason: ipc.DeleteCameraDeleted}})
			}
		}
		return
	}
	if m.SourceMissing {
		g.db.SetCopyState(m.SegmentID, ipc.StagingID, index.CopyDeleted, "", false)
		g.db.FailPendingCopies(m.SegmentID, m.Error)
		return
	}
	if err := g.db.SetCopyState(m.SegmentID, m.DestinationID, index.CopyFailed, m.Error, true); err != nil {
		g.log.Error("could not record the failed copy", "error", err.Error())
	}
	g.mu.Lock()
	d := g.destLive(m.DestinationID)
	first := !d.lastFailed
	d.lastFailed, d.lastError = true, m.Error
	g.mu.Unlock()
	if first {
		g.event(index.Event{Source: "mover", Type: "destination_unreachable", Severity: ipc.SevWarning,
			Data: map[string]any{"message": "Copying to " + g.destName(m.DestinationID) + " failed: " + m.Error +
				". Files are kept in staging and the copy is retried. Recording is not affected.",
				"destinationId": m.DestinationID, "error": m.Error}})
	}
}

func (g *Guardian) destName(id string) string {
	if d, ok := g.config().Destination(id); ok {
		return d.Name
	}
	return id
}

// onDiskStatus records free space and runs retention for that root.
func (g *Guardian) onDiskStatus(m *ipc.DiskStatus) {
	cfg := g.config()
	if m.DestinationID == ipc.StagingID {
		g.mu.Lock()
		g.staging.probed, g.staging.reachable = true, m.Reachable
		g.staging.free, g.staging.total = m.FreeBytes, m.TotalBytes
		g.staging.lastError = m.Error
		g.mu.Unlock()
		g.stagingRetention(cfg, m)
		return
	}
	dest, ok := cfg.Destination(m.DestinationID)
	if !ok {
		return
	}
	g.mu.Lock()
	d := g.destLive(m.DestinationID)
	d.probed, d.reachable = true, m.Reachable
	if m.Reachable {
		d.free, d.total = m.FreeBytes, m.TotalBytes
	} else if m.Error != "" && !d.lastFailed {
		d.lastError = m.Error
	}
	g.mu.Unlock()
	if !m.Reachable || !dest.Enabled {
		return // nothing can be deleted on a share that does not answer
	}

	stored, err := g.db.StoredFiles(dest.ID)
	if err != nil {
		g.log.Error("retention could not list the files", "destination", dest.ID, "error", err.Error())
		return
	}
	p := mover.Params{
		Now:       time.Now(),
		CapBytes:  int64(dest.CapGB * (1 << 30)),
		DiskKnown: true, FreeBytes: m.FreeBytes, TotalBytes: m.TotalBytes,
	}
	if dest.MaxAgeDays != nil {
		p.MaxAge = time.Duration(*dest.MaxAgeDays) * 24 * time.Hour
	}
	g.sendDeletes(dest.ID, mover.SelectDestination(g.notPending(dest.ID, stored), p))
}

// notPending converts indexed files to retention input, leaving out files
// whose deletion is already under way.
func (g *Guardian) notPending(dest string, stored []index.StoredFile) []mover.File {
	g.mu.Lock()
	defer g.mu.Unlock()
	pend := g.pendingDel[dest]
	out := make([]mover.File, 0, len(stored))
	for _, f := range stored {
		if _, inFlight := pend[f.SegmentID]; inFlight {
			continue
		}
		out = append(out, mover.File{SegmentID: f.SegmentID, CameraID: f.CameraID, FileName: f.FileName,
			Start: f.Start, Bytes: f.Bytes, Protected: f.Protected})
	}
	return out
}

// stagingRetention removes staging copies that are safely stored on every
// destination, and enforces the staging floor (spec 7.2 step 3 and 8).
func (g *Guardian) stagingRetention(cfg *config.Config, m *ipc.DiskStatus) {
	now := time.Now()
	freed := uint64(0)
	// Deletes sent earlier and not yet reported free space soon too. Without
	// them a status that arrives before they finish would pick more files,
	// including ones that were never copied.
	g.mu.Lock()
	for _, b := range g.pendingDel[ipc.StagingID] {
		if b > 0 {
			freed += uint64(b)
		}
	}
	g.mu.Unlock()
	if copied, err := g.db.CopiedEverywhere(); err == nil {
		var items []ipc.DeleteItem
		for _, f := range g.notPending(ipc.StagingID, copied) {
			if p, ok := naming.Parse(f.FileName); ok && now.Before(p.HourStart.Add(stagingHold)) {
				continue
			}
			items = append(items, ipc.DeleteItem{SegmentID: f.SegmentID, CameraID: f.CameraID, FileName: f.FileName,
				Bytes: f.Bytes, Reason: ipc.DeleteCopied})
			freed += uint64(f.Bytes)
		}
		g.sendDeletes(ipc.StagingID, items)
	}
	if !m.Reachable || m.TotalBytes == 0 {
		return
	}
	floor := mover.StagingFloor(cfg.Staging, m.TotalBytes)
	if m.FreeBytes+freed >= floor {
		return
	}
	stored, err := g.db.StoredFiles(ipc.StagingID)
	if err != nil {
		g.log.Error("staging retention could not list the files", "error", err.Error())
		return
	}
	items := mover.SelectStagingFloor(g.notPending(ipc.StagingID, stored), m.FreeBytes+freed, floor)
	if len(items) == 0 {
		return
	}
	names := make([]string, len(items))
	var bytes int64
	for i, it := range items {
		names[i] = it.FileName
		bytes += it.Bytes
	}
	g.event(index.Event{Source: "guardian", Type: "staging_floor", Severity: ipc.SevCritical,
		Data: map[string]any{
			"message": fmt.Sprintf("Staging is nearly full (%s free, floor %s). To keep recording, %d of the oldest staging files were deleted: %s.",
				humanBytes(m.FreeBytes), humanBytes(floor), len(items), strings.Join(names, ", ")),
			"files": names, "bytes": bytes, "freeBytes": m.FreeBytes, "floorBytes": floor}})
	g.sendDeletes(ipc.StagingID, items)
}

func humanBytes(n uint64) string {
	const gb = 1 << 30
	if n >= gb {
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
}

// sendDeletes sends a retention job to the mover in chunks and remembers
// which deletes are in flight.
func (g *Guardian) sendDeletes(dest string, items []ipc.DeleteItem) {
	if len(items) == 0 {
		return
	}
	g.mu.Lock()
	pend := g.pendingDel[dest]
	if pend == nil {
		pend = map[int64]int64{}
		g.pendingDel[dest] = pend
	}
	for _, it := range items {
		pend[it.SegmentID] = it.Bytes
	}
	g.mu.Unlock()
	for len(items) > 0 {
		n := len(items)
		if n > deleteChunk {
			n = deleteChunk
		}
		g.sendMover(&ipc.RetentionJob{DestinationID: dest, Deletes: items[:n]})
		items = items[n:]
	}
}

func (g *Guardian) onRetentionResult(m *ipc.RetentionResult) {
	for _, r := range m.Results {
		g.mu.Lock()
		delete(g.pendingDel[m.DestinationID], r.SegmentID)
		g.mu.Unlock()
		if !r.Deleted && !r.Missing {
			g.log.Warn("a recording could not be deleted", "destination", m.DestinationID, "file", r.FileName, "error", r.Error)
			continue
		}
		if err := g.db.SetCopyState(r.SegmentID, m.DestinationID, index.CopyDeleted, "", false); err != nil {
			g.log.Error("could not record the deletion", "error", err.Error())
		}
		g.log.Info("recording deleted", "destination", m.DestinationID, "path", r.Path, "bytes", r.Bytes,
			"reason", r.Reason, "alreadyGone", r.Missing)
		if m.DestinationID == ipc.StagingID && r.Reason == ipc.DeleteStagingFloor {
			// Copies that were still waiting for this file can never be made.
			g.db.FailPendingCopies(r.SegmentID, "the staging copy was deleted to keep recording (staging floor)")
			g.sendMover(&ipc.CancelCopy{SegmentID: r.SegmentID})
		}
	}
}

// Storage returns the live state of the destinations and of staging.
func (g *Guardian) Storage() (map[string]api.DestinationStatus, api.StagingStatus) {
	cfg := g.config()
	out := map[string]api.DestinationStatus{}
	for _, d := range cfg.Destinations {
		st := api.DestinationStatus{State: "ok"}
		st.UsedBytes, _, _ = g.db.Usage(d.ID)
		n, oldest, _ := g.db.Queue(d.ID)
		st.QueueLength = n
		if n > 0 && !oldest.IsZero() {
			o := oldest
			st.OldestQueued = &o
		}
		g.mu.Lock()
		if live := g.dests[d.ID]; live != nil {
			st.LastError = live.lastError
			if live.lastFailed {
				st.State = "retrying"
			}
			if live.probed && live.reachable {
				f, t := live.free, live.total
				st.FreeBytes, st.TotalBytes = &f, &t
			}
			if !live.lastSuccess.IsZero() {
				ls := live.lastSuccess
				st.LastSuccessTS = &ls
			}
			if live.probed && !live.reachable && n > 0 {
				st.State = "retrying"
			}
		}
		g.mu.Unlock()
		if !d.Enabled {
			st.State = "disabled"
		}
		out[d.ID] = st
	}
	sg := api.StagingStatus{Path: cfg.Staging.Path, MinFreePercent: cfg.Staging.MinFreePercent, MinFreeGB: cfg.Staging.MinFreeGB}
	sg.UsedBytes, _, _ = g.db.Usage(ipc.StagingID)
	g.mu.Lock()
	if g.staging.probed && g.staging.reachable {
		f, t := g.staging.free, g.staging.total
		fl := mover.StagingFloor(cfg.Staging, t)
		sg.FreeBytes, sg.TotalBytes, sg.FloorBytes = &f, &t, &fl
	}
	g.mu.Unlock()
	return out, sg
}
