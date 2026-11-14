package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
)

// Root is one place recordings live: the staging folder (id "staging") or a
// destination.
type Root struct {
	ID   string
	Path string
}

// RebuildResult summarises a rebuild.
type RebuildResult struct {
	Segments int      // segments found
	Files    int      // files found (a segment may exist in several roots)
	Skipped  []string // files that matched the naming pattern but could not be read
	Offline  []string // roots that could not be listed
}

type foundFile struct {
	info  mp4meta.Info
	roots []string
}

// Scan walks the roots and reads the metadata of every file that follows the
// ButterCams layout <root>/<camera-id>/<YYYY-MM-DD>/<name>.mp4. Files that do
// not match the naming pattern are never opened. In-progress .part.mp4 files
// are left to the recorder, which repairs and reports them itself.
func Scan(ctx context.Context, roots []Root, progress func(files int)) (map[[2]string]*foundFile, RebuildResult, error) {
	found := map[[2]string]*foundFile{}
	var res RebuildResult
	for _, root := range roots {
		cams, err := os.ReadDir(root.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				res.Offline = append(res.Offline, root.ID)
			}
			continue
		}
		for _, cam := range cams {
			if !cam.IsDir() || !naming.ValidID(cam.Name()) {
				continue
			}
			days, err := os.ReadDir(filepath.Join(root.Path, cam.Name()))
			if err != nil {
				continue
			}
			for _, day := range days {
				if !day.IsDir() {
					continue
				}
				if _, err := time.Parse("2006-01-02", day.Name()); err != nil {
					continue
				}
				dir := filepath.Join(root.Path, cam.Name(), day.Name())
				files, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, f := range files {
					if err := ctx.Err(); err != nil {
						return nil, res, err
					}
					p, ok := naming.Parse(f.Name())
					if !ok || p.Part || f.IsDir() || p.CameraID != cam.Name() || p.Date != day.Name() {
						continue
					}
					path := filepath.Join(dir, f.Name())
					info, err := mp4meta.Read(path)
					if err != nil || info.Dur90k == 0 {
						res.Skipped = append(res.Skipped, path)
						continue
					}
					if info.CameraID == "" {
						info.CameraID = p.CameraID
					}
					if info.Start.IsZero() {
						info.Start = p.HourStart
					}
					res.Files++
					key := [2]string{p.CameraID, f.Name()}
					if ff := found[key]; ff != nil {
						ff.roots = append(ff.roots, root.ID)
					} else {
						found[key] = &foundFile{info: info, roots: []string{root.ID}}
					}
					if progress != nil {
						progress(res.Files)
					}
				}
			}
		}
	}
	res.Segments = len(found)
	return found, res, nil
}

// Rebuild brings the segments and copies tables in line with what is actually
// on disk in the given roots (spec 10.4). On an empty database this recreates
// the whole index. For every file found it restores the name, start, end,
// size, codecs, gap flag and close reason, which the files carry themselves;
// the SHA-256 is recomputed the next time a file is copied.
//
// It is safe to run while recording continues:
//   - files still being written are not on its list and their rows are kept;
//   - rows touched after the scan began are left alone;
//   - a root that cannot be listed (an offline share) is reported in the
//     result and what the index says about it is kept;
//   - copies that are waiting to be made stay queued.
//
// wantDest returns the destinations a camera's recordings belong on. A file
// found in staging that is missing from one of them is queued for copying,
// which restores the copy queue after the database was lost.
func (d *DB) Rebuild(ctx context.Context, roots []Root, wantDest func(cameraID string) []string, progress func(files int)) (RebuildResult, error) {
	scanStart := time.Now()
	found, res, err := Scan(ctx, roots, progress)
	if err != nil {
		return res, err
	}
	offline := map[string]bool{}
	for _, id := range res.Offline {
		offline[id] = true
	}
	scanned := map[string]bool{}
	for _, r := range roots {
		if !offline[r.ID] {
			scanned[r.ID] = true
		}
	}

	keys := make([][2]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := found[keys[i]].info, found[keys[j]].info
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})

	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	now := fmtTime(time.Now())
	cutoff := fmtTime(scanStart)

	// 1. Copies the index believes in that are no longer on disk.
	rows, err := tx.Query(`SELECT c.segment_id, c.destination_id, s.camera_id, s.file_name
		FROM copies c JOIN segments s ON s.id = c.segment_id
		WHERE c.state = 'verified' AND s.end_ts IS NOT NULL AND julianday(c.updated_ts) < julianday(?)`, cutoff)
	if err != nil {
		return res, err
	}
	type lost struct {
		seg  int64
		dest string
	}
	var gone []lost
	for rows.Next() {
		var l lost
		var cam, file string
		if err := rows.Scan(&l.seg, &l.dest, &cam, &file); err != nil {
			rows.Close()
			return res, err
		}
		if !scanned[l.dest] {
			continue
		}
		ff := found[[2]string{cam, file}]
		present := false
		if ff != nil {
			for _, r := range ff.roots {
				present = present || r == l.dest
			}
		}
		if !present {
			gone = append(gone, l)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, l := range gone {
		if _, err := tx.Exec(`UPDATE copies SET state = 'deleted', updated_ts = ? WHERE segment_id = ? AND destination_id = ?`,
			now, l.seg, l.dest); err != nil {
			return res, err
		}
	}

	// 2. Every file found on disk.
	for _, k := range keys {
		ff := found[k]
		info := ff.info
		if err := d.ensureCamera(tx, k[0]); err != nil {
			return res, err
		}
		start := info.Start.In(time.Local)
		end := mp4meta.EndOf(start, info.Dur90k)
		var id int64
		var bytes sql.NullInt64
		err := tx.QueryRow(`SELECT id, bytes FROM segments WHERE camera_id = ? AND file_name = ?`, k[0], k[1]).Scan(&id, &bytes)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			r, err := tx.Exec(`INSERT INTO segments(camera_id, file_name, start_ts, end_ts, bytes, video_codec, audio_codec, gap_before, close_reason)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				k[0], k[1], fmtTime(start), fmtTime(end), info.Size, info.VideoCodec, info.AudioCodec, info.Gap, nullString(info.CloseReason))
			if err != nil {
				return res, fmt.Errorf("indexing %s: %w", k[1], err)
			}
			if id, err = r.LastInsertId(); err != nil {
				return res, err
			}
		case err != nil:
			return res, err
		default:
			// Keep the stored SHA-256 only when the file is still the same size.
			keepHash := bytes.Valid && bytes.Int64 == info.Size
			if _, err := tx.Exec(`UPDATE segments SET start_ts = ?, end_ts = ?, bytes = ?, video_codec = ?, audio_codec = ?,
				gap_before = ?, close_reason = ?, sha256 = CASE WHEN ? THEN sha256 ELSE NULL END WHERE id = ?`,
				fmtTime(start), fmtTime(end), info.Size, info.VideoCodec, info.AudioCodec, info.Gap,
				nullString(info.CloseReason), keepHash, id); err != nil {
				return res, err
			}
		}
		inStaging := false
		have := map[string]bool{}
		for _, root := range ff.roots {
			have[root] = true
			inStaging = inStaging || root == ipc.StagingID
			if _, err := tx.Exec(`INSERT INTO copies(segment_id, destination_id, state, updated_ts) VALUES(?, ?, 'verified', ?)
				ON CONFLICT(segment_id, destination_id) DO UPDATE SET state = 'verified', last_error = NULL,
					updated_ts = CASE WHEN copies.state = 'verified' THEN copies.updated_ts ELSE excluded.updated_ts END`,
				id, root, now); err != nil {
				return res, err
			}
		}
		// 3. Restore the copy queue for staging files that still have to go
		// to a destination.
		if inStaging && wantDest != nil {
			for _, dest := range wantDest(k[0]) {
				if have[dest] || dest == ipc.StagingID {
					continue
				}
				if _, err := tx.Exec(`INSERT OR IGNORE INTO copies(segment_id, destination_id, state, updated_ts) VALUES(?, ?, 'queued', ?)`,
					id, dest, now); err != nil {
					return res, err
				}
			}
		}
	}
	return res, tx.Commit()
}
