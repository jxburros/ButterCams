package mover

import (
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
)

var t0 = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

// hours builds one 100-byte file per hour, oldest first, alternating cameras.
func hours(n int) []File {
	out := make([]File, n)
	for i := range out {
		cam := "a"
		if i%2 == 1 {
			cam = "b"
		}
		out[i] = File{SegmentID: int64(i + 1), CameraID: cam, FileName: cam, Start: t0.Add(time.Duration(i) * time.Hour), Bytes: 100}
	}
	return out
}

func ids(items []ipc.DeleteItem) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.SegmentID
	}
	return out
}

func equal(a []int64, b ...int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUnderCapDeletesNothing(t *testing.T) {
	got := SelectDestination(hours(10), Params{Now: t0.Add(10 * time.Hour), CapBytes: 1000})
	if len(got) != 0 {
		t.Fatalf("deleted %v while exactly at the cap", ids(got))
	}
}

func TestOverCapDeletesOldestDownTo95Percent(t *testing.T) {
	// 12 files × 100 = 1200 bytes, cap 1000 → must get to ≤ 950: delete 3.
	got := SelectDestination(hours(12), Params{Now: t0.Add(12 * time.Hour), CapBytes: 1000})
	if !equal(ids(got), 1, 2, 3) {
		t.Fatalf("deleted %v, want the three oldest across both cameras", ids(got))
	}
	for _, it := range got {
		if it.Reason != ipc.DeleteCap {
			t.Fatalf("reason %q", it.Reason)
		}
	}
}

func TestProtectedFilesSurviveRetention(t *testing.T) {
	files := hours(12)
	files[0].Protected = true
	files[2].Protected = true
	got := SelectDestination(files, Params{Now: t0.Add(12 * time.Hour), CapBytes: 1000, MaxAge: time.Hour})
	for _, it := range got {
		if it.SegmentID == 1 || it.SegmentID == 3 {
			t.Fatalf("a protected file was selected: %v", ids(got))
		}
	}
	// Even at a full cap with everything else gone, they stay.
	got = SelectDestination(files, Params{Now: t0.Add(12 * time.Hour), CapBytes: 1})
	if len(got) != 10 {
		t.Fatalf("selected %d files, want every unprotected one (10)", len(got))
	}
}

func TestMaxAge(t *testing.T) {
	// Files start at hours 0..9; at hour 10 with a 6 h limit, 0..3 are too old.
	got := SelectDestination(hours(10), Params{Now: t0.Add(10 * time.Hour), MaxAge: 6 * time.Hour})
	if !equal(ids(got), 1, 2, 3, 4) {
		t.Fatalf("deleted %v", ids(got))
	}
	if got[0].Reason != ipc.DeleteMaxAge {
		t.Fatalf("reason %q", got[0].Reason)
	}
}

func TestMaxAgeAndCapTogetherDoNotDoubleCount(t *testing.T) {
	// 1200 bytes; age removes 2 (→1000, not over cap 1000), so cap removes none.
	got := SelectDestination(hours(12), Params{Now: t0.Add(12 * time.Hour), CapBytes: 1000, MaxAge: 10*time.Hour + time.Minute})
	if !equal(ids(got), 1, 2) {
		t.Fatalf("deleted %v", ids(got))
	}
	// Tighter cap: age removes 2, cap removes more of the oldest remaining.
	got = SelectDestination(hours(12), Params{Now: t0.Add(12 * time.Hour), CapBytes: 800, MaxAge: 10*time.Hour + time.Minute})
	if !equal(ids(got), 1, 2, 3, 4, 5) { // 1000 → ≤ 760 needs 3 more
		t.Fatalf("deleted %v", ids(got))
	}
}

func TestDiskFullActsAsIfCapWereHit(t *testing.T) {
	p := Params{Now: t0.Add(20 * time.Hour), CapBytes: 1_000_000, DiskKnown: true, TotalBytes: 10000, FreeBytes: 150}
	got := SelectDestination(hours(20), p) // 1.5% free, far below the cap
	if !equal(ids(got), 1) {               // 2000 bytes → ≤ 1900
		t.Fatalf("deleted %v, want the oldest file", ids(got))
	}
	if got[0].Reason != ipc.DeleteDiskFull {
		t.Fatalf("reason %q", got[0].Reason)
	}
	p.FreeBytes = 300 // 3% free: fine
	if got := SelectDestination(hours(20), p); len(got) != 0 {
		t.Fatalf("deleted %v with enough free space", ids(got))
	}
	p.DiskKnown = false
	p.FreeBytes = 0
	if got := SelectDestination(hours(20), p); len(got) != 0 {
		t.Fatal("unknown disk size must not trigger deletions")
	}
}

func TestStagingFloorUsesWhicheverLeavesMoreFree(t *testing.T) {
	st := config.Staging{MinFreePercent: 10, MinFreeGB: 50}
	if got := StagingFloor(st, 1000<<30); got != 100<<30 { // 10% of 1000 GB > 50 GB
		t.Fatalf("floor %d", got)
	}
	if got := StagingFloor(st, 200<<30); got != 50<<30 { // 10% of 200 GB < 50 GB
		t.Fatalf("floor %d", got)
	}
}

func TestStagingFloorDeletesOldestUntilAboveFloor(t *testing.T) {
	files := hours(10)
	got := SelectStagingFloor(files, 500, 750) // needs 250 more → 3 files
	if !equal(ids(got), 1, 2, 3) {
		t.Fatalf("deleted %v", ids(got))
	}
	if got[0].Reason != ipc.DeleteStagingFloor {
		t.Fatalf("reason %q", got[0].Reason)
	}
	if got := SelectStagingFloor(files, 800, 750); len(got) != 0 {
		t.Fatalf("deleted %v above the floor", ids(got))
	}
	// Not enough files to reach the floor: everything goes, nothing panics.
	if got := SelectStagingFloor(files, 0, 1<<40); len(got) != 10 {
		t.Fatalf("deleted %d of 10", len(got))
	}
}
