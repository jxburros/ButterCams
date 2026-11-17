package guardian

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// UpdateConfig applies mutate to a copy of the configuration, validates and
// saves it, and applies the difference live: only the cameras and
// destinations that changed are touched (spec 5.3).
func (g *Guardian) UpdateConfig(mutate func(*config.Config) error) (*config.Config, error) {
	g.cfgSeq.Lock()
	defer g.cfgSeq.Unlock()

	old := g.Config()
	next := old.Clone()
	if err := mutate(next); err != nil {
		return nil, err
	}
	next.Normalize()
	// Folders shared between destinations or with staging are refused when
	// a change brings them in; ones an older configuration already has are
	// only reported (see config.StorageOverlaps).
	if ov := config.NewStorageOverlaps(old, next, config.Env{IsRemoteDrive: winsvc.IsRemoteDrive}); len(ov) > 0 {
		return nil, &config.ValidationError{Fields: ov}
	}
	if err := g.store.Save(next); err != nil {
		return nil, err
	}
	changes := config.Diff(old, next)

	g.cfgMu.Lock()
	g.cfg = next
	g.cfgMu.Unlock()

	// A successful save confirms the settings that were loaded from a backup.
	g.mu.Lock()
	keep := g.sticky[:0]
	for _, r := range g.sticky {
		if r.Code != "config_recovered" {
			keep = append(keep, r)
		}
	}
	g.sticky = keep
	g.mu.Unlock()

	g.applyChanges(old, next, changes)
	return next.Clone(), nil
}

func (g *Guardian) applyChanges(old, next *config.Config, ch config.Changes) {
	if ch.Empty() {
		return
	}
	g.log.Info("configuration changed",
		"camerasAdded", ch.CamerasAdded, "camerasRemoved", ch.CamerasRemoved, "camerasChanged", ch.CamerasChanged,
		"destinationsChanged", ch.DestinationsChanged, "stagingPathChanged", ch.StagingPathChanged, "serverChanged", ch.ServerChanged,
		"liveChanged", ch.LiveChanged)

	for _, id := range ch.CamerasRemoved {
		g.removeCamera(id)
	}
	for _, id := range ch.CamerasAdded {
		cam, _ := next.Camera(id)
		g.db.UpsertCamera(cam.ID, cam.Name)
		g.placeCamera(cam)
	}
	for _, id := range ch.CamerasChanged {
		cam, _ := next.Camera(id)
		g.db.UpsertCamera(cam.ID, cam.Name)
		if p := g.workerFor(id); p != nil {
			p.Send(&ipc.UpdateCamera{Camera: cam})
		}
	}

	if ch.StagingPathChanged {
		// Files already recorded stay where they are until they are copied
		// or removed; cameras use the new folder from their next file.
		g.mu.Lock()
		known := false
		for _, p := range g.oldStaging {
			known = known || samePath(p, old.Staging.Path)
		}
		if !known {
			g.oldStaging = append(g.oldStaging, old.Staging.Path)
		}
		keep := g.oldStaging[:0]
		for _, p := range g.oldStaging {
			if !samePath(p, next.Staging.Path) {
				keep = append(keep, p)
			}
		}
		g.oldStaging = keep
		g.staging = destLive{}
		g.mu.Unlock()
		g.saveOldStaging()
		for _, w := range g.allWorkers() {
			w.proc.Send(&ipc.SetStaging{StagingPath: next.Staging.Path})
		}
	}

	if ch.DestinationsChanged {
		nextIDs := map[string]config.Destination{}
		for _, d := range next.Destinations {
			nextIDs[d.ID] = d
		}
		for _, d := range old.Destinations {
			nd, still := nextIDs[d.ID]
			switch {
			case !still:
				// Removed: forget its copies. The files on it are left alone.
				g.sendMover(&ipc.CancelCopy{DestinationID: d.ID})
				g.db.DropDestination(d.ID)
				g.mu.Lock()
				delete(g.dests, d.ID)
				delete(g.pendingDel, d.ID)
				g.mu.Unlock()
			case d.Enabled && !nd.Enabled:
				// Disabled: nothing waits for it any more, so staging is not
				// held up by a destination nobody is using.
				g.sendMover(&ipc.CancelCopy{DestinationID: d.ID})
				g.db.CancelPending(d.ID)
			}
		}
		g.mu.Lock()
		for _, d := range next.Destinations {
			if g.dests[d.ID] == nil {
				g.dests[d.ID] = &destLive{}
			}
		}
		g.mu.Unlock()
	}
	if ch.DestinationsChanged || ch.StagingPathChanged || ch.StagingFloorChanged {
		g.sendDestinations()
		if ch.DestinationsChanged {
			g.requeueCopies() // a destination that was fixed or re-enabled gets its backlog
		}
	}

	if ch.LoggingChanged {
		g.logs.SetRetentionDays(next.Logging.RetentionDays)
	}
	if ch.ServerChanged && g.server != nil {
		g.server.Restart(next.Server)
	}

	// Live view (spec 16.8). None of this touches a recording.
	if ch.LiveChanged {
		for _, w := range g.allWorkers() {
			g.sendRestream(w.proc)
		}
		g.syncMedia()
	}
	if ch.LiveChanged || ch.StagingPathChanged || len(ch.CamerasAdded)+len(ch.CamerasRemoved)+len(ch.CamerasChanged) > 0 {
		g.sendAssignMedia()
	}
}

// PurgeCamera deletes every indexed recording of a camera that was deleted
// with "keep recordings" switched off. Only files in the index are touched.
func (g *Guardian) PurgeCamera(cameraID string) {
	at := time.Now()
	g.mu.Lock()
	g.purged[cameraID] = at
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.safely("purge-camera", func() {
			// Give the worker a moment to close the camera's last file; a file
			// that arrives later is caught through g.purged.
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			g.indexMu.Lock()
			defer g.indexMu.Unlock()
			segs, err := g.db.CameraSegments(cameraID)
			if err != nil {
				g.log.Error("could not list the recordings of the deleted camera", "camera", cameraID, "error", err.Error())
				return
			}
			byDest := map[string][]ipc.DeleteItem{}
			for _, s := range segs {
				// A camera added again under the same id meanwhile keeps
				// what it recorded after the deletion.
				if !s.Closed() || !g.isPurged(cameraID, s.Start) {
					continue
				}
				for _, c := range s.Copies {
					switch c.State {
					case index.CopyVerified:
						byDest[c.DestinationID] = append(byDest[c.DestinationID], ipc.DeleteItem{
							SegmentID: s.ID, CameraID: cameraID, FileName: s.FileName, Bytes: s.Bytes, Reason: ipc.DeleteCameraDeleted})
					case index.CopyQueued, index.CopyCopying, index.CopyFailed:
						g.sendMover(&ipc.CancelCopy{SegmentID: s.ID, DestinationID: c.DestinationID})
						g.db.SetCopyState(s.ID, c.DestinationID, index.CopyDeleted, "", false)
					}
				}
			}
			n := 0
			for dest, items := range byDest {
				n += len(items)
				g.sendDeletes(dest, items)
			}
			g.event(index.Event{Source: "guardian", Type: "camera_recordings_deleted", Severity: ipc.SevInfo, CameraID: cameraID,
				Data: map[string]any{"message": fmt.Sprintf("Camera %s was deleted together with its recordings (%d files).", cameraID, n)}})
		})
	}()
}

// OpenSegment opens the nearest available copy of a segment: staging first,
// then local destinations, then network shares (spec 11.3).
func (g *Guardian) OpenSegment(ctx context.Context, id int64) (*os.File, index.Segment, error) {
	seg, err := g.db.Segment(id)
	if err != nil {
		return nil, seg, err
	}
	cands, err := g.segmentCandidates(seg)
	if err != nil {
		return nil, seg, err
	}

	for _, c := range cands {
		// Opening a file on a share that is down can hang for a long time;
		// give each candidate a few seconds and move on.
		type result struct {
			f   *os.File
			err error
		}
		ch := make(chan result, 1)
		go func(c candidate) {
			if c.dest != nil && c.dest.Type == config.DestUNC && c.dest.Username != "" {
				if pw, err := config.Unprotect(c.dest.PasswordEnc); err == nil {
					winsvc.ConnectShare(c.dest.Path, c.dest.Username, pw)
				}
			}
			f, err := winsvc.OpenShared(c.path)
			ch <- result{f, err}
		}(c)
		select {
		case r := <-ch:
			if r.err == nil {
				return r.f, seg, nil
			}
		case <-time.After(8 * time.Second):
			go func() {
				if r := <-ch; r.f != nil {
					r.f.Close()
				}
			}()
		case <-ctx.Done():
			go func() {
				if r := <-ch; r.f != nil {
					r.f.Close()
				}
			}()
			return nil, seg, ctx.Err()
		}
	}
	return nil, seg, os.ErrNotExist
}

// roots lists every place recordings live, for an index rebuild.
func (g *Guardian) roots() []index.Root {
	return Roots(g.config(), g.stagingRoots()[1:])
}

// Roots lists the staging folder(s) and the enabled destinations.
func Roots(cfg *config.Config, oldStaging []string) []index.Root {
	roots := []index.Root{{ID: ipc.StagingID, Path: cfg.Staging.Path}}
	for _, p := range oldStaging {
		roots = append(roots, index.Root{ID: ipc.StagingID, Path: p})
	}
	for _, d := range cfg.Destinations {
		if !d.Enabled {
			continue
		}
		if d.Type == config.DestUNC && d.Username != "" {
			if pw, err := config.Unprotect(d.PasswordEnc); err == nil {
				winsvc.ConnectShare(d.Path, d.Username, pw)
			}
		}
		roots = append(roots, index.Root{ID: d.ID, Path: d.Path})
	}
	return roots
}

// WantDest returns the function that tells a rebuild which destinations a
// camera's recordings belong on.
func WantDest(cfg *config.Config) func(string) []string {
	return func(cameraID string) []string { return copyTargets(cfg, cameraID) }
}

// StartRebuild starts an index rebuild in the background (spec 10.4).
func (g *Guardian) StartRebuild() error {
	return g.startRebuild(true)
}

func (g *Guardian) startRebuild(manual bool) error {
	g.mu.Lock()
	if g.rebuild.Running {
		g.mu.Unlock()
		return errors.New("an index rebuild is already running")
	}
	now := time.Now()
	g.rebuild = api.RebuildStatus{Running: true, StartedAt: &now}
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		var res index.RebuildResult
		var err error
		g.safely("rebuild-index", func() {
			roots := g.roots()
			cfg := g.config()
			g.log.Info("index rebuild started", "roots", len(roots), "manual", manual)
			// The scan runs beside normal operation; only the final update of
			// the tables excludes the message handlers.
			res, err = g.db.Rebuild(g.ctx, roots, WantDest(cfg), func(n int) {
				g.mu.Lock()
				g.rebuild.Segments = n
				g.mu.Unlock()
			})
		})
		done := time.Now()
		g.mu.Lock()
		g.rebuild.Running = false
		g.rebuild.FinishedAt = &done
		g.rebuild.Segments = res.Segments
		if err != nil {
			g.rebuild.Error = err.Error()
		}
		g.mu.Unlock()
		if err != nil {
			g.log.Error("index rebuild failed", "error", err.Error())
			g.event(index.Event{Source: "guardian", Type: "index_rebuild_failed", Severity: ipc.SevWarning,
				Data: map[string]any{"message": "Rebuilding the index failed: " + err.Error()}})
			return
		}
		msg := fmt.Sprintf("The index was rebuilt from disk: %d recordings in %d files.", res.Segments, res.Files)
		if len(res.Offline) > 0 {
			msg += fmt.Sprintf(" %d location(s) could not be reached and were left as they were.", len(res.Offline))
		}
		if len(res.Skipped) > 0 {
			msg += fmt.Sprintf(" %d file(s) could not be read.", len(res.Skipped))
		}
		g.log.Info("index rebuild finished", "segments", res.Segments, "files", res.Files, "offline", res.Offline, "skipped", len(res.Skipped))
		if manual || res.Segments > 0 {
			g.event(index.Event{Source: "guardian", Type: "index_rebuilt", Severity: ipc.SevInfo,
				Data: map[string]any{"message": msg, "segments": res.Segments, "files": res.Files}})
		}
		g.requeueCopies() // staging files that still have to go somewhere
	}()
	return nil
}

// Rebuild returns the state of the last index rebuild.
func (g *Guardian) Rebuild() api.RebuildStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rebuild
}
