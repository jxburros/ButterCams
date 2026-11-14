package mover

import (
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
)

// Retention thresholds (spec 8).
const (
	// CapTarget is the fraction of the cap usage is brought down to once the
	// cap is exceeded.
	CapTarget = 0.95
	// DiskFullPercent is the free-space percentage below which a destination
	// is treated as if its cap were hit.
	DiskFullPercent = 2.0
	// RetentionInterval is how often free space is measured and retention
	// runs when nothing else triggers it.
	RetentionInterval = 5 * time.Minute
)

// File is an indexed file on a destination, as far as retention cares.
type File struct {
	SegmentID int64
	CameraID  string
	FileName  string
	Start     time.Time
	Bytes     int64
	Protected bool
}

// Params are the inputs to a destination's retention decision.
type Params struct {
	Now      time.Time
	CapBytes int64         // 0 = no cap
	MaxAge   time.Duration // 0 = no age limit

	// DiskKnown is true when FreeBytes and TotalBytes were measured.
	DiskKnown  bool
	FreeBytes  uint64
	TotalBytes uint64
}

func deleteItem(f File, reason string) ipc.DeleteItem {
	return ipc.DeleteItem{SegmentID: f.SegmentID, CameraID: f.CameraID, FileName: f.FileName, Bytes: f.Bytes, Reason: reason}
}

// SelectDestination decides which indexed files to delete from a destination.
// files must be sorted oldest first (by start, across all cameras). Protected
// files are never selected.
//
//   - Max age: every file older than MaxAge goes.
//   - Size cap: when the indexed files exceed the cap, the oldest go until
//     usage is at or below 95% of the cap.
//   - Disk full: when the volume has less than 2% free, the destination is
//     treated as if its cap were its current usage, so 5% of what ButterCams
//     stores there is freed even though the cap was not reached.
func SelectDestination(files []File, p Params) []ipc.DeleteItem {
	var out []ipc.DeleteItem
	selected := make([]bool, len(files))
	var used int64
	for _, f := range files {
		used += f.Bytes
	}

	if p.MaxAge > 0 {
		cutoff := p.Now.Add(-p.MaxAge)
		for i, f := range files {
			if !f.Protected && f.Start.Before(cutoff) {
				selected[i] = true
				used -= f.Bytes
				out = append(out, deleteItem(f, ipc.DeleteMaxAge))
			}
		}
	}

	trim := func(target int64, reason string) {
		for i, f := range files {
			if used <= target {
				return
			}
			if selected[i] || f.Protected {
				continue
			}
			selected[i] = true
			used -= f.Bytes
			out = append(out, deleteItem(f, reason))
		}
	}

	if p.CapBytes > 0 && used > p.CapBytes {
		trim(int64(float64(p.CapBytes)*CapTarget), ipc.DeleteCap)
	}
	if p.DiskKnown && p.TotalBytes > 0 &&
		float64(p.FreeBytes) < float64(p.TotalBytes)*DiskFullPercent/100 && used > 0 {
		trim(int64(float64(used)*CapTarget), ipc.DeleteDiskFull)
	}
	return out
}

// StagingFloor returns the free space staging must keep: the larger of
// minFreePercent of the volume and minFreeGB (spec 8).
func StagingFloor(st config.Staging, totalBytes uint64) uint64 {
	byPercent := uint64(float64(totalBytes) * st.MinFreePercent / 100)
	byGB := uint64(st.MinFreeGB * (1 << 30))
	if byPercent > byGB {
		return byPercent
	}
	return byGB
}

// SelectStagingFloor picks the oldest staging files to delete so that free
// space gets back above the floor, whether or not they were copied yet.
// Recording must never stop for lack of space. files must be sorted oldest
// first and must not include the files currently being written.
func SelectStagingFloor(files []File, freeBytes, floorBytes uint64) []ipc.DeleteItem {
	var out []ipc.DeleteItem
	free := freeBytes
	for _, f := range files {
		if free >= floorBytes {
			break
		}
		if f.Protected {
			continue
		}
		out = append(out, deleteItem(f, ipc.DeleteStagingFloor))
		free += uint64(f.Bytes)
	}
	return out
}
