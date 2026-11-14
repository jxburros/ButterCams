package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/test/simulator"
)

// harness runs a worker in-process, playing the guardian's side of the pipes.
type harness struct {
	t       *testing.T
	staging string
	in      *io.PipeWriter
	enc     *ipc.Writer
	done    chan error
	w       *Worker

	mu   sync.Mutex
	msgs []any
	cond *sync.Cond
}

func startWorker(t *testing.T, staging string, now func() time.Time) *harness {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, staging: staging, in: inW, enc: ipc.NewWriter(inW), done: make(chan error, 1)}
	h.cond = sync.NewCond(&h.mu)
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h.w = newWorker(1, outW, log, now)
	go func() {
		err := h.w.run(context.Background(), inR)
		outW.Close()
		h.done <- err
	}()
	go func() {
		r := ipc.NewReader(outR)
		for {
			m, err := r.Next()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.msgs = append(h.msgs, m)
			h.cond.Broadcast()
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() { h.stop() })
	return h
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	defer func() { recover() }() // the test may already have finished
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (h *harness) send(msg any) {
	h.t.Helper()
	if err := h.enc.Send(msg); err != nil {
		h.t.Fatalf("sending to worker: %v", err)
	}
}

func (h *harness) assign(cams ...config.Camera) {
	h.send(&ipc.Assign{StagingPath: h.staging, Cameras: cams})
}

// stop shuts the worker down and waits until its files are finalized.
func (h *harness) stop() {
	select {
	case <-h.done:
		h.done <- nil
		return
	default:
	}
	h.enc.Send(&ipc.Shutdown{})
	select {
	case err := <-h.done:
		h.done <- err
	case <-time.After(20 * time.Second):
		h.t.Errorf("worker did not shut down")
	}
	h.in.Close()
	time.Sleep(50 * time.Millisecond) // let the reader collect the last messages
}

// wait blocks until pred matches a message received at index >= from. It
// returns the message and the index after it.
func (h *harness) wait(timeout time.Duration, what string, from int, pred func(any) bool) (any, int) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() { h.mu.Lock(); h.cond.Broadcast(); h.mu.Unlock() })
	defer timer.Stop()
	h.mu.Lock()
	defer h.mu.Unlock()
	i := from
	for {
		for ; i < len(h.msgs); i++ {
			if pred(h.msgs[i]) {
				return h.msgs[i], i + 1
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		h.cond.Wait()
	}
}

func (h *harness) waitClosed(timeout time.Duration, from int) (*ipc.SegmentClosed, int) {
	h.t.Helper()
	m, next := h.wait(timeout, "segment_closed", from, func(m any) bool { _, ok := m.(*ipc.SegmentClosed); return ok })
	return m.(*ipc.SegmentClosed), next
}

func (h *harness) waitOpened(timeout time.Duration, from int) (*ipc.SegmentOpened, int) {
	h.t.Helper()
	m, next := h.wait(timeout, "segment_opened", from, func(m any) bool { _, ok := m.(*ipc.SegmentOpened); return ok })
	return m.(*ipc.SegmentOpened), next
}

func (h *harness) waitState(timeout time.Duration, state string) *ipc.CameraStats {
	h.t.Helper()
	m, _ := h.wait(timeout, "state "+state, 0, func(m any) bool {
		s, ok := m.(*ipc.CameraStats)
		return ok && s.State == state
	})
	return m.(*ipc.CameraStats)
}

func (h *harness) waitEvent(timeout time.Duration, code string) *ipc.Event {
	h.t.Helper()
	m, _ := h.wait(timeout, "event "+code, 0, func(m any) bool {
		e, ok := m.(*ipc.Event)
		return ok && e.Code == code
	})
	return m.(*ipc.Event)
}

func (h *harness) all() []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]any(nil), h.msgs...)
}

func startSim(t *testing.T, streams ...simulator.Stream) *simulator.Server {
	t.Helper()
	s, err := simulator.Start("127.0.0.1:0", streams)
	if err != nil {
		t.Fatalf("starting simulator: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func simCamera(t *testing.T, sim *simulator.Server, id, path string) config.Camera {
	t.Helper()
	host, portStr, err := net.SplitHostPort(sim.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return config.Camera{
		ID: id, Name: id, Enabled: true, Host: host, Port: port, Path: path,
		Transport: "tcp", RecordAudio: true, RecordingMode: "continuous",
	}
}

// probe is what ffprobe says about a recording.
type probe struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		CodecType  string `json:"codec_type"`
		SampleRate string `json:"sample_rate"`
		NbFrames   string `json:"nb_read_frames"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

func (p probe) codec(kind string) string {
	for _, s := range p.Streams {
		if s.CodecType == kind {
			return s.CodecName
		}
	}
	return ""
}

func (p probe) frames(kind string) int {
	for _, s := range p.Streams {
		if s.CodecType == kind {
			n, _ := strconv.Atoi(s.NbFrames)
			return n
		}
	}
	return 0
}

func (p probe) duration() float64 {
	d, _ := strconv.ParseFloat(p.Format.Duration, 64)
	return d
}

// ffprobe decodes the whole file with an independent tool, proving it plays
// without ButterCams software (spec rule 5). The test is skipped when ffprobe
// is not installed.
func ffprobe(t *testing.T, path string) probe {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	out, err := exec.Command(bin, "-v", "error", "-count_frames", "-show_format", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	// A full decode must not report any error.
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		msg, err := exec.Command(ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-").CombinedOutput()
		if err != nil || len(strings.TrimSpace(string(msg))) > 0 {
			t.Fatalf("ffmpeg could not decode %s cleanly: %v %s", filepath.Base(path), err, msg)
		}
	}
	return p
}

func mustExist(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
	return st.Size()
}

func partFiles(t *testing.T, staging string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(staging, "*", "*", "*.part.mp4"))
	return m
}
