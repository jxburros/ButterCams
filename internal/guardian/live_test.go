package guardian

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/test/simulator"
)

// These tests run live view and playback end to end: a browser's requests
// against the real guardian, which proxies to a real media process, which
// reads sub streams from the simulator, main streams from the real workers'
// restream, and recordings from staging.

// liveSystem is a system with the web server on and an administrator.
type liveSystem struct {
	*system
	base string
	c    *http.Client
	csrf string
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newLiveSystem(t *testing.T, streams []simulator.Stream, mutate func(cfg *config.Config, sim *simulator.Server, root string)) *liveSystem {
	t.Helper()
	port := freePort(t)
	sys := newSystem(t, streams, func(cfg *config.Config, sim *simulator.Server, root string) {
		cfg.Server = config.Server{ListenAddress: "127.0.0.1", Port: port, HTTPS: false, AllowedSubnets: []string{}}
		if mutate != nil {
			mutate(cfg, sim, root)
		}
	})
	users, err := api.OpenUserStore(sys.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create("admin", "correct horse", api.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	return &liveSystem{system: sys, base: "http://127.0.0.1:" + strconv.Itoa(port), c: &http.Client{Jar: jar}}
}

func (s *liveSystem) start() {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan error, 1)
	s.g = New(Options{DataRoot: s.root, Exe: testExe, HealthInterval: time.Hour})
	go func() { s.done <- s.g.Run(ctx) }()
	select {
	case <-s.g.ready:
	case err := <-s.done:
		s.t.Fatalf("guardian did not start: %v", err)
	case <-time.After(30 * time.Second):
		s.t.Fatal("guardian did not start in time")
	}
	s.t.Cleanup(s.stop)
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "correct horse"})
	res, err := s.c.Post(s.base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		CSRF string `json:"csrfToken"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || out.CSRF == "" {
		s.t.Fatalf("login: %d", res.StatusCode)
	}
	s.csrf = out.CSRF
}

func (s *liveSystem) get(path string, hdr map[string]string) (int, []byte) {
	s.t.Helper()
	req, _ := http.NewRequest("GET", s.base+"/api/v1"+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := s.c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}

func (s *liveSystem) getJSON(path string, v any) int {
	s.t.Helper()
	status, data := s.get(path, nil)
	if status == 200 {
		if err := json.Unmarshal(data, v); err != nil {
			s.t.Fatalf("%s: %v in %s", path, err, data)
		}
	}
	return status
}

func (s *liveSystem) putSystem(body string) {
	s.t.Helper()
	req, _ := http.NewRequest("PUT", s.base+"/api/v1/system", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.CSRFHeader, s.csrf)
	res, err := s.c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		s.t.Fatalf("PUT /system %s: %d", body, res.StatusCode)
	}
}

func (s *liveSystem) waitMedia() {
	s.t.Helper()
	s.until(20*time.Second, "the media process", func() bool { _, _, ok := s.g.Media(); return ok })
}

// liveCapture is what a browser would have appended for one live stream.
type liveCapture struct {
	inits     []map[string]any // every init message
	states    []string         // messages of state messages
	data      bytes.Buffer     // initialization segment and fragments, in order
	fragments int
	err       error
}

// watch opens a live stream as a browser would and records it until stop
// returns true or the time is up. The connection error, if any, is in err.
func (s *liveSystem) watch(camera, kind string, audio bool, limit time.Duration, stop func(c *liveCapture) bool) *liveCapture {
	s.t.Helper()
	u, _ := url.Parse(s.base)
	hdr := http.Header{"Origin": {s.base}}
	for _, c := range s.c.Jar.Cookies(u) {
		hdr.Add("Cookie", c.String())
	}
	q := "0"
	if audio {
		q = "1"
	}
	out := &liveCapture{}
	conn, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.base, "http")+"/api/v1/live/"+camera+"/"+kind+"?audio="+q, hdr)
	if err != nil {
		code := 0
		if res != nil {
			code = res.StatusCode
		}
		out.err = fmt.Errorf("connect: %v (HTTP %d)", err, code)
		return out
	}
	defer conn.Close()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		typ, msg, err := conn.ReadMessage()
		if err != nil {
			out.err = err
			return out
		}
		if typ == websocket.TextMessage {
			var m map[string]any
			json.Unmarshal(msg, &m)
			switch m["type"] {
			case "init":
				out.inits = append(out.inits, m)
			case "state":
				text, _ := m["message"].(string)
				out.states = append(out.states, text)
			}
		} else {
			out.data.Write(msg)
			if len(out.inits) > 0 && bytes.Contains(msg[:min(len(msg), 64)], []byte("moof")) {
				out.fragments++
			}
		}
		if stop != nil && stop(out) {
			return out
		}
	}
	return out
}

type decoded struct {
	codec  string
	width  int
	frames int
	audio  string
	ptsOK  bool
}

// decode runs ffprobe over what a viewer received. It returns false when
// ffprobe is not installed.
func decodeMP4(t *testing.T, data []byte) (decoded, bool) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return decoded{}, false
	}
	path := filepath.Join(t.TempDir(), "capture.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-show_entries",
		"stream=codec_type,codec_name,width,nb_read_frames", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe cannot read what the viewer received: %v", err)
	}
	var doc struct {
		Streams []struct {
			Type   string `json:"codec_type"`
			Name   string `json:"codec_name"`
			Width  int    `json:"width"`
			Frames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	json.Unmarshal(out, &doc)
	var d decoded
	for _, st := range doc.Streams {
		if st.Type == "video" {
			d.codec, d.width = st.Name, st.Width
			d.frames, _ = strconv.Atoi(st.Frames)
		} else if st.Type == "audio" {
			d.audio = st.Name
		}
	}
	// Presentation times must rise without a hole.
	ptsOut, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=pts_time",
		"-of", "csv=p=0", path).Output()
	if err == nil {
		d.ptsOK = true
		last := -1.0
		for _, line := range strings.Fields(string(ptsOut)) {
			v, err := strconv.ParseFloat(strings.TrimRight(line, ","), 64)
			if err != nil {
				continue
			}
			if last >= 0 && (v <= last || v-last > 0.5) {
				d.ptsOK = false
			}
			last = v
		}
	}
	return d, true
}

func liveStreams() []simulator.Stream {
	return []simulator.Stream{
		{Path: "/cam1", Audio: "pcmu"},
		{Path: "/cam1/sub", Small: true},
		{Path: "/cam2"},
	}
}

func liveCams(cfg *config.Config, sim *simulator.Server, root string) {
	sub := "/cam1/sub"
	cam1 := simCam(sim, "cam1", "/cam1")
	cam1.SubStreamPath = &sub
	cfg.Cameras = []config.Camera{cam1, simCam(sim, "cam2", "/cam2")}
}

// Live view and playback through every layer: browser → guardian → media
// process → camera, restream and recordings (spec 16.2 to 16.5).
func TestLiveViewAndPlaybackEndToEnd(t *testing.T) {
	s := newLiveSystem(t, liveStreams(), liveCams)
	s.start()
	s.waitRecording("cam1", "cam2")
	s.waitMedia()

	// Sub stream: the media process connects to the camera itself.
	sub := s.watch("cam1", "sub", false, 20*time.Second, func(c *liveCapture) bool { return c.fragments >= 45 })
	if sub.err != nil || len(sub.inits) == 0 || sub.fragments < 45 {
		t.Fatalf("sub stream: %d fragments, %d init messages, states %v, error %v\n%s", sub.fragments, len(sub.inits), sub.states, sub.err, s.logTail())
	}
	if mime, _ := sub.inits[0]["mime"].(string); !strings.HasPrefix(mime, `video/mp4; codecs="avc1.`) || sub.inits[0]["width"] != float64(160) {
		t.Fatalf("sub stream init message: %v", sub.inits[0])
	}
	if d, ok := decodeMP4(t, sub.data.Bytes()); ok && (d.codec != "h264" || d.width != 160 || d.frames < 40 || !d.ptsOK || d.audio != "") {
		t.Fatalf("what the sub stream viewer received does not decode cleanly: %+v", d)
	}
	if got := s.sim.Sessions("/cam1/sub"); got != 1 {
		t.Fatalf("the camera sees %d sub stream sessions, want 1", got)
	}

	// Main stream: from the worker's restream, never a second connection to
	// the camera (spec 16.3). With audio, G.711 arrives as AAC.
	before := s.sim.Sessions("/cam1")
	main := s.watch("cam1", "main", true, 20*time.Second, func(c *liveCapture) bool { return c.fragments >= 45 })
	if main.err != nil || len(main.inits) == 0 || main.fragments < 45 {
		t.Fatalf("main stream: %d fragments, %d init messages, states %v, error %v\n%s", main.fragments, len(main.inits), main.states, main.err, s.logTail())
	}
	if mime, _ := main.inits[0]["mime"].(string); !strings.Contains(mime, "avc1.") || !strings.Contains(mime, "mp4a.40.2") || main.inits[0]["width"] != float64(320) {
		t.Fatalf("main stream init message: %v", main.inits[0])
	}
	if d, ok := decodeMP4(t, main.data.Bytes()); ok && (d.codec != "h264" || d.width != 320 || d.frames < 15 || !d.ptsOK || d.audio != "aac") {
		t.Fatalf("what the main stream viewer received does not decode cleanly: %+v", d)
	}
	if after := s.sim.Sessions("/cam1"); after != before {
		t.Fatalf("watching the main stream changed the camera's sessions from %d to %d", before, after)
	}

	// A camera without a sub stream says so and keeps the socket open.
	nosub := s.watch("cam2", "sub", false, 4*time.Second, func(c *liveCapture) bool { return len(c.states) > 0 })
	if nosub.err != nil || len(nosub.states) == 0 || nosub.fragments != 0 {
		t.Fatalf("camera without a sub stream: states %v, %d fragments, error %v", nosub.states, nosub.fragments, nosub.err)
	}

	// The media process is reported on the System page.
	var sys struct {
		Media api.MediaStatus `json:"media"`
	}
	if status := s.getJSON("/system", &sys); status != 200 || sys.Media.State != "running" || !sys.Media.Enabled {
		t.Fatalf("media status: %d %+v", status, sys.Media)
	}

	// Timeline and playback of the hour being recorded, then of a closed file.
	type timeline struct {
		Segments []struct {
			ID   int64 `json:"id"`
			Open bool  `json:"open"`
		} `json:"segments"`
	}
	type playIndex struct {
		Mime       string       `json:"mime"`
		Init       [2]int64     `json:"init"`
		DurationMs float64      `json:"durationMs"`
		Chunks     [][4]float64 `json:"chunks"`
		Open       bool         `json:"open"`
		Width      int          `json:"width"`
		HasAudio   bool         `json:"hasAudio"`
	}
	rng := "from=" + url.QueryEscape(time.Now().Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))
	var tl timeline
	if status := s.getJSON("/timeline?camera=cam1&"+rng, &tl); status != 200 || len(tl.Segments) != 1 || !tl.Segments[0].Open {
		t.Fatalf("timeline while recording: %d %+v", status, tl)
	}
	play := func(id int64, wantOpen bool) {
		t.Helper()
		var idx playIndex
		deadline := time.Now().Add(15 * time.Second)
		for {
			if status := s.getJSON(fmt.Sprintf("/segments/%d/play/index", id), &idx); status != 200 {
				t.Fatalf("play index: %d\n%s", status, s.logTail())
			}
			if len(idx.Chunks) >= 3 || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if len(idx.Chunks) < 3 || idx.Open != wantOpen || idx.Width != 320 || !idx.HasAudio || !strings.Contains(idx.Mime, "avc1.") {
			t.Fatalf("play index (open=%v): %+v", wantOpen, idx)
		}
		rangeOf := func(off, size int64) []byte {
			status, data := s.get(fmt.Sprintf("/segments/%d/play/data", id), map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, off+size-1)})
			if status != http.StatusPartialContent || int64(len(data)) != size {
				t.Fatalf("range %d+%d: HTTP %d, %d bytes", off, size, status, len(data))
			}
			return data
		}
		var file bytes.Buffer
		file.Write(rangeOf(idx.Init[0], idx.Init[1]))
		for _, c := range idx.Chunks[:3] {
			file.Write(rangeOf(int64(c[2]), int64(c[3])))
		}
		if d, ok := decodeMP4(t, file.Bytes()); ok && (d.codec != "h264" || d.frames < 30 || !d.ptsOK || d.audio != "aac") {
			t.Fatalf("three chunks of a recording do not decode cleanly: %+v", d)
		}
		// Without a Range header the data route refuses.
		if status, _ := s.get(fmt.Sprintf("/segments/%d/play/data", id), nil); status == 200 {
			t.Fatal("the data route served a whole file without a Range header")
		}
		// A thumbnail is a complete MP4 with one picture.
		status, thumb := s.get(fmt.Sprintf("/segments/%d/play/thumb?t=%d", id, int64(idx.Chunks[1][0])+10), nil)
		if status != 200 {
			t.Fatalf("thumbnail: %d", status)
		}
		if d, ok := decodeMP4(t, thumb); ok && (d.frames != 1 || d.width != 320) {
			t.Fatalf("thumbnail: %+v", d)
		}
		// Keyframe-only fragments: one picture per chunk.
		status, kf := s.get(fmt.Sprintf("/segments/%d/play/keyframes?from=0&to=%d", id, int64(idx.Chunks[2][0]+idx.Chunks[2][1])-1), nil)
		if status != 200 {
			t.Fatalf("keyframes: %d", status)
		}
		var kfile bytes.Buffer
		kfile.Write(rangeOf(idx.Init[0], idx.Init[1]))
		kfile.Write(kf)
		if d, ok := decodeMP4(t, kfile.Bytes()); ok && d.frames != 3 {
			t.Fatalf("keyframe mode returned %d pictures for 3 chunks", d.frames)
		}
	}
	play(tl.Segments[0].ID, true)

	s.cut("cam1")
	if status := s.getJSON("/timeline?camera=cam1&"+rng, &tl); status != 200 || len(tl.Segments) != 2 || tl.Segments[0].Open || !tl.Segments[1].Open {
		t.Fatalf("timeline after a cut: %d %+v", status, tl)
	}
	play(tl.Segments[0].ID, false)

	// Nothing of this reached the recorder: no stall, no dropped packets.
	if evs := append(s.events("stream_stalled"), s.events("packets_dropped")...); len(evs) != 0 {
		t.Fatalf("watching disturbed recording: %+v", evs)
	}
}

// Killing the media process repeatedly while someone watches does not affect
// recording, and live view returns within 10 s (spec 16.11).
func TestMediaCrashDoesNotTouchRecording(t *testing.T) {
	s := newLiveSystem(t, liveStreams(), liveCams)
	s.start()
	s.waitRecording("cam1", "cam2")
	s.waitMedia()
	files := map[string]string{}
	for _, cam := range []string{"cam1", "cam2"} {
		files[cam] = s.g.CameraStats()[cam].CurrentFile
	}

	for round := 0; round < 5; round++ {
		kind := []string{"sub", "main"}[round%2]
		first := s.watch("cam1", kind, false, 15*time.Second, func(c *liveCapture) bool { return c.fragments >= 10 })
		if first.fragments < 10 {
			t.Fatalf("round %d: no %s stream before the kill: %v %v\n%s", round, kind, first.err, first.states, s.logTail())
		}
		// A viewer is connected at the moment of the kill.
		gone := make(chan *liveCapture, 1)
		go func() { gone <- s.watch("cam1", kind, false, 20*time.Second, nil) }()
		time.Sleep(300 * time.Millisecond)
		s.g.mu.Lock()
		proc := s.g.media.proc
		s.g.mu.Unlock()
		proc.Kill("test")
		killed := time.Now()
		if c := <-gone; c.err == nil {
			t.Fatalf("round %d: the viewer's stream did not end when the media process died", round)
		}
		// Live view is back within 10 s.
		var back *liveCapture
		for time.Since(killed) < 10*time.Second {
			back = s.watch("cam1", kind, false, 5*time.Second, func(c *liveCapture) bool { return c.fragments >= 5 })
			if back.fragments >= 5 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if back == nil || back.fragments < 5 {
			t.Fatalf("round %d: live view did not return within 10 s of the kill: %+v\n%s", round, back, s.logTail())
		}
		t.Logf("round %d (%s): live view back %.1f s after the kill", round, kind, time.Since(killed).Seconds())
	}

	// Recording went on in the same files the whole time.
	for cam, file := range files {
		st := s.g.CameraStats()[cam]
		if st.State != ipc.StateRecording || st.CurrentFile != file {
			t.Fatalf("%s: recording was disturbed: state %s, file %s (was %s)", cam, st.State, st.CurrentFile, file)
		}
		if n := len(s.segments(cam)); n != 1 {
			t.Fatalf("%s has %d segments; killing the media process must not start a new file", cam, n)
		}
	}
	if evs := append(s.events("stream_stalled"), s.events("worker_restarted")...); len(evs) != 0 {
		t.Fatalf("recorder incidents during the media kills: %+v", evs)
	}
	if evs := s.events("media_restarted"); len(evs) == 0 {
		t.Fatal("the restarts of the media process were not recorded as events")
	}
}

// The two switches of spec 16.8, changed while recording.
func TestLiveSwitches(t *testing.T) {
	s := newLiveSystem(t, liveStreams(), liveCams)
	s.start()
	s.waitRecording("cam1", "cam2")
	s.waitMedia()
	file := s.g.CameraStats()["cam1"].CurrentFile

	if c := s.watch("cam1", "main", false, 15*time.Second, func(c *liveCapture) bool { return c.fragments >= 5 }); c.fragments < 5 {
		t.Fatalf("main stream before the switch: %v %v", c.err, c.states)
	}

	// mainStream off: the restream is gone from the workers; a main viewer
	// is told so instead of getting video, and sub streams still play.
	s.putSystem(`{"live":{"mainStream":false}}`)
	s.until(10*time.Second, "the restream to be switched off", func() bool {
		s.g.mu.Lock()
		defer s.g.mu.Unlock()
		return len(s.g.media.restreams) == 0
	})
	c := s.watch("cam1", "main", false, 5*time.Second, func(c *liveCapture) bool { return len(c.states) > 0 || c.fragments > 0 })
	if c.fragments != 0 || len(c.states) == 0 {
		t.Fatalf("main stream with mainStream off: %d fragments, states %v, error %v", c.fragments, c.states, c.err)
	}
	if c := s.watch("cam1", "sub", false, 15*time.Second, func(c *liveCapture) bool { return c.fragments >= 5 }); c.fragments < 5 {
		t.Fatalf("sub stream with mainStream off: %v %v", c.err, c.states)
	}

	// On again: main streams return without restarting anything.
	s.putSystem(`{"live":{"mainStream":true}}`)
	s.until(15*time.Second, "the main stream to return", func() bool {
		c := s.watch("cam1", "main", false, 5*time.Second, func(c *liveCapture) bool { return c.fragments >= 5 })
		return c.fragments >= 5
	})

	// enabled off: the media process stops and the routes say so.
	s.putSystem(`{"live":{"enabled":false}}`)
	s.until(10*time.Second, "the media process to stop", func() bool { _, _, ok := s.g.Media(); return !ok })
	if c := s.watch("cam1", "sub", false, 3*time.Second, nil); c.err == nil || !strings.Contains(c.err.Error(), "503") {
		t.Fatalf("live view switched off: %v", c.err)
	}
	if st := s.g.MediaStatus(); st.Enabled || st.State != "off" {
		t.Fatalf("media status when switched off: %+v", st)
	}

	// On again.
	s.putSystem(`{"live":{"enabled":true}}`)
	s.waitMedia()
	if c := s.watch("cam1", "sub", false, 15*time.Second, func(c *liveCapture) bool { return c.fragments >= 5 }); c.fragments < 5 {
		t.Fatalf("sub stream after switching live view on again: %v %v", c.err, c.states)
	}

	// None of the switches touched the recording.
	if st := s.g.CameraStats()["cam1"]; st.State != ipc.StateRecording || st.CurrentFile != file {
		t.Fatalf("a live view switch disturbed recording: %s %s (was %s)", st.State, st.CurrentFile, file)
	}
}
