package worker

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/test/simulator"
)

// clockBefore returns a wall clock that reads `lead` before the next top of
// the hour right now, and that boundary. The monotonic clock is untouched.
func clockBefore(lead time.Duration) (func() time.Time, time.Time) {
	real := time.Now()
	boundary := naming.NextBoundary(real).Add(time.Hour)
	offset := boundary.Add(-lead).Sub(real)
	return func() time.Time { return time.Now().Add(offset) }, boundary
}

func hourOf(path string) time.Time {
	p, _ := naming.Parse(filepath.Base(path))
	return p.HourStart
}

// Make-before-break: a second session is opened before the hour, the new
// file starts at its first keyframe at or after HH:00:00, and there is no gap
// (spec 6.5, steps 1–3).
func TestHourlyRolloverMakeBeforeBreak(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})
	now, boundary := clockBefore(13 * time.Second)
	h := startWorker(t, t.TempDir(), now)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	first, _ := h.waitOpened(10*time.Second, 0)
	if !hourOf(first.Path).Equal(boundary.Add(-time.Hour)) {
		t.Fatalf("first file %s is not in the hour before the boundary", filepath.Base(first.Path))
	}

	// Two sessions must be open shortly before the hour.
	sawTwo := false
	deadline := time.Now().Add(14 * time.Second)
	for time.Now().Before(deadline) && !sawTwo {
		sawTwo = sim.Sessions("/cam") == 2
		time.Sleep(50 * time.Millisecond)
	}
	if !sawTwo {
		t.Fatal("no second session was opened before the hour")
	}

	closed, idx := h.waitClosed(20*time.Second, 0)
	second, _ := h.waitOpened(5*time.Second, idx-1)
	if closed.Reason != ipc.ReasonHourRollover {
		t.Fatalf("close reason %q", closed.Reason)
	}
	if second.Gap {
		t.Fatal("make-before-break must not leave a gap")
	}
	if !hourOf(second.Path).Equal(boundary) {
		t.Fatalf("second file %s is not the new hour's file", filepath.Base(second.Path))
	}
	if p, _ := naming.Parse(filepath.Base(second.Path)); p.Suffix != "" {
		t.Fatalf("the new hour's first file has suffix %q", p.Suffix)
	}
	if second.Start.Before(boundary) || second.Start.Sub(boundary) > 1500*time.Millisecond {
		t.Fatalf("new file starts %v after the hour; want the first keyframe at or after HH:00:00", second.Start.Sub(boundary))
	}
	if seam := second.Start.Sub(closed.End.Time); math.Abs(seam.Seconds()) > 0.5 {
		t.Fatalf("seam between the files is %v; want no gap (small overlap allowed)", seam)
	}
	checkClosedFile(t, closed)
	ffprobe(t, closed.Path)

	// The old session is torn down after the switch.
	time.Sleep(1500 * time.Millisecond)
	if n := sim.Sessions("/cam"); n != 1 {
		t.Fatalf("%d sessions after the rollover, want 1", n)
	}
	for _, m := range h.all() {
		if e, ok := m.(*ipc.Event); ok && e.Severity != ipc.SevInfo {
			t.Fatalf("unexpected event during a clean rollover: %+v", e)
		}
	}

	h.stop()
	last, _ := h.waitClosed(time.Second, idx)
	checkClosedFile(t, last)
	p := ffprobe(t, last.Path)
	if p.codec("video") != "h264" || p.codec("audio") != "aac" {
		t.Fatalf("new hour's file has %q/%q", p.codec("video"), p.codec("audio"))
	}
}

// A camera that refuses a second session is remembered as single-session and
// rolled over break-before-make, with the gap recorded (spec 6.5, step 5).
func TestSingleSessionFallback(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", SingleSession: true})
	now, boundary := clockBefore(13 * time.Second)
	h := startWorker(t, t.TempDir(), now)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(10*time.Second, 0)

	h.waitEvent(15*time.Second, "single_session")
	closed, idx := h.waitClosed(20*time.Second, 0)
	if closed.Reason != ipc.ReasonHourRollover {
		t.Fatalf("close reason %q", closed.Reason)
	}
	if closed.End.Before(boundary.Add(-500*time.Millisecond)) || closed.End.Sub(boundary) > 2*time.Second {
		t.Fatalf("old file ends %v from the hour", closed.End.Sub(boundary))
	}
	second, _ := h.waitOpened(15*time.Second, idx)
	if !second.Gap {
		t.Fatal("break-before-make must record the gap")
	}
	if !hourOf(second.Path).Equal(boundary) {
		t.Fatalf("second file %s is not the new hour's file", filepath.Base(second.Path))
	}
	if gap := second.Start.Sub(closed.End.Time); gap <= 0 || gap > 8*time.Second {
		t.Fatalf("gap at the hour is %v", gap)
	}
	checkClosedFile(t, closed)

	m, _ := h.wait(10*time.Second, "single-session stats", 0, func(m any) bool {
		s, ok := m.(*ipc.CameraStats)
		return ok && s.SingleSession && s.State == ipc.StateRecording
	})
	_ = m
}

// When the second session cannot be opened by HH:00:15, the old session
// starts the new hour's file at its next keyframe and a warning is raised
// (spec 6.5, step 4).
func TestRefreshFailureRollsOverOnOldSession(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	now, boundary := clockBefore(13 * time.Second)
	h := startWorker(t, t.TempDir(), now)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(10*time.Second, 0)
	sim.FailNewSessions("/cam", true)

	closed, idx := h.waitClosed(40*time.Second, 0)
	second, _ := h.waitOpened(5*time.Second, idx-1)
	if closed.Reason != ipc.ReasonHourRollover || second.Gap {
		t.Fatalf("reason %q gap %v; want hour_rollover without a gap", closed.Reason, second.Gap)
	}
	late := second.Start.Sub(boundary)
	if late < RolloverGrace || late > RolloverGrace+2*time.Second {
		t.Fatalf("old session started the new file %v after the hour; want just after HH:00:15", late)
	}
	if !hourOf(second.Path).Equal(boundary) {
		t.Fatalf("second file %s is not the new hour's file", filepath.Base(second.Path))
	}
	if seam := second.Start.Sub(closed.End.Time); math.Abs(seam.Seconds()) > 0.2 {
		t.Fatalf("seam on the same session is %v", seam)
	}
	ev := h.waitEvent(5*time.Second, "refresh_failed")
	if ev.Severity != ipc.SevWarning {
		t.Fatalf("refresh failure severity %q", ev.Severity)
	}
	checkClosedFile(t, closed)
	if n := sim.Sessions("/cam"); n != 1 {
		t.Fatalf("%d sessions, want the original one only", n)
	}
}
