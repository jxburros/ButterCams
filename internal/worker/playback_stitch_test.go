package worker

import (
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/media"
	"github.com/jxburros/buttercams/internal/media/mediatest"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/test/simulator"
)

// decodedFrame is one picture of a stream as ffmpeg decoded it.
type decodedFrame struct {
	pts  float64 // seconds
	hash string  // MD5 of the decoded picture
}

// decodeFrames decodes a stream completely and returns every picture's time
// and checksum.
func decodeFrames(t *testing.T, path string) []decodedFrame {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out, err := exec.Command(ff, "-v", "error", "-xerror", "-i", path, "-an", "-fps_mode", "passthrough", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg could not decode %s: %v", filepath.Base(path), err)
	}
	tb := 1.0
	var frames []decodedFrame
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "#tb 0:") {
			num, den, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:")), "/")
			n, _ := strconv.ParseFloat(num, 64)
			d, _ := strconv.ParseFloat(den, 64)
			if d > 0 {
				tb = n / d
			}
			continue
		}
		f := strings.Split(line, ",")
		if strings.HasPrefix(line, "#") || len(f) < 6 {
			continue
		}
		pts, _ := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		frames = append(frames, decodedFrame{pts * tb, strings.TrimSpace(f[5])})
	}
	return frames
}

// Playback crosses hour files with no visible seam (spec 16.5). Three
// make-before-break hand-overs in a row are recorded with the clock moved
// close to an hour boundary each time; then the files are fetched through
// the media process's playback handler exactly as the browser's player
// fetches them (index, header, byte ranges of the chunks) and put on one
// timeline the way the player does. ffmpeg must find every picture of the
// camera exactly once, evenly spaced, across every seam.
func TestPlaybackStitchesHourFilesWithoutASeam(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})

	// The worker's wall clock: real time plus an offset the test moves.
	real := time.Now()
	boundary := naming.NextBoundary(real).Add(time.Hour)
	var offset atomic.Int64
	offset.Store(int64(boundary.Add(-13 * time.Second).Sub(real)))
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	h := startWorker(t, t.TempDir(), now)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(10*time.Second, 0)

	// First hand-over at the hour, then some seconds of the new hour.
	var closed []*ipc.SegmentClosed
	c, idx := h.waitClosed(25*time.Second, 0)
	closed = append(closed, c)
	time.Sleep(3 * time.Second)

	// Move the clock to 13 s before the next hour. The recorder notices the
	// change and starts a new file at the next keyframe, on the same
	// session and without losing a frame (spec 6.6); then comes the second
	// hand-over.
	offset.Store(int64(boundary.Add(time.Hour - 13*time.Second).Sub(time.Now())))
	c, idx = h.waitClosed(10*time.Second, idx)
	closed = append(closed, c)
	c, idx = h.waitClosed(25*time.Second, idx)
	closed = append(closed, c)
	time.Sleep(3 * time.Second)
	h.stop()
	c, _ = h.waitClosed(time.Second, idx)
	closed = append(closed, c)

	reasons := ""
	for _, c := range closed {
		reasons += c.Reason + " "
		checkClosedFile(t, c)
	}
	if reasons != "hour_rollover clock_jump hour_rollover shutdown " {
		t.Fatalf("the files were closed as: %s", reasons)
	}
	for _, m := range h.all() {
		if o, ok := m.(*ipc.SegmentOpened); ok && o.Gap && filepath.Base(o.Path) != naming.PartName(filepath.Base(closed[0].Path)) {
			t.Fatalf("%s was opened with a gap", filepath.Base(o.Path))
		}
	}

	// The media process's playback server, and a client that does what the
	// player does.
	pb := media.NewPlayback(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(media.NewPlaybackHandler(pb))
	defer func() { srv.Close(); pb.Close() }()
	client := &mediatest.PlaybackClient{URL: srv.URL}
	file := func(c *ipc.SegmentClosed) mediatest.File { return mediatest.File{Paths: []string{c.Path}} }

	// The reference: the pictures of each file on its own.
	var own [][]decodedFrame
	for _, c := range closed {
		own = append(own, decodeFrames(t, c.Path))
	}
	// The camera loops a short clip, so the pictures repeat with a fixed
	// period. Find it in the first file.
	period := 0
	for n := 2; n <= 240 && period == 0; n++ {
		ok := len(own[0]) >= n+30
		for i := 0; ok && i+n < len(own[0]); i++ {
			ok = own[0][i].hash == own[0][i+n].hash
		}
		if ok {
			period = n
		}
	}
	if period == 0 {
		t.Fatal("the simulator's pictures do not repeat; the test needs a looping clip")
	}
	distinct := map[string]bool{}
	for _, f := range own[0][:period] {
		distinct[f.hash] = true
	}
	if len(distinct) < period/2 {
		t.Fatalf("only %d different pictures in a loop of %d: too few to tell frames apart", len(distinct), period)
	}

	// check verifies one stitched stream: evenly spaced, and every picture
	// is the one that follows its predecessor in the camera's loop, so none
	// is missing and none is shown twice.
	check := func(name string, frames []decodedFrame, want int) {
		t.Helper()
		if len(frames) != want {
			t.Fatalf("%s: %d pictures, want %d", name, len(frames), want)
		}
		step := frames[1].pts - frames[0].pts
		for i := 1; i < len(frames); i++ {
			if d := frames[i].pts - frames[i-1].pts; math.Abs(d-step) > 0.004 {
				t.Fatalf("%s: picture %d comes %.4f s after the one before it; the others %.4f s apart", name, i, d, step)
			}
		}
		for i := period; i < len(frames); i++ {
			if frames[i].hash != frames[i-period].hash {
				t.Fatalf("%s: picture %d (at %.3f s) is not the next picture of the camera: one was lost or repeated near it",
					name, i, frames[i].pts)
			}
		}
	}

	// The two hand-overs at the hour, each stitched like the player does.
	var pictures []decodedFrame
	dropped := 0
	for i, pair := range [][2]int{{0, 1}, {2, 3}} {
		a, b := closed[pair[0]], closed[pair[1]]
		if seam := b.Start.Sub(a.End.Time); math.Abs(seam.Seconds()) > 0.5 {
			t.Fatalf("hand-over %d: the files are %v apart", i+1, seam)
		}
		stitched, places, err := client.Stitch([]mediatest.File{file(a), file(b)})
		if err != nil {
			t.Fatal(err)
		}
		if !places[1].Joined || places[0].MediaStartMs != 0 {
			t.Fatalf("hand-over %d: the second file was not joined to the first: %+v", i+1, places)
		}
		path := filepath.Join(t.TempDir(), "stitched.mp4")
		if err := os.WriteFile(path, stitched, 0o644); err != nil {
			t.Fatal(err)
		}
		p := ffprobe(t, path) // a full decode without any error, audio included
		if p.codec("video") != "h264" || p.codec("audio") != "aac" {
			t.Fatalf("hand-over %d: stitched stream has %q/%q", i+1, p.codec("video"), p.codec("audio"))
		}
		frames := decodeFrames(t, path)
		// Pictures of the old file that the new file starts with are shown
		// once: the chunks holding them were left out of the old file.
		lost := 0
		if places[0].Dropped > 0 {
			ix, err := client.Index(file(a))
			if err != nil {
				t.Fatal(err)
			}
			cut := ix.Chunks[places[0].Chunks][0] / 1000
			for _, f := range own[pair[0]] {
				if f.pts >= cut-0.001 {
					lost++
				}
			}
			dropped++
		}
		t.Logf("hand-over %d: %d + %d pictures, %d duplicated at the seam and left out", i+1,
			len(own[pair[0]]), len(own[pair[1]]), lost)
		check("hand-over "+strconv.Itoa(i+1), frames, len(own[pair[0]])+len(own[pair[1]])-lost)
		pictures = append(pictures, frames...)
	}
	// The clock change between the second and the third file is not a seam
	// in the pictures either: the camera's loop carries on across it.
	for i := period; i < len(pictures); i++ {
		if pictures[i].hash != pictures[i-period].hash {
			t.Fatalf("across all four files, picture %d is not the next picture of the camera", i)
		}
	}
	_ = dropped
}
