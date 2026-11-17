package media_test

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/media"
	"github.com/jxburros/buttercams/internal/media/mediatest"
	"github.com/jxburros/buttercams/test/simulator"
)

const testToken = "run-token"

// liveRig is a media server with its real handler, and a camera simulator.
type liveRig struct {
	t   *testing.T
	hub *media.Hub
	ts  *httptest.Server
	sim *simulator.Server
}

func newLiveRig(t *testing.T, streams ...simulator.Stream) *liveRig {
	t.Helper()
	sim, err := simulator.Start("127.0.0.1:0", streams)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	hub, handler := media.NewLiveTestServer(testToken)
	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		hub.Close()
		ts.Close()
	})
	return &liveRig{t: t, hub: hub, ts: ts, sim: sim}
}

// camera returns a camera whose main stream is path and whose sub stream is
// sub ("" for none).
func (r *liveRig) camera(id, path, sub string) config.Camera {
	host, portStr, _ := net.SplitHostPort(r.sim.Addr())
	port, _ := strconv.Atoi(portStr)
	cam := config.Camera{ID: id, Name: id, Enabled: true, Host: host, Port: port, Path: path,
		Transport: "tcp", RecordAudio: true, RecordingMode: "continuous"}
	if sub != "" {
		cam.SubStreamPath = &sub
	}
	return cam
}

func (r *liveRig) url(camera, stream string, audio bool) string {
	u := "ws" + strings.TrimPrefix(r.ts.URL, "http") + "/live/" + camera + "/" + stream
	if audio {
		u += "?audio=1"
	}
	return u
}

func (r *liveRig) view(camera, stream string, audio bool) *mediatest.LiveViewer {
	r.t.Helper()
	v, res, err := mediatest.DialLive(r.url(camera, stream, audio), http.Header{ipc.TokenHeader: {testToken}})
	if err != nil {
		r.t.Fatalf("opening the live stream: %v (%v)", err, res)
	}
	r.t.Cleanup(v.Close)
	return v
}

func waitFragments(t *testing.T, v *mediatest.LiveViewer, n int, timeout time.Duration) {
	t.Helper()
	if !v.Wait(timeout, func(v *mediatest.LiveViewer) bool { return v.Fragments() >= n }) {
		t.Fatalf("only %d of %d fragments arrived in %s; states: %q", v.Fragments(), n, timeout, v.States())
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type decoded struct {
	pts   []float64 // presentation times of the video frames, in order
	keys  []bool
	audio int // audio frames
}

// decode writes a stream to a file and has ffmpeg decode every frame of it.
// Without ffmpeg the test is skipped.
func decode(t *testing.T, data []byte) decoded {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	path := filepath.Join(t.TempDir(), "live.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		t.Fatalf("ffmpeg cannot decode the live stream: %v\n%s", err, stderr.String())
	}
	var d decoded
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
		"frame=pts_time,key_frame", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 2 {
			continue
		}
		key, pts := f[0], f[1]
		if strings.Contains(f[0], ".") {
			key, pts = f[1], f[0]
		}
		v, err := strconv.ParseFloat(pts, 64)
		if err != nil {
			t.Fatalf("ffprobe line %q", line)
		}
		d.pts = append(d.pts, v)
		d.keys = append(d.keys, key == "1")
	}
	out, _ = exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-count_frames", "-show_entries",
		"stream=nb_read_frames", "-of", "csv=p=0", path).Output()
	d.audio, _ = strconv.Atoi(strings.Trim(strings.TrimSpace(string(out)), ","))
	return d
}

// checkContinuous asserts what a browser needs: the first frame is a
// keyframe, times only go forward, and no hole is longer than maxGap.
func checkContinuous(t *testing.T, d decoded, minFrames int, maxGap float64) {
	t.Helper()
	if len(d.pts) < minFrames {
		t.Fatalf("%d frames decoded, want at least %d", len(d.pts), minFrames)
	}
	if !d.keys[0] {
		t.Fatal("the first frame is not a keyframe")
	}
	for i := 1; i < len(d.pts); i++ {
		gap := d.pts[i] - d.pts[i-1]
		if gap <= 0 {
			t.Fatalf("frame %d: time went from %.3f to %.3f", i, d.pts[i-1], d.pts[i])
		}
		if gap > maxGap {
			t.Fatalf("frame %d: a hole of %.3f s (from %.3f to %.3f)", i, gap, d.pts[i-1], d.pts[i])
		}
	}
}

func TestLiveSubStream(t *testing.T) {
	cases := []struct {
		name   string
		stream simulator.Stream
		mime   string
		w, h   int
	}{
		{"h264", simulator.Stream{Path: "/cam/sub", Small: true}, `video/mp4; codecs="avc1.`, 160, 120},
		{"h264-4s-keyframes", simulator.Stream{Path: "/cam/sub", KeyframeInterval: 4 * time.Second}, `video/mp4; codecs="avc1.`, 320, 240},
		{"h265", simulator.Stream{Path: "/cam/sub", Video: "h265"}, `video/mp4; codecs="hvc1.`, 320, 240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLiveRig(t, simulator.Stream{Path: "/cam"}, tc.stream)
			r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
			v := r.view("cam", "sub", true) // audio is never sent on a sub stream
			waitFragments(t, v, 5*simulator.FPS, 20*time.Second)

			inits := v.Inits()
			if len(inits) != 1 || !strings.HasPrefix(inits[0].Mime, tc.mime) || strings.Contains(inits[0].Mime, "mp4a") ||
				inits[0].Width != tc.w || inits[0].Height != tc.h {
				t.Fatalf("init messages: %+v", inits)
			}
			ev := v.Events()
			first := -1
			for i, e := range ev {
				if e.Kind == "init" {
					first = i
					break
				}
			}
			if first < 0 || ev[first+1].Kind != "header" || ev[first+2].Kind != "fragment" {
				t.Fatalf("message order: %+v", ev[:min(len(ev), 6)])
			}
			before := v.Fragments()
			data := v.Bytes()
			after := v.Fragments()
			d := decode(t, data)
			checkContinuous(t, d, 5*simulator.FPS-4, 0.2)
			if got := len(d.pts); got < before-4 || got > after {
				t.Fatalf("%d frames decoded from %d to %d fragments: not one fragment per frame", got, before, after)
			}
			if r.sim.Sessions("/cam") != 0 {
				t.Fatal("the sub stream viewer opened a session on the main stream")
			}
			viewers, sources, _ := r.hub.Stats()
			if viewers != 1 || len(sources) != 1 || sources[0] != (ipc.MediaSource{CameraID: "cam", Stream: "sub", Viewers: 1, State: "playing"}) {
				t.Fatalf("stats: %d %+v", viewers, sources)
			}
		})
	}
}

// The media process serves only the guardian (spec 16.2).
func TestLiveRoutingAndToken(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub", Small: true})
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", r.ts.URL+path, nil)
		if token != "" {
			req.Header.Set(ipc.TokenHeader, token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := get("/live/cam/sub", ""); got != 403 {
		t.Fatalf("no token: %d", got)
	}
	if got := get("/live/cam/sub", "wrong"); got != 403 {
		t.Fatalf("wrong token: %d", got)
	}
	if got := get("/live/nobody/sub", testToken); got != 404 {
		t.Fatalf("unknown camera: %d", got)
	}
	if got := get("/live/cam/both", testToken); got != 404 {
		t.Fatalf("unknown stream: %d", got)
	}
	if got := get("/live/cam/sub", testToken); got != 400 {
		t.Fatalf("a plain GET instead of a WebSocket: %d", got)
	}
	if _, res, err := mediatest.DialLive(r.url("cam", "sub", false), nil); err == nil || res == nil || res.StatusCode != 403 {
		t.Fatalf("a WebSocket without the token: %v %v", err, res)
	}
}

// A second viewer shares the camera session and gets a picture at once from
// the pictures kept since the last keyframe (spec 16.3).
func TestLiveViewersShareOneSource(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub", KeyframeInterval: 4 * time.Second})
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	a := r.view("cam", "sub", false)
	waitFragments(t, a, 2*simulator.FPS, 20*time.Second)

	start := time.Now()
	b := r.view("cam", "sub", false)
	waitFragments(t, b, 1, 2*time.Second)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the second viewer waited %s for its first picture", took)
	}
	if n := r.sim.Sessions("/cam/sub"); n != 1 {
		t.Fatalf("%d camera sessions for two viewers", n)
	}
	waitFragments(t, b, 3*simulator.FPS, 20*time.Second)
	checkContinuous(t, decode(t, b.Bytes()), 3*simulator.FPS-2, 0.2)
	if viewers, sources, _ := r.hub.Stats(); viewers != 2 || len(sources) != 1 || sources[0].Viewers != 2 {
		t.Fatalf("stats: %d %+v", viewers, sources)
	}
}

// Nothing runs when nobody watches: the source closes after the linger and a
// later viewer starts it again (spec 16.1).
func TestLiveSourceStopsAfterTheLastViewer(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub", Small: true})
	r.hub.Linger = 400 * time.Millisecond
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	if r.sim.Sessions("/cam/sub") != 0 {
		t.Fatal("a source was opened without a viewer")
	}
	v := r.view("cam", "sub", false)
	waitFragments(t, v, 5, 20*time.Second)
	v.Close()

	// Within the linger the session is kept, and a returning viewer uses it.
	time.Sleep(100 * time.Millisecond)
	if r.sim.Sessions("/cam/sub") != 1 {
		t.Fatal("the source was closed before the linger passed")
	}
	v2 := r.view("cam", "sub", false)
	waitFragments(t, v2, 5, 5*time.Second)
	time.Sleep(600 * time.Millisecond)
	if r.sim.Sessions("/cam/sub") != 1 {
		t.Fatal("the source was closed while a viewer was watching")
	}
	v2.Close()

	waitFor(t, 5*time.Second, "the camera session to close", func() bool { return r.sim.Sessions("/cam/sub") == 0 })
	waitFor(t, 5*time.Second, "the source to leave the statistics", func() bool {
		_, sources, _ := r.hub.Stats()
		return len(sources) == 0
	})
	v3 := r.view("cam", "sub", false)
	waitFragments(t, v3, 5, 20*time.Second)
	checkContinuous(t, decode(t, v3.Bytes()), 4, 0.2)
}

// A camera that stalls is reported to the viewer and comes back by itself.
func TestLiveStallAndReturn(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub", Small: true})
	r.hub.StallTimeout = time.Second
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	v := r.view("cam", "sub", false)
	waitFragments(t, v, simulator.FPS, 20*time.Second)

	r.sim.Stall("/cam/sub", 3*time.Second)
	if !v.Wait(10*time.Second, func(v *mediatest.LiveViewer) bool {
		for _, s := range v.States() {
			if strings.Contains(s, "stopped sending video") {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the stall was not reported: %q", v.States())
	}
	before := v.Fragments()
	waitFragments(t, v, before+2*simulator.FPS, 30*time.Second)
	if len(v.Inits()) != 1 {
		t.Fatalf("the same stream must not get a new header after a reconnect: %+v", v.Inits())
	}
	// The viewer's timeline has no hole: the browser never waits at one.
	checkContinuous(t, decode(t, v.Bytes()), 3*simulator.FPS-4, 0.2)
}

// A change of the camera's picture size gives the viewer a new header.
func TestLiveParameterChange(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub"})
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	v := r.view("cam", "sub", false)
	waitFragments(t, v, simulator.FPS, 20*time.Second)
	if err := r.sim.ChangeResolution("/cam/sub"); err != nil {
		t.Fatal(err)
	}
	if !v.Wait(10*time.Second, func(v *mediatest.LiveViewer) bool { return len(v.Inits()) == 2 }) {
		t.Fatalf("no new init after the resolution changed: %+v", v.Inits())
	}
	waitFragments(t, v, v.Fragments()+2*simulator.FPS, 20*time.Second)
	inits := v.Inits()
	if inits[0].Width != 320 || inits[1].Width != 160 || inits[1].Height != 120 {
		t.Fatalf("inits: %+v", inits)
	}
	streams := v.Streams()
	if len(streams) != 2 {
		t.Fatalf("%d streams", len(streams))
	}
	checkContinuous(t, decode(t, streams[0]), simulator.FPS-2, 0.2)
	second := decode(t, streams[1])
	checkContinuous(t, second, 2*simulator.FPS-4, 0.2)
	// The timeline carries on over the change.
	if first := decode(t, streams[0]); second.pts[0] <= first.pts[len(first.pts)-1] {
		t.Fatalf("the second header's frames start at %.3f, before the first ended", second.pts[0])
	}
}

// A viewer that never reads delays nobody: its queue overflows and is
// emptied, and the other viewer's stream stays smooth (spec 16.4).
func TestSlowViewerAffectsNobody(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam/sub", PadBytes: 250_000})
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub")}, nil, "")
	good := r.view("cam", "sub", false)
	waitFragments(t, good, simulator.FPS, 20*time.Second)

	stalled, err := mediatest.DialLiveStalled(r.url("cam", "sub", false), http.Header{ipc.TokenHeader: {testToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	waitFor(t, 60*time.Second, "the stalled viewer's queue to overflow", func() bool {
		_, _, overflows := r.hub.Stats()
		return overflows > 0
	})
	waitFragments(t, good, good.Fragments()+2*simulator.FPS, 20*time.Second)

	var last time.Time
	worst := time.Duration(0)
	for _, e := range good.Events() {
		if e.Kind != "fragment" {
			continue
		}
		if !last.IsZero() && e.At.Sub(last) > worst {
			worst = e.At.Sub(last)
		}
		last = e.At
	}
	if worst > 700*time.Millisecond {
		t.Fatalf("the good viewer waited %s between two frames while another viewer was stuck", worst)
	}
	if viewers, _, _ := r.hub.Stats(); viewers != 2 {
		t.Fatalf("%d viewers", viewers)
	}
	checkContinuous(t, decode(t, good.Bytes()), 3*simulator.FPS-4, 0.2)
}

func TestLiveWithoutASubStream(t *testing.T) {
	r := newLiveRig(t, simulator.Stream{Path: "/cam"}, simulator.Stream{Path: "/off/sub", Refuse: true})
	r.hub.MainRetry = 200 * time.Millisecond
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", ""), r.camera("off", "/cam", "/off/sub")}, nil, "")
	hasState := func(want string) func(v *mediatest.LiveViewer) bool {
		return func(v *mediatest.LiveViewer) bool {
			for _, s := range v.States() {
				if strings.Contains(s, want) {
					return true
				}
			}
			return false
		}
	}
	v := r.view("cam", "sub", false)
	if !v.Wait(5*time.Second, hasState("has no sub stream")) {
		t.Fatalf("states: %q", v.States())
	}
	m := r.view("cam", "main", false)
	if !m.Wait(5*time.Second, hasState("main stream is not available")) {
		t.Fatalf("states: %q", m.States())
	}
	refused := r.view("off", "sub", false)
	if !refused.Wait(10*time.Second, hasState("RTSP 500")) {
		t.Fatalf("states: %q", refused.States())
	}
	if _, sources, _ := r.hub.Stats(); len(sources) != 3 || sources[0].State != "waiting" {
		t.Fatalf("sources: %+v", sources)
	}

	// The sub stream is added later: the waiting viewer gets video without
	// reconnecting.
	r.sim.Add(simulator.Stream{Path: "/cam/sub", Small: true})
	r.hub.Assign([]config.Camera{r.camera("cam", "/cam", "/cam/sub"), r.camera("off", "/cam", "/off/sub")}, nil, "")
	waitFragments(t, v, 5, 20*time.Second)

	// The camera is deleted: its viewers are disconnected.
	r.hub.Assign([]config.Camera{r.camera("off", "/cam", "/off/sub")}, nil, "")
	waitFor(t, 5*time.Second, "the deleted camera's viewers to be disconnected", func() bool { return v.Closed() && m.Closed() })
	if refused.Closed() {
		t.Fatal("a viewer of another camera was disconnected")
	}
}
