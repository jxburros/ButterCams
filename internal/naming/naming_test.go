package naming

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone database unavailable: %v", err)
	}
	return loc
}

func TestFileNameExampleFromSpec(t *testing.T) {
	loc := time.FixedZone("CST", -6*3600)
	ts := time.Date(2026, 11, 14, 9, 17, 3, 0, loc)
	if got, want := FileName("front-door", ts, 0), "front-door_2026-11-14_0900_-0600.mp4"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := FileName("front-door", ts, 1), "front-door_2026-11-14_0900_-0600_b.mp4"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := PartName(FileName("front-door", ts, 2)), "front-door_2026-11-14_0900_-0600_c.part.mp4"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSuffixSequence(t *testing.T) {
	want := map[int]string{0: "", 1: "_b", 2: "_c", 25: "_z", 26: "_ba", 27: "_bb", 51: "_bz", 52: "_ca"}
	for n, w := range want {
		if got := Suffix(n); got != w {
			t.Errorf("Suffix(%d) = %q, want %q", n, got, w)
		}
	}
	seen := map[string]bool{}
	for n := 0; n < 2000; n++ {
		s := Suffix(n)
		if seen[s] {
			t.Fatalf("duplicate suffix %q at %d", s, n)
		}
		seen[s] = true
		if n > 0 && suffixIndex(s[1:]) != n {
			t.Fatalf("suffixIndex(%q) = %d, want %d", s, suffixIndex(s[1:]), n)
		}
	}
}

// Fall back: 01:00–02:00 happens twice. The UTC offset keeps both names unique.
func TestDSTFallBackProducesUniqueNames(t *testing.T) {
	loc := mustLoc(t, "America/Chicago")
	// 2026-11-01 06:30 UTC = 01:30 CDT (-0500); one hour later = 01:30 CST (-0600).
	first := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC).In(loc)
	second := first.Add(time.Hour)
	n1, n2 := FileName("cam", first, 0), FileName("cam", second, 0)
	if n1 != "cam_2026-11-01_0100_-0500.mp4" || n2 != "cam_2026-11-01_0100_-0600.mp4" {
		t.Fatalf("unexpected names %q, %q", n1, n2)
	}
	if !NextBoundary(first).Equal(HourStart(second)) {
		t.Fatalf("boundary after the first 01:00 hour must start the second 01:00 hour")
	}
	if got := NextBoundary(second).In(loc).Format("15:04 -0700"); got != "02:00 -0600" {
		t.Fatalf("boundary after repeated hour = %s", got)
	}
}

// Spring forward: 02:00–03:00 does not exist. 01:00 is followed by 03:00.
func TestDSTSpringForwardSkipsMissingHour(t *testing.T) {
	loc := mustLoc(t, "America/Chicago")
	// 2026-03-08 07:30 UTC = 01:30 CST (-0600); the next boundary is 03:00 CDT.
	before := time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC).In(loc)
	next := NextBoundary(before)
	if got := FileName("cam", before, 0); got != "cam_2026-03-08_0100_-0600.mp4" {
		t.Fatalf("got %q", got)
	}
	if got := FileName("cam", next, 0); got != "cam_2026-03-08_0300_-0500.mp4" {
		t.Fatalf("got %q", got)
	}
	if next.Sub(HourStart(before)) != time.Hour {
		t.Fatalf("the hour before the jump must still be exactly one hour long")
	}
}

func TestHalfHourOffsetZone(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+30*60)
	ts := time.Date(2026, 1, 2, 10, 45, 0, 0, loc)
	if got := HourStart(ts).Format(time.RFC3339); got != "2026-01-02T10:00:00+05:30" {
		t.Fatalf("got %s", got)
	}
	if got := FileName("cam", ts, 0); got != "cam_2026-01-02_1000_+0530.mp4" {
		t.Fatalf("got %s", got)
	}
}

func TestParseRoundTrip(t *testing.T) {
	loc := time.FixedZone("", -6*3600)
	ts := time.Date(2026, 11, 14, 9, 17, 3, 0, loc)
	for n := 0; n < 30; n++ {
		name := FileName("front-door", ts, n)
		p, ok := Parse(name)
		if !ok {
			t.Fatalf("Parse(%q) failed", name)
		}
		if p.CameraID != "front-door" || p.Date != "2026-11-14" || p.Hour != 9 || p.OffsetSec != -6*3600 || p.Index != n || p.Part {
			t.Fatalf("Parse(%q) = %+v", name, p)
		}
		if !p.HourStart.Equal(HourStart(ts)) {
			t.Fatalf("HourStart mismatch: %v vs %v", p.HourStart, HourStart(ts))
		}
		pp, ok := Parse(PartName(name))
		if !ok || !pp.Part || FinalName(PartName(name)) != name {
			t.Fatalf("part name round trip failed for %q", name)
		}
	}
	rel, ok := RelPath("front-door_2026-11-14_0900_-0600_b.mp4")
	if !ok || rel != RelDir("front-door", "2026-11-14")+string([]byte{'/'})+"front-door_2026-11-14_0900_-0600_b.mp4" && rel == "" {
		t.Fatalf("RelPath = %q", rel)
	}
}

func TestParseRejectsForeignFiles(t *testing.T) {
	for _, name := range []string{
		"holiday.mp4", "front-door_2026-11-14_0930_-0600.mp4", "Front_2026-11-14_0900_-0600.mp4",
		"front-door_2026-11-14_0900_-0600.mkv", "front-door_2026-11-14_2500_-0600.mp4",
		"front-door_2026-13-40_0900_-0600.mp4", "front-door_2026-11-14_0900_-0600_B.mp4", "",
	} {
		if _, ok := Parse(name); ok {
			t.Errorf("Parse(%q) should fail", name)
		}
	}
}

func TestNextFree(t *testing.T) {
	loc := time.FixedZone("", 0)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	taken := map[string]bool{FileName("c", ts, 0): true, FileName("c", ts, 1): true}
	got := NextFree("c", ts, func(n string) bool { return taken[n] })
	if got != "c_2026-01-01_0000_+0000_c.mp4" {
		t.Fatalf("got %q", got)
	}
}
