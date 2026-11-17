package worker

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/restream"
	"github.com/jxburros/buttercams/internal/stream"
	"github.com/jxburros/buttercams/test/simulator"
)

const rsToken = "restream-test-token"

// enableRestream switches the worker's restream on and returns its port.
func enableRestream(t *testing.T, h *harness, from int) int {
	t.Helper()
	h.send(&ipc.Restream{Enabled: true, Token: rsToken})
	m, _ := h.wait(5*time.Second, "restream_ready", from, func(m any) bool { _, ok := m.(*ipc.RestreamReady); return ok })
	ready := m.(*ipc.RestreamReady)
	if ready.Port == 0 || ready.WorkerID != 1 || ready.Error != "" {
		t.Fatalf("restream_ready: %+v", ready)
	}
	return ready.Port
}

// msgCount is the index after the last message received so far.
func (h *harness) msgCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

func (h *harness) restreamServer() *restream.Server { return h.w.rs.Load() }

func (h *harness) camera(id string) *Camera {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return h.w.cams[id]
}

// restreamIdle reports whether no trace of a watched camera is left: no
// mount, no pump and no fan-out reader.
func (h *harness) restreamIdle(cam string) bool {
	c := h.camera(cam)
	if c == nil {
		return true
	}
	c.rs.mu.Lock()
	reader := c.rs.reader
	c.rs.mu.Unlock()
	rs := h.restreamServer()
	if rs == nil {
		return reader == nil
	}
	mounts, pumps := rs.Active()
	return reader == nil && mounts == 0 && pumps == 0
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rsClient is a plain RTSP client of the restream, as the media process is.
type rsClient struct {
	c     *gortsplib.Client
	desc  *description.Session
	video format.Format
	audio format.Format
	done  chan struct{}

	packets atomic.Int64
	mu      sync.Mutex
	pkts    []*stream.Packet // kept while keep is set
	keep    bool
	block   chan struct{} // when set, the packet callback waits on it: a client that never reads
}

func rsStatus(err error) base.StatusCode {
	var bad liberrors.ErrClientBadStatusCode
	if errors.As(err, &bad) {
		return bad.Code
	}
	return 0
}

// dialRestream connects and describes rtsp://127.0.0.1:port/<cam>.
func dialRestream(port int, cam, token string) (*rsClient, error) {
	u, err := base.ParseURL(fmt.Sprintf("rtsp://127.0.0.1:%d/%s", port, cam))
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(ipc.RestreamUser, token)
	proto := gortsplib.ProtocolTCP
	r := &rsClient{done: make(chan struct{})}
	r.c = &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &proto, ReadTimeout: 5 * time.Second,
		OnDecodeError: func(error) {}, OnPacketsLost: func(uint64) {}}
	if err := r.c.Start(); err != nil {
		return nil, err
	}
	desc, _, err := r.c.Describe(u)
	if err != nil {
		r.c.Close()
		return nil, err
	}
	r.desc = desc
	return r, nil
}

// play sets up every track and starts playing.
func (r *rsClient) play() error {
	for _, m := range r.desc.Medias {
		if _, err := r.c.Setup(r.desc.BaseURL, m, 0, 0); err != nil {
			r.c.Close()
			return err
		}
		track := stream.TrackVideo
		if m.Type == description.MediaTypeAudio {
			track = stream.TrackAudio
			r.audio = m.Formats[0]
		} else {
			r.video = m.Formats[0]
		}
		media := m
		r.c.OnPacketRTP(m, m.Formats[0], func(pkt *rtp.Packet) {
			r.packets.Add(1)
			r.mu.Lock()
			block := r.block
			if r.keep {
				pts, ok := r.c.PacketPTS(media, pkt)
				r.pkts = append(r.pkts, &stream.Packet{Track: track, RTP: pkt, PTS: pts, PTSValid: ok, Recv: time.Now()})
			}
			r.mu.Unlock()
			if block != nil {
				<-block
			}
		})
	}
	if _, err := r.c.Play(nil); err != nil {
		r.c.Close()
		return err
	}
	go func() {
		r.c.Wait() //nolint:errcheck
		close(r.done)
	}()
	return nil
}

func (r *rsClient) close() { r.c.Close() }

func (r *rsClient) ended(timeout time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// mustPlay connects a client and waits until packets arrive.
func mustPlay(t *testing.T, port int, cam string, keep bool) *rsClient {
	t.Helper()
	var r *rsClient
	var err error
	// The camera may still be connecting: 404 until its session plays.
	eventually(t, 15*time.Second, "the restream to describe "+cam, func() bool {
		r, err = dialRestream(port, cam, rsToken)
		return err == nil
	})
	r.keep = keep
	if err := r.play(); err != nil {
		t.Fatalf("playing the restream of %s: %v", cam, err)
	}
	t.Cleanup(r.close)
	eventually(t, 5*time.Second, "packets from the restream of "+cam, func() bool { return r.packets.Load() > 20 })
	return r
}

// The media process's view of a main stream: the camera's own formats and
// packets, through the worker, without a second connection to the camera.
func TestRestreamServesTheCameraStream(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})
	h := startWorker(t, t.TempDir(), nil)
	port := enableRestream(t, h, 0)
	offline := simCamera(t, sim, "offline", "/nothing-here")
	h.assign(simCamera(t, sim, "cam", "/cam"), offline)
	h.waitOpened(15*time.Second, 0)

	// The first session is announced, for live view to connect to.
	m, _ := h.wait(5*time.Second, "restream_session", 0, func(m any) bool { _, ok := m.(*ipc.RestreamSession); return ok })
	if rs := m.(*ipc.RestreamSession); rs.CameraID != "cam" || rs.Session == 0 {
		t.Fatalf("restream_session: %+v", rs)
	}
	if !h.restreamIdle("cam") {
		t.Fatal("the restream holds a reader although nobody watches")
	}

	if _, err := dialRestream(port, "cam", "wrong"); rsStatus(err) != base.StatusUnauthorized {
		t.Fatalf("wrong token: %v, want RTSP 401", err)
	}
	if _, err := dialRestream(port, "no-such-camera", rsToken); rsStatus(err) != base.StatusNotFound {
		t.Fatalf("unknown camera: %v, want RTSP 404", err)
	}
	if _, err := dialRestream(port, "offline", rsToken); rsStatus(err) != base.StatusNotFound {
		t.Fatalf("camera that is not recording: %v, want RTSP 404", err)
	}

	c := mustPlay(t, port, "cam", true)
	if _, ok := c.video.(*format.H264); !ok {
		t.Fatalf("video format %T, want the camera's H.264", c.video)
	}
	if _, ok := c.audio.(*format.MPEG4Audio); !ok {
		t.Fatalf("audio format %T, want the camera's AAC", c.audio)
	}
	// One client per session.
	if _, err := dialRestream(port, "cam", rsToken); rsStatus(err) != base.StatusServiceUnavailable {
		t.Fatalf("second client for the same session: %v, want RTSP 503", err)
	}

	// What arrives is the camera's stream: it reassembles into frames.
	time.Sleep(3 * time.Second)
	c.mu.Lock()
	pkts := c.pkts
	c.keep = false
	c.mu.Unlock()
	proc, err := stream.NewProcessor(c.video, c.audio)
	if err != nil {
		t.Fatal(err)
	}
	var frames, keys, audio int
	var lastDTS int64
	for _, p := range pkts {
		for _, u := range proc.Push(p) {
			switch {
			case !u.Video:
				audio++
			default:
				if frames > 0 && u.DTS-lastDTS != 90000/simulator.FPS {
					t.Fatalf("frame %d is %d ticks after the previous one", frames, u.DTS-lastDTS)
				}
				lastDTS = u.DTS
				frames++
				if u.Key {
					keys++
				}
			}
		}
	}
	if frames < 2*simulator.FPS || keys < 2 || audio < 20 {
		t.Fatalf("restream delivered %d frames, %d keyframes, %d audio units in 3 s", frames, keys, audio)
	}
	if proc.Width != 320 || proc.Height != 240 {
		t.Fatalf("picture size %dx%d", proc.Width, proc.Height)
	}
	// Never a second connection to the camera for live view (spec 16.3).
	if n := sim.Sessions("/cam"); n != 1 {
		t.Fatalf("the camera has %d sessions with a viewer attached, want 1", n)
	}

	// The reader exists only while the client is connected.
	if h.restreamIdle("cam") {
		t.Fatal("no reader while a client is playing")
	}
	c.close()
	eventually(t, 5*time.Second, "the reader to be removed after the client left", func() bool { return h.restreamIdle("cam") })
	// ... and the next client is served again.
	c2 := mustPlay(t, port, "cam", false)
	c2.close()
	eventually(t, 5*time.Second, "the reader to be removed again", func() bool { return h.restreamIdle("cam") })

	h.stop()
	closed, _ := h.waitClosed(time.Second, 0)
	checkClosedFile(t, closed)
	ffprobe(t, closed.Path)
	for _, m := range h.all() {
		if s, ok := m.(*ipc.CameraStats); ok && (s.DroppedPackets != 0 || s.RestreamDropped != 0) {
			t.Fatalf("packets were dropped with a healthy viewer: %+v", s)
		}
	}
}

// frameTable lists the video samples of a recording: time from the first
// one, duration, size and flags, exactly as stored.
func frameTable(t *testing.T, path string) []string {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	out, err := exec.Command(bin, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"packet=pts,dts,duration,size,flags", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var rows []string
	var first int64
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var pts, dts, dur, size int64
		var flags string
		line = strings.ReplaceAll(strings.TrimRight(line, ","), ",", " ")
		if _, err := fmt.Sscan(line, &pts, &dts, &dur, &size, &flags); err != nil {
			t.Fatalf("ffprobe line %q: %v", line, err)
		}
		if i == 0 {
			first = dts
		}
		rows = append(rows, fmt.Sprintf("pts=%d dts=%d dur=%d size=%d %s", pts-first, dts-first, dur, size, flags))
	}
	return rows
}

// The recorder never waits for the restream (spec 16.3, 16.9). Three cameras
// record the same stream: one unwatched, one with a restream client that
// stops reading, one whose restream pump is stuck. Their files must hold the
// same frames with the same timing, and the stuck restream must count what
// it dropped. The simulator repeats one second of video, so recordings that
// started at different keyframes still compare sample for sample.
func TestRecordingIsIdenticalWithAStuckRestream(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", PadBytes: 33000})
	h := startWorker(t, t.TempDir(), nil)
	release := make(chan struct{})
	var released sync.Once
	free := func() { released.Do(func() { close(release) }) }
	t.Cleanup(free)
	hook := func(cam string) {
		if cam == "stuck-pump" {
			<-release
		}
	}
	h.w.testPumpHook.Store(&hook)
	port := enableRestream(t, h, 0)
	h.assign(simCamera(t, sim, "plain", "/cam"), simCamera(t, sim, "stuck-client", "/cam"), simCamera(t, sim, "stuck-pump", "/cam"))
	from := 0
	for i := 0; i < 3; i++ {
		_, from = h.waitOpened(15*time.Second, from)
	}

	deaf := mustPlay(t, port, "stuck-client", false)
	deaf.mu.Lock()
	deaf.block = release // from now on it reads nothing
	deaf.mu.Unlock()
	var stuck *rsClient
	eventually(t, 10*time.Second, "a client on the camera with the stuck pump", func() bool {
		var err error
		if stuck, err = dialRestream(port, "stuck-pump", rsToken); err != nil {
			return false
		}
		return stuck.play() == nil
	})
	t.Cleanup(stuck.close)

	// The pump of "stuck-pump" takes one packet and never returns: its
	// bounded reader fills and drops, and the count is reported.
	m, _ := h.wait(30*time.Second, "restreamDropped in camera_stats", 0, func(m any) bool {
		s, ok := m.(*ipc.CameraStats)
		return ok && s.CameraID == "stuck-pump" && s.RestreamDropped > 0
	})
	if s := m.(*ipc.CameraStats); s.State != ipc.StateRecording || s.DroppedPackets != 0 {
		t.Fatalf("the recorder was affected by the stuck restream: %+v", s)
	}
	time.Sleep(3 * time.Second)

	free()
	h.stop()
	files := map[string]*ipc.SegmentClosed{}
	for _, m := range h.all() {
		switch m := m.(type) {
		case *ipc.SegmentClosed:
			if files[m.CameraID] != nil {
				t.Fatalf("camera %s wrote more than one file", m.CameraID)
			}
			files[m.CameraID] = m
		case *ipc.CameraStats:
			if m.DroppedPackets != 0 {
				t.Fatalf("the file writer of %s dropped packets", m.CameraID)
			}
			if m.CameraID != "stuck-pump" && m.RestreamDropped != 0 {
				t.Fatalf("%s reports restream drops: %+v", m.CameraID, m)
			}
		case *ipc.Event:
			if m.Severity != ipc.SevInfo {
				t.Fatalf("unexpected event: %+v", m)
			}
		}
	}
	if len(files) != 3 {
		t.Fatalf("%d files, want one per camera", len(files))
	}
	want := frameTable(t, files["plain"].Path)
	if len(want) < 5*simulator.FPS {
		t.Fatalf("the reference recording holds only %d frames", len(want))
	}
	for _, cam := range []string{"stuck-client", "stuck-pump"} {
		checkClosedFile(t, files[cam])
		got := frameTable(t, files[cam].Path)
		n := len(want)
		if len(got) < n {
			n = len(got)
		}
		n-- // the last frame of a file gets an estimated duration
		if n < 5*simulator.FPS {
			t.Fatalf("%s holds only %d frames", cam, len(got))
		}
		// Recordings start within a second or two of each other and all
		// stop together, so their lengths differ by whole seconds at most.
		if d := len(want) - len(got); d > 3*simulator.FPS || d < -3*simulator.FPS {
			t.Fatalf("%s holds %d frames, the unwatched camera %d", cam, len(got), len(want))
		}
		for i := 0; i < n; i++ {
			if got[i] != want[i] {
				t.Fatalf("%s differs from the unwatched recording at frame %d:\n got  %s\n want %s", cam, i, got[i], want[i])
			}
		}
		ffprobe(t, files[cam].Path)
	}
}

// Across the hourly rollover the restream offers each session as its own
// stream and reports the second one, so that live view can move over without
// a freeze; the recording itself is as gapless as without a viewer.
func TestRestreamAcrossHourlyRollover(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})
	now, boundary := clockBefore(14 * time.Second)
	h := startWorker(t, t.TempDir(), now)
	port := enableRestream(t, h, 0)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(10*time.Second, 0)
	m, idx := h.wait(5*time.Second, "restream_session", 0, func(m any) bool { _, ok := m.(*ipc.RestreamSession); return ok })
	firstSession := m.(*ipc.RestreamSession).Session

	old := mustPlay(t, port, "cam", false)

	// The second session is reported as soon as it plays.
	m, _ = h.wait(15*time.Second, "restream_session for the second session", idx, func(m any) bool {
		_, ok := m.(*ipc.RestreamSession)
		return ok
	})
	if second := m.(*ipc.RestreamSession); second.Session == firstSession || second.CameraID != "cam" {
		t.Fatalf("second restream_session: %+v (first was %d)", second, firstSession)
	}
	// A client that connects now gets the new session, while the first
	// client keeps receiving the old one.
	fresh, err := dialRestream(port, "cam", rsToken)
	if err != nil {
		t.Fatalf("connecting to the second session while the first is watched: %v", err)
	}
	if err := fresh.play(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.close)
	eventually(t, 5*time.Second, "packets of the second session", func() bool { return fresh.packets.Load() > 20 })
	before := old.packets.Load()
	time.Sleep(500 * time.Millisecond)
	if old.ended(0) || old.packets.Load() == before {
		t.Fatal("the first client stopped receiving before the old session ended")
	}

	closed, cidx := h.waitClosed(25*time.Second, 0)
	second, _ := h.waitOpened(5*time.Second, cidx-1)
	if closed.Reason != ipc.ReasonHourRollover || second.Gap {
		t.Fatalf("rollover with a viewer: reason %q, gap %v", closed.Reason, second.Gap)
	}
	if !hourOf(second.Path).Equal(boundary) {
		t.Fatalf("second file %s is not the new hour's file", filepath.Base(second.Path))
	}
	if seam := second.Start.Sub(closed.End.Time); seam > 500*time.Millisecond || seam < -500*time.Millisecond {
		t.Fatalf("seam between the files is %v with a viewer attached", seam)
	}
	checkClosedFile(t, closed)
	ffprobe(t, closed.Path)

	// The old session ends: its client's connection ends; the new one plays on.
	if !old.ended(5 * time.Second) {
		t.Fatal("the client of the old session was not disconnected when that session ended")
	}
	before = fresh.packets.Load()
	time.Sleep(time.Second)
	if fresh.ended(0) || fresh.packets.Load() < before+10 {
		t.Fatal("the client of the new session stopped receiving after the switch")
	}
	for _, m := range h.all() {
		if e, ok := m.(*ipc.Event); ok && e.Severity != ipc.SevInfo {
			t.Fatalf("unexpected event during a rollover with viewers: %+v", e)
		}
		if s, ok := m.(*ipc.CameraStats); ok && (s.DroppedPackets != 0 || s.RestreamDropped != 0) {
			t.Fatalf("packets were dropped: %+v", s)
		}
	}
	fresh.close()
	eventually(t, 5*time.Second, "the reader to be removed", func() bool { return h.restreamIdle("cam") })
}

// A camera that allows one session only never sees a second one because of
// a viewer; at the hour the viewer's stream ends with the session and a new
// one can be opened on the next.
func TestRestreamSingleSessionCamera(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", SingleSession: true})
	now, _ := clockBefore(13 * time.Second)
	h := startWorker(t, t.TempDir(), now)
	port := enableRestream(t, h, 0)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(10*time.Second, 0)
	c := mustPlay(t, port, "cam", false)

	var most atomic.Int32
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if n := int32(sim.Sessions("/cam")); n > most.Load() {
					most.Store(n)
				}
			}
		}
	}()

	closed, idx := h.waitClosed(25*time.Second, 0)
	if closed.Reason != ipc.ReasonHourRollover {
		t.Fatalf("close reason %q", closed.Reason)
	}
	if !c.ended(5 * time.Second) {
		t.Fatal("the viewer's stream did not end with the camera's session")
	}
	second, _ := h.waitOpened(15*time.Second, idx)
	if !second.Gap {
		t.Fatal("break-before-make must record the gap")
	}
	c2 := mustPlay(t, port, "cam", false)
	time.Sleep(500 * time.Millisecond)
	c2.close()
	if n := most.Load(); n != 1 {
		t.Fatalf("the camera saw %d sessions at once, want 1", n)
	}
	checkClosedFile(t, closed)
}

// live.mainStream can be switched while recording. Off removes every trace of
// the restream; neither switch touches the recording.
func TestRestreamSwitchedOffAndOn(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	h := startWorker(t, t.TempDir(), nil)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	h.waitOpened(15*time.Second, 0)

	// Never switched on: nothing of the restream exists and no session is
	// announced.
	if h.restreamServer() != nil {
		t.Fatal("a restream server exists although it was never switched on")
	}
	for _, m := range h.all() {
		if _, ok := m.(*ipc.RestreamSession); ok {
			t.Fatal("restream_session sent while the restream is off")
		}
		if _, ok := m.(*ipc.RestreamReady); ok {
			t.Fatal("restream_ready sent although nothing asked")
		}
	}

	port := enableRestream(t, h, 0)
	c := mustPlay(t, port, "cam", false)

	// The same message again (the guardian sends it after every assignment)
	// answers with the same port and leaves the client alone.
	if again := enableRestream(t, h, h.msgCount()); again != port {
		t.Fatalf("a repeated restream message moved the port from %d to %d", port, again)
	}
	before := c.packets.Load()
	time.Sleep(500 * time.Millisecond)
	if c.ended(0) || c.packets.Load() == before {
		t.Fatal("a repeated restream message disconnected the client")
	}

	// Off: port 0, the client is disconnected, nothing is left.
	from := h.msgCount()
	h.send(&ipc.Restream{Enabled: false})
	m, _ := h.wait(5*time.Second, "restream_ready after switching off", from, func(m any) bool { _, ok := m.(*ipc.RestreamReady); return ok })
	if ready := m.(*ipc.RestreamReady); ready.Port != 0 || ready.Error != "" {
		t.Fatalf("restream_ready after switching off: %+v", ready)
	}
	if !c.ended(5 * time.Second) {
		t.Fatal("the client was not disconnected when the restream was switched off")
	}
	eventually(t, 5*time.Second, "every trace of the restream to go", func() bool {
		return h.restreamServer() == nil && h.restreamIdle("cam")
	})
	eventually(t, 5*time.Second, "the port to close", func() bool {
		_, err := dialRestream(port, "cam", rsToken)
		return err != nil && rsStatus(err) == 0
	})

	// On again, with a new token: a new server, and the stream is back.
	from = h.msgCount()
	h.send(&ipc.Restream{Enabled: true, Token: "second-token"})
	m, _ = h.wait(5*time.Second, "restream_ready after switching on", from, func(m any) bool { _, ok := m.(*ipc.RestreamReady); return ok })
	port2 := m.(*ipc.RestreamReady).Port
	if port2 == 0 {
		t.Fatalf("restream_ready after switching on again: %+v", m)
	}
	if _, err := dialRestream(port2, "cam", rsToken); rsStatus(err) != base.StatusUnauthorized {
		t.Fatalf("the old token still works: %v", err)
	}
	c2, err := dialRestream(port2, "cam", "second-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.play(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "packets after switching on again", func() bool { return c2.packets.Load() > 20 })
	c2.close()

	// The recording never noticed: still the first file, closed at shutdown.
	h.stop()
	var closed []*ipc.SegmentClosed
	opened := 0
	for _, m := range h.all() {
		switch m := m.(type) {
		case *ipc.SegmentClosed:
			closed = append(closed, m)
		case *ipc.SegmentOpened:
			opened++
		case *ipc.Event:
			if m.Severity != ipc.SevInfo {
				t.Fatalf("unexpected event: %+v", m)
			}
		}
	}
	if opened != 1 || len(closed) != 1 || closed[0].Reason != ipc.ReasonShutdown {
		t.Fatalf("switching the restream touched the recording: %d files opened, closed %+v", opened, closed)
	}
	checkClosedFile(t, closed[0])
	ffprobe(t, closed[0].Path)
}

// A failure inside the restream is contained: the camera's restream goes off
// until its next session, an event says so, and the recording carries on in
// the same file (spec 16.3).
func TestRestreamPanicNeverTouchesTheRecording(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	h := startWorker(t, t.TempDir(), nil)
	var explode atomic.Bool
	hook := func(string) {
		if explode.Load() {
			panic("restream test failure")
		}
	}
	h.w.testPumpHook.Store(&hook)
	port := enableRestream(t, h, 0)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	_, from := h.waitOpened(15*time.Second, 0)

	const rounds = 4
	for round := 1; round <= rounds; round++ {
		c := mustPlay(t, port, "cam", false)
		mark := h.msgCount()
		explode.Store(true)
		if !c.ended(5 * time.Second) {
			t.Fatalf("round %d: the client was not disconnected after the pump failed", round)
		}
		explode.Store(false)
		m, _ := h.wait(5*time.Second, "restream_failed event", mark, func(m any) bool {
			e, ok := m.(*ipc.Event)
			return ok && e.Code == "restream_failed"
		})
		if e := m.(*ipc.Event); e.Severity != ipc.SevWarning || e.CameraID != "cam" {
			t.Fatalf("event: %+v", e)
		}
		eventually(t, 5*time.Second, "the failed restream to be cleaned up", func() bool { return h.restreamIdle("cam") })
		// Off for this session ...
		if _, err := dialRestream(port, "cam", rsToken); rsStatus(err) != base.StatusNotFound {
			t.Fatalf("round %d: the restream is offered again in the same session: %v", round, err)
		}
		// ... and the recording did not notice: no file was closed.
		time.Sleep(time.Second)
		for _, m := range h.all()[mark:] {
			if sc, ok := m.(*ipc.SegmentClosed); ok {
				t.Fatalf("round %d: the restream failure closed a file: %+v", round, sc)
			}
			if s, ok := m.(*ipc.CameraStats); ok && s.State != ipc.StateRecording {
				t.Fatalf("round %d: the camera left the recording state: %+v", round, s)
			}
		}
		// The camera's next session brings the restream back.
		h.send(&ipc.RestartCamera{CameraID: "cam", Reason: "test: next session"})
		_, from = h.waitOpened(20*time.Second, from)
	}
	c := mustPlay(t, port, "cam", false)
	c.close()

	h.stop()
	n := 0
	for _, m := range h.all() {
		if sc, ok := m.(*ipc.SegmentClosed); ok {
			n++
			checkClosedFile(t, sc)
			ffprobe(t, sc.Path)
		}
		if e, ok := m.(*ipc.Event); ok && e.Code == "camera_panic" {
			t.Fatalf("a restream failure reached the camera goroutine: %+v", e)
		}
	}
	if n != rounds+1 {
		t.Fatalf("%d complete files, want %d", n, rounds+1)
	}
	if parts := partFiles(t, h.staging); len(parts) != 0 {
		t.Fatalf("in-progress files left behind: %v", parts)
	}
}

// What a watched camera costs the recorder: one more non-blocking channel
// send per packet in Fanout.Publish. The copy into the RTSP server's write
// queue is paid by the restream's own goroutine.
func BenchmarkPublish(b *testing.B) {
	drain := func(r *stream.Reader, stop chan struct{}) {
		for {
			select {
			case <-r.C:
			case <-stop:
				return
			}
		}
	}
	pkt := &stream.Packet{RTP: &rtp.Packet{Payload: make([]byte, 1400)}}
	for _, readers := range []int{1, 2} {
		name := "file-writer-only"
		if readers == 2 {
			name = "with-restream-reader"
		}
		b.Run(name, func(b *testing.B) {
			f := &stream.Fanout{}
			stop := make(chan struct{})
			defer close(stop)
			go drain(f.Register("file-writer", writerQueue), stop)
			if readers == 2 {
				go drain(f.Register("restream", 1024), stop)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f.Publish(pkt)
			}
		})
	}
}
