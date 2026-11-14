package worker

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/test/simulator"
)

// recordFor records one simulated camera for d and returns the closed file.
func recordFor(t *testing.T, stream simulator.Stream, d time.Duration) (*ipc.SegmentClosed, *harness) {
	t.Helper()
	sim := startSim(t, stream)
	h := startWorker(t, t.TempDir(), nil)
	h.assign(simCamera(t, sim, "cam", stream.Path))
	h.waitOpened(15*time.Second, 0)
	time.Sleep(d)
	h.stop()
	closed, _ := h.waitClosed(time.Second, 0)
	return closed, h
}

func checkClosedFile(t *testing.T, c *ipc.SegmentClosed) mp4meta.Info {
	t.Helper()
	if size := mustExist(t, c.Path); size != c.Bytes {
		t.Fatalf("reported %d bytes, file has %d", c.Bytes, size)
	}
	if !strings.HasSuffix(c.Path, ".mp4") || strings.HasSuffix(c.Path, ".part.mp4") {
		t.Fatalf("closed file has the wrong name: %s", c.Path)
	}
	sum, _, err := mp4meta.HashFile(c.Path)
	if err != nil || sum != c.SHA256 {
		t.Fatalf("streaming SHA-256 %s does not match the file (%s, %v)", c.SHA256, sum, err)
	}
	info, err := mp4meta.Read(c.Path)
	if err != nil {
		t.Fatalf("reading metadata: %v", err)
	}
	if info.CameraID != c.CameraID || !info.Start.Equal(c.Start.Time) || !info.End().Equal(c.End.Time) ||
		info.CloseReason != c.Reason || info.Gap != c.Gap || info.VideoCodec != c.VideoCodec || info.AudioCodec != c.AudioCodec {
		t.Fatalf("metadata in the file does not match the report:\n file  %+v end=%v\n report %+v", info, info.End(), c)
	}
	p, ok := naming.Parse(filepath.Base(c.Path))
	if !ok || p.CameraID != c.CameraID || !p.HourStart.Equal(naming.HourStart(c.Start.Time)) {
		t.Fatalf("file name %s does not match its start %v", filepath.Base(c.Path), c.Start.Time)
	}
	return info
}

func TestRecordH264WithAAC(t *testing.T) {
	t.Parallel()
	c, h := recordFor(t, simulator.Stream{Path: "/cam", Audio: "aac"}, 7*time.Second)
	if c.Reason != ipc.ReasonShutdown || !c.Gap || c.VideoCodec != "h264" || c.AudioCodec != "aac" {
		t.Fatalf("unexpected close report %+v", c)
	}
	checkClosedFile(t, c)
	if parts := partFiles(t, h.staging); len(parts) != 0 {
		t.Fatalf("in-progress files left behind: %v", parts)
	}

	p := ffprobe(t, c.Path)
	if p.codec("video") != "h264" || p.codec("audio") != "aac" {
		t.Fatalf("ffprobe sees %q/%q", p.codec("video"), p.codec("audio"))
	}
	dur := c.End.Sub(c.Start.Time).Seconds()
	if dur < 6 || dur > 9 {
		t.Fatalf("recorded %.1f s, expected about 7", dur)
	}
	if math.Abs(p.duration()-dur) > 0.3 {
		t.Fatalf("ffprobe duration %.2f s, index says %.2f s", p.duration(), dur)
	}
	if frames := p.frames("video"); math.Abs(float64(frames)-dur*simulator.FPS) > 3 {
		t.Fatalf("%d video frames in %.2f s at %d fps", frames, dur, simulator.FPS)
	}
	if got := p.Format.Tags[mp4meta.KeyCamera]; got != "cam" {
		t.Fatalf("tag %s = %q (tags: %v)", mp4meta.KeyCamera, got, p.Format.Tags)
	}
	if got := p.Format.Tags[mp4meta.KeyStart]; got != c.Start.Format(mp4meta.TimeFormat) {
		t.Fatalf("tag %s = %q, want %q", mp4meta.KeyStart, got, c.Start.Format(mp4meta.TimeFormat))
	}

	// Statistics reported while recording.
	var st *ipc.CameraStats
	for _, m := range h.all() {
		if s, ok := m.(*ipc.CameraStats); ok && s.State == ipc.StateRecording && s.FPS > 0 {
			st = s
		}
	}
	if st == nil {
		t.Fatal("no camera_stats in state recording")
	}
	if st.Width != 320 || st.Height != 240 || st.VideoCodec != "h264" || st.AudioCodec != "aac" ||
		math.Abs(st.FPS-simulator.FPS) > 2 || st.KeyframeIntervalMs < 900 || st.KeyframeIntervalMs > 1100 ||
		st.BitrateKbps <= 0 || st.CurrentFile == "" || st.LastPacketTS.IsZero() {
		t.Fatalf("unexpected stats %+v", st)
	}
	hb := false
	for _, m := range h.all() {
		if b, ok := m.(*ipc.Heartbeat); ok && b.WorkerID == 1 && b.MemBytes > 0 {
			hb = true
		}
	}
	if !hb {
		t.Fatal("no heartbeat with memory usage")
	}
}

func TestRecordH265NoAudio(t *testing.T) {
	t.Parallel()
	c, _ := recordFor(t, simulator.Stream{Path: "/cam", Video: "h265"}, 3*time.Second)
	if c.VideoCodec != "h265" || c.AudioCodec != "" {
		t.Fatalf("unexpected codecs %+v", c)
	}
	checkClosedFile(t, c)
	p := ffprobe(t, c.Path)
	if p.codec("video") != "hevc" || p.codec("audio") != "" {
		t.Fatalf("ffprobe sees %q/%q", p.codec("video"), p.codec("audio"))
	}
}

// G.711 and G.726 are the only streams ButterCams transcodes: to AAC.
func TestTelephoneAudioIsStoredAsAAC(t *testing.T) {
	for _, codec := range []string{"pcmu", "pcma", "g726"} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			c, _ := recordFor(t, simulator.Stream{Path: "/cam", Audio: codec}, 4*time.Second)
			if c.AudioCodec != "aac" {
				t.Fatalf("audio codec %q, want aac", c.AudioCodec)
			}
			checkClosedFile(t, c)
			p := ffprobe(t, c.Path)
			if p.codec("audio") != "aac" || p.codec("video") != "h264" {
				t.Fatalf("ffprobe sees %q/%q", p.codec("video"), p.codec("audio"))
			}
			// About 46.9 AAC frames per second; allow for the first second.
			dur := c.End.Sub(c.Start.Time).Seconds()
			if n := p.frames("audio"); float64(n) < (dur-1.5)*46 {
				t.Fatalf("only %d audio frames in %.1f s", n, dur)
			}
		})
	}
}

func TestUnsupportedAudioRecordsVideoOnly(t *testing.T) {
	t.Parallel()
	c, h := recordFor(t, simulator.Stream{Path: "/cam", Audio: "speex"}, 3*time.Second)
	if c.AudioCodec != "" {
		t.Fatalf("audio codec %q, want none", c.AudioCodec)
	}
	checkClosedFile(t, c)
	ev := h.waitEvent(time.Second, "audio_not_recorded")
	if !strings.HasPrefix(ev.Message, "audio not recorded: ") {
		t.Fatalf("unexpected message %q", ev.Message)
	}
	warned := 0
	for _, m := range h.all() {
		if e, ok := m.(*ipc.Event); ok && e.Code == "audio_not_recorded" {
			warned++
		}
		if s, ok := m.(*ipc.CameraStats); ok && s.State == ipc.StateRecording && !strings.HasPrefix(s.AudioNote, "audio not recorded: ") {
			t.Fatalf("status does not explain the missing audio: %+v", s)
		}
	}
	if warned != 1 {
		t.Fatalf("warning logged %d times, want once per connection", warned)
	}
}

func TestRecordAudioFalseSkipsAudio(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})
	h := startWorker(t, t.TempDir(), nil)
	cam := simCamera(t, sim, "cam", "/cam")
	cam.RecordAudio = false
	h.assign(cam)
	h.waitOpened(15*time.Second, 0)
	time.Sleep(2 * time.Second)
	h.stop()
	c, _ := h.waitClosed(time.Second, 0)
	if c.AudioCodec != "" {
		t.Fatalf("audio was recorded although recordAudio is false")
	}
	if p := ffprobe(t, c.Path); p.codec("audio") != "" {
		t.Fatalf("file has an audio track")
	}
}

func TestAuthenticationFailureAndRecovery(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Username: "admin", Password: "right"})
	h := startWorker(t, t.TempDir(), nil)
	cam := simCamera(t, sim, "cam", "/cam")
	cam.Username = "admin"
	cam.PasswordEnc, _ = configProtect("wrong")
	h.assign(cam)
	st := h.waitState(15*time.Second, ipc.StateError)
	if st.Message != "Authentication failed" {
		t.Fatalf("message %q", st.Message)
	}
	h.waitEvent(time.Second, "auth_failed")

	// Correcting the password takes effect without waiting for the retry.
	cam.PasswordEnc, _ = configProtect("right")
	h.send(&ipc.UpdateCamera{Camera: cam})
	h.waitOpened(15*time.Second, 0)
}

func TestUnsupportedVideoCodec(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Video: "mjpeg"})
	h := startWorker(t, t.TempDir(), nil)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	st := h.waitState(15*time.Second, ipc.StateError)
	want := "Camera sends MJPEG; ButterCams v1 records H.264 or H.265 only. Change the camera's encoding setting."
	if st.Message != want {
		t.Fatalf("message %q\nwant    %q", st.Message, want)
	}
}

func TestUnreachableCameraBacksOff(t *testing.T) {
	t.Parallel()
	h := startWorker(t, t.TempDir(), nil)
	cam := simCamera(t, startSim(t), "cam", "/nothing")
	h.assign(cam)
	st := h.waitState(15*time.Second, ipc.StateBackoff)
	if st.Message == "" {
		t.Fatal("backoff without a reason")
	}
	h.waitEvent(time.Second, "camera_unreachable")
}

func TestDisabledCameraDoesNotConnect(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	h := startWorker(t, t.TempDir(), nil)
	cam := simCamera(t, sim, "cam", "/cam")
	cam.Enabled = false
	h.assign(cam)
	h.waitState(10*time.Second, ipc.StateDisabled)
	if n := sim.Sessions("/cam"); n != 0 {
		t.Fatalf("disabled camera has %d sessions", n)
	}
	// Enabling starts recording; disabling again closes the file.
	cam.Enabled = true
	h.send(&ipc.UpdateCamera{Camera: cam})
	h.waitOpened(15*time.Second, 0)
	cam.Enabled = false
	h.send(&ipc.UpdateCamera{Camera: cam})
	c, _ := h.waitClosed(10*time.Second, 0)
	checkClosedFile(t, c)
}

// A stall closes the file cleanly, marks the gap and the camera comes back
// within 15 s of the stream returning (spec 6.1, 9, 12).
func TestStallRecovery(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "pcmu"})
	h := startWorker(t, t.TempDir(), nil)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	first, _ := h.waitOpened(15*time.Second, 0)
	time.Sleep(2 * time.Second)

	stallStart := time.Now()
	sim.Stall("/cam", 14*time.Second)
	closed, idx := h.waitClosed(20*time.Second, 0)
	detect := time.Since(stallStart)
	if closed.Reason != ipc.ReasonStall {
		t.Fatalf("close reason %q, want stall", closed.Reason)
	}
	if detect < 9*time.Second || detect > 13*time.Second {
		t.Fatalf("stall detected after %v, want about 10 s", detect)
	}
	checkClosedFile(t, closed)
	ffprobe(t, closed.Path)
	h.waitEvent(time.Second, "stream_stalled")
	h.waitState(10*time.Second, ipc.StateBackoff)

	streamBack := stallStart.Add(14 * time.Second)
	second, _ := h.waitOpened(40*time.Second, idx)
	if back := time.Since(streamBack); back > 15*time.Second {
		t.Fatalf("recording resumed %v after the stream returned, want under 15 s", back)
	}
	if !second.Gap {
		t.Fatal("the file after a stall must be marked with a gap")
	}
	// Same hour → the new file gets the _b suffix (unless the hour just rolled).
	p1, _ := naming.Parse(filepath.Base(first.Path))
	p2, _ := naming.Parse(filepath.Base(second.Path))
	if p1.HourStart.Equal(p2.HourStart) && p2.Suffix != "b" {
		t.Fatalf("second file of the hour is %s, want the _b suffix", filepath.Base(second.Path))
	}
	h.waitEvent(5*time.Second, "camera_recovered")
}

// A file cut off by a crash is repaired, renamed, reported with gap=true and
// stays playable; the new recording gets the next suffix (spec 6.4).
func TestCrashRecoveryOfPartFile(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam", Audio: "aac"})

	// Record for a while, then take a copy of the file as it is on disk and
	// cut it mid-fragment: exactly what a power loss leaves behind.
	h1 := startWorker(t, t.TempDir(), nil)
	h1.assign(simCamera(t, sim, "cam", "/cam"))
	opened, _ := h1.waitOpened(15*time.Second, 0)
	time.Sleep(4 * time.Second)
	raw, err := os.ReadFile(opened.Path)
	if err != nil {
		t.Fatal(err)
	}
	h1.stop()
	if len(raw) < 20000 {
		t.Fatalf("only %d bytes on disk after 4 s: fragments are not being written", len(raw))
	}
	cut := raw[:len(raw)-777]

	staging := t.TempDir()
	rel, _ := filepath.Rel(h1.staging, opened.Path)
	crashed := filepath.Join(staging, rel)
	os.MkdirAll(filepath.Dir(crashed), 0o755)
	if err := os.WriteFile(crashed, cut, 0o644); err != nil {
		t.Fatal(err)
	}

	h2 := startWorker(t, staging, nil)
	h2.assign(simCamera(t, sim, "cam", "/cam"))
	m, _ := h2.wait(15*time.Second, "crash_recovered segment", 0, func(m any) bool {
		c, ok := m.(*ipc.SegmentClosed)
		return ok && c.Reason == ipc.ReasonCrashRecovered
	})
	rec := m.(*ipc.SegmentClosed)
	if !rec.Gap {
		t.Fatal("a recovered file must be reported with gap=true")
	}
	if rec.Path != filepath.Join(filepath.Dir(crashed), naming.FinalName(filepath.Base(crashed))) {
		t.Fatalf("recovered file is %s", rec.Path)
	}
	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Fatal("the .part.mp4 file was not renamed")
	}
	info, err := mp4meta.Read(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.CloseReason != ipc.ReasonCrashRecovered || !info.Start.Equal(opened.Start.Time) ||
		!rec.Start.Equal(opened.Start.Time) || !info.End().Equal(rec.End.Time) {
		t.Fatalf("recovered metadata wrong: %+v vs %+v", info, rec)
	}
	sum, size, _ := mp4meta.HashFile(rec.Path)
	if sum != rec.SHA256 || size != rec.Bytes {
		t.Fatal("recovered file hash or size does not match the report")
	}
	if size >= int64(len(cut)+4096) || size < int64(len(cut))/2 {
		t.Fatalf("recovered file is %d bytes from a %d byte crash file", size, len(cut))
	}
	// Under 3 s of video lost (spec 12): the copy had 4 s, one fragment is cut.
	if d := rec.End.Sub(rec.Start.Time); d < time.Second || d > 5*time.Second {
		t.Fatalf("recovered duration %v", d)
	}
	p := ffprobe(t, rec.Path) // plays, including the crash cut
	if p.codec("video") != "h264" {
		t.Fatalf("recovered file has no playable video")
	}

	// The new recording must not reuse the recovered file's name.
	newOpen, _ := h2.waitOpened(15*time.Second, 0)
	pOld, _ := naming.Parse(filepath.Base(rec.Path))
	pNew, _ := naming.Parse(filepath.Base(newOpen.Path))
	if pOld.HourStart.Equal(pNew.HourStart) && pNew.Index != pOld.Index+1 {
		t.Fatalf("new file %s after recovered %s", filepath.Base(newOpen.Path), filepath.Base(rec.Path))
	}
}

func TestEmptyPartFileIsRemoved(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	staging := t.TempDir()
	hour := naming.HourStart(time.Now())
	dir := filepath.Join(staging, "cam", hour.Format("2006-01-02"))
	os.MkdirAll(dir, 0o755)
	junk := filepath.Join(dir, naming.PartName(naming.FileName("cam", hour, 0)))
	os.WriteFile(junk, []byte{0, 0, 0, 24, 'f', 't', 'y', 'p'}, 0o644)
	foreign := filepath.Join(dir, "holiday.mp4")
	os.WriteFile(foreign, []byte("not ours"), 0o644)

	h := startWorker(t, staging, nil)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	opened, _ := h.waitOpened(15*time.Second, 0)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatal("an empty interrupted file should be removed")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("a file ButterCams did not write was touched")
	}
	if p, _ := naming.Parse(filepath.Base(opened.Path)); p.Index != 1 {
		t.Fatalf("new file %s should not reuse the interrupted file's name", filepath.Base(opened.Path))
	}
}

func TestClockJumpStartsNewFileAtKeyframe(t *testing.T) {
	t.Parallel()
	sim := startSim(t, simulator.Stream{Path: "/cam"})
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	h := startWorker(t, t.TempDir(), now)
	h.assign(simCamera(t, sim, "cam", "/cam"))
	first, _ := h.waitOpened(15*time.Second, 0)
	time.Sleep(2 * time.Second)

	// The wall clock is set three hours ahead while the monotonic clock
	// carries on, exactly like an NTP step or a manual change.
	offset.Store(int64(3 * time.Hour))

	closed, idx := h.waitClosed(10*time.Second, 0)
	if closed.Reason != ipc.ReasonClockJump {
		t.Fatalf("close reason %q, want clock_jump", closed.Reason)
	}
	second, _ := h.waitOpened(5*time.Second, idx)
	if second.Gap {
		t.Fatal("a clock change is not a gap in the recording")
	}
	p1, _ := naming.Parse(filepath.Base(first.Path))
	p2, _ := naming.Parse(filepath.Base(second.Path))
	if got := p2.HourStart.Sub(p1.HourStart); got != 3*time.Hour {
		t.Fatalf("new file is named %v after the old one, want the corrected time (3h)", got)
	}
}
