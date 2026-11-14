// Package naming implements the recording file naming scheme (spec 6.4):
//
//	<root>\<camera-id>\<YYYY-MM-DD>\<camera-id>_<YYYY-MM-DD>_<HH>00_<±HHMM>[_b].mp4
//
// The name carries everything needed to locate a file: the folder is derived
// from the name, never stored.
package naming

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SegmentDuration is the fixed length of a recording file. It is a constant on
// purpose (spec 5.2): it is not configurable in v1.
const SegmentDuration = time.Hour

const (
	// Ext is the extension of a closed recording.
	Ext = ".mp4"
	// PartExt marks a file that is still being written.
	PartExt = ".part.mp4"
)

var (
	idRe   = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)
	fileRe = regexp.MustCompile(`^([a-z0-9-]{1,48})_(\d{4}-\d{2}-\d{2})_(\d{2})00_([+-])(\d{2})(\d{2})(?:_([a-z]+))?(\.part)?\.mp4$`)
)

// ValidID reports whether s is a valid camera or destination id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// HourStart returns the top of the local-time hour containing t. It works on
// the absolute timeline, so it is correct across DST changes and for zones with
// fractional-hour offsets.
func HourStart(t time.Time) time.Time {
	rem := time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
	return t.Add(-rem)
}

// NextBoundary returns the next top-of-hour strictly after t.
func NextBoundary(t time.Time) time.Time {
	return HourStart(t).Add(SegmentDuration)
}

// Suffix returns the partial-file suffix for the n-th file of an hour:
// 0 → "", 1 → "_b", 2 → "_c" … 25 → "_z", 26 → "_ba", 27 → "_bb" …
func Suffix(n int) string {
	if n <= 0 {
		return ""
	}
	var letters []byte
	for n > 0 {
		letters = append([]byte{byte('a' + n%26)}, letters...)
		n /= 26
	}
	return "_" + string(letters)
}

// suffixIndex is the inverse of Suffix ("" → 0, "b" → 1 …).
func suffixIndex(s string) int {
	n := 0
	for _, c := range s {
		n = n*26 + int(c-'a')
	}
	return n
}

// Base returns the file name without suffix or extension for the hour
// containing t, for example "front-door_2026-11-14_0900_-0600".
func Base(cameraID string, t time.Time) string {
	h := HourStart(t)
	return fmt.Sprintf("%s_%s_%s", cameraID, h.Format("2006-01-02"), h.Format("1500_-0700"))
}

// FileName returns the closed-file name for the n-th file of t's hour.
func FileName(cameraID string, t time.Time, n int) string {
	return Base(cameraID, t) + Suffix(n) + Ext
}

// PartName converts a closed-file name into its in-progress name.
func PartName(final string) string {
	return strings.TrimSuffix(final, Ext) + PartExt
}

// FinalName converts an in-progress name into its closed-file name.
func FinalName(part string) string {
	if strings.HasSuffix(part, PartExt) {
		return strings.TrimSuffix(part, PartExt) + Ext
	}
	return part
}

// Parsed is the information carried by a recording file name.
type Parsed struct {
	CameraID  string
	Date      string // YYYY-MM-DD, local date of the hour
	Hour      int
	OffsetSec int    // UTC offset of the hour, seconds east of UTC
	Suffix    string // "", "b", "c" …
	Index     int    // 0 for the first file of the hour
	Part      bool   // true for a .part.mp4 file
	HourStart time.Time
}

// Parse decodes a recording file name. ok is false for any name that
// ButterCams did not produce.
func Parse(name string) (p Parsed, ok bool) {
	m := fileRe.FindStringSubmatch(name)
	if m == nil {
		return p, false
	}
	hour, _ := strconv.Atoi(m[3])
	oh, _ := strconv.Atoi(m[5])
	om, _ := strconv.Atoi(m[6])
	if hour > 23 || oh > 14 || om > 59 {
		return p, false
	}
	off := oh*3600 + om*60
	if m[4] == "-" {
		off = -off
	}
	day, err := time.Parse("2006-01-02", m[2])
	if err != nil {
		return p, false
	}
	loc := time.FixedZone("", off)
	p = Parsed{
		CameraID:  m[1],
		Date:      m[2],
		Hour:      hour,
		OffsetSec: off,
		Suffix:    m[7],
		Index:     suffixIndex(m[7]),
		Part:      m[8] != "",
		HourStart: time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, loc),
	}
	return p, true
}

// RelDir returns the folder of a file relative to a storage root.
func RelDir(cameraID, date string) string {
	return filepath.Join(cameraID, date)
}

// RelPath returns the path of a recording relative to a storage root. The
// layout is the same on staging and on every destination.
func RelPath(fileName string) (string, bool) {
	p, ok := Parse(fileName)
	if !ok {
		return "", false
	}
	return filepath.Join(p.CameraID, p.Date, fileName), true
}

// NextFree returns the first file name for t's hour for which taken reports
// false. taken is called with closed-file names; callers check both the closed
// and the in-progress form.
func NextFree(cameraID string, t time.Time, taken func(finalName string) bool) string {
	for n := 0; ; n++ {
		name := FileName(cameraID, t, n)
		if !taken(name) {
			return name
		}
	}
}
