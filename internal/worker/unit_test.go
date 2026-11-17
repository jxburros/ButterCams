package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mp4meta"
)

func TestBackoffSequence(t *testing.T) {
	b := &Backoff{jitter: func() float64 { return 0.5 }} // no jitter
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("delay %d = %v, want %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != 2*time.Second {
		t.Fatalf("after reset %v, want 2s", got)
	}
}

func TestBackoffJitterWithinTwentyPercent(t *testing.T) {
	lo := &Backoff{jitter: func() float64 { return 0 }}
	hi := &Backoff{jitter: func() float64 { return 0.999999 }}
	for i, base := range []time.Duration{2, 4, 8, 16, 32, 60, 60} {
		base *= time.Second
		l, h := lo.Next(), hi.Next()
		if l != time.Duration(float64(base)*0.8) {
			t.Fatalf("step %d: low %v, want %v", i, l, time.Duration(float64(base)*0.8))
		}
		if h <= base || h > time.Duration(float64(base)*1.2) {
			t.Fatalf("step %d: high %v outside (%v, %v]", i, h, base, time.Duration(float64(base)*1.2))
		}
	}
	r := NewBackoff()
	for i := 0; i < 200; i++ {
		d := r.Next()
		if d < 1600*time.Millisecond || d > 72*time.Second {
			t.Fatalf("random delay %v out of range", d)
		}
	}
}

// The recorder must never block on a reader (spec 15).
func TestFanoutDropsWhenReaderIsFull(t *testing.T) {
	f := &Fanout{}
	slow := f.Register("slow", 2)
	fast := f.Register("fast", 100)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			f.Publish(&Packet{RTP: &rtp.Packet{}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full reader")
	}
	if len(slow.C) != 2 || slow.Dropped() != 48 {
		t.Fatalf("slow reader: %d queued, %d dropped", len(slow.C), slow.Dropped())
	}
	if len(fast.C) != 50 || fast.Dropped() != 0 {
		t.Fatalf("fast reader: %d queued, %d dropped", len(fast.C), fast.Dropped())
	}
	f.Unregister(slow)
	f.Publish(&Packet{RTP: &rtp.Packet{}})
	if len(slow.C) != 2 || len(fast.C) != 51 {
		t.Fatal("unregistered reader still receives packets")
	}
}

func TestRecorderChangedIgnoresCosmeticEdits(t *testing.T) {
	a := config.Camera{ID: "c", Name: "A", Enabled: true, Host: "h", Port: 554, Path: "/", Transport: "tcp", RecordAudio: true}
	b := a
	b.Name = "B"
	b.DestinationIDs = []string{"nas"}
	if recorderChanged(a, b) {
		t.Fatal("renaming a camera or changing its destinations must not restart the recording")
	}
	for _, mut := range []func(*config.Camera){
		func(c *config.Camera) { c.Host = "other" },
		func(c *config.Camera) { c.Port = 8554 },
		func(c *config.Camera) { c.Path = "/x" },
		func(c *config.Camera) { c.PasswordEnc = "p" },
		func(c *config.Camera) { c.Transport = "udp" },
		func(c *config.Camera) { c.RecordAudio = false },
		func(c *config.Camera) { c.Enabled = false },
	} {
		c := a
		mut(&c)
		if !recorderChanged(a, c) {
			t.Fatalf("change not detected: %+v", c)
		}
	}
}

func TestClassifyDialError(t *testing.T) {
	if k := classifyDialError(&dialError{dialUnsupported, "x"}).kind; k != dialUnsupported {
		t.Fatalf("kind %v", k)
	}
	if k := classifyDialError(errString("dial tcp 10.0.0.1:554: connect: connection refused")).kind; k != dialRefused {
		t.Fatalf("refused not recognised: %v", k)
	}
	if k := classifyDialError(errString("No connection could be made because the target machine actively refused it.")).kind; k != dialRefused {
		t.Fatalf("Windows refusal not recognised: %v", k)
	}
	got := classifyDialError(errString("cannot reach rtsp://admin:hunter2@10.0.0.1/stream"))
	if got.kind != dialOther || got.msg != "cannot reach rtsp://admin:***@10.0.0.1/stream" {
		t.Fatalf("got %+v", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// The segment writer on its own: fragments at keyframes, a forced fragment
// after four seconds without one, exact duration bookkeeping, name suffixes.
func TestSegmentWriterFragmentsAndNames(t *testing.T) {
	dir := t.TempDir()
	sps := []byte{0x67, 0x42, 0xc0, 0x0d, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0xc0, 0xf1, 0x42, 0x99, 0x60}
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	vp := videoParams{codec: "h264", sps: sps, pps: pps}
	start := time.Date(2026, 11, 14, 9, 30, 0, 123_000_000, time.Local)

	open := func(taken func(string) bool) *segment {
		seg, err := openSegment(dir, "cam", start, vp, nil, false, taken)
		if err != nil {
			t.Fatal(err)
		}
		return seg
	}
	seg := open(nil)
	if filepath.Base(seg.partPath) != "cam_"+start.Format("2006-01-02")+"_0900_"+start.Format("-0700")+".part.mp4" {
		t.Fatalf("unexpected name %s", seg.partPath)
	}

	// 15 fps for 10 s with keyframes at 0 s and 1 s only: the stretch from
	// 1 s to 10 s must be cut into fragments of at most 4 s.
	const frame = 6000
	n := 0
	for i := 0; i < 150; i++ {
		key := i == 0 || i == 15
		u := sample{video: true, pts: 1000 + int64(i)*frame, dts: 1000 + int64(i)*frame, key: key, nalus: [][]byte{{0x65}}}
		if err := seg.writeVideo(u); err != nil {
			t.Fatal(err)
		}
		n++
	}
	cs, err := seg.close(ipc.ReasonShutdown)
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.end.Sub(cs.start); got != 10*time.Second {
		t.Fatalf("duration %v, want 10s", got)
	}
	if !cs.start.Equal(start) {
		t.Fatalf("start %v, want %v", cs.start, start)
	}
	if seg.seq != 4 { // [0,1) [1,5) [5,9) [9,10)
		t.Fatalf("%d fragments, want 4", seg.seq)
	}
	if len(seg.frags) != 2 {
		t.Fatalf("%d fragments start with a keyframe, want 2", len(seg.frags))
	}
	info, err := mp4meta.Read(cs.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Dur90k != 150*frame || info.CloseReason != ipc.ReasonShutdown || info.Gap || info.CameraID != "cam" ||
		!info.Start.Equal(start) || info.VideoCodec != "h264" || info.AudioCodec != "" || info.Size != cs.bytes {
		t.Fatalf("metadata %+v", info)
	}
	if sum, _, _ := mp4meta.HashFile(cs.path); sum != cs.sha256 {
		t.Fatal("streaming hash differs from the file")
	}

	// The next file of the same hour gets _b, then _c; a name that is no
	// longer on disk but was used is skipped too.
	b := open(nil)
	if got := filepath.Base(b.finalPath); got != filepath.Base(cs.path[:len(cs.path)-4])+"_b.mp4" {
		t.Fatalf("second file %s", got)
	}
	c := open(nil) // _b exists as .part.mp4
	if p := filepath.Base(c.finalPath); p[len(p)-6:] != "_c.mp4" {
		t.Fatalf("third file %s", p)
	}
	b.abort()
	c.abort()
	os.Remove(b.partPath)
	os.Remove(c.partPath)
	d := open(func(final string) bool { return final == filepath.Base(b.finalPath) })
	if p := filepath.Base(d.finalPath); p[len(p)-6:] != "_c.mp4" {
		t.Fatalf("a name marked as used was handed out again: %s", p)
	}
	d.abort()
}

// Timestamps that jump or go backwards must not corrupt the file's timeline.
func TestSegmentWriterSurvivesTimestampJumps(t *testing.T) {
	dir := t.TempDir()
	sps := []byte{0x67, 0x42, 0xc0, 0x0d, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0xc0, 0xf1, 0x42, 0x99, 0x60}
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	seg, err := openSegment(dir, "cam", time.Now(), videoParams{codec: "h264", sps: sps, pps: pps}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	dts := []int64{0, 6000, 12000, 5_000_000_000, 5_000_006_000, 100, 6100}
	for i, d := range dts {
		if err := seg.writeVideo(sample{video: true, pts: d, dts: d, key: i == 0, nalus: [][]byte{{0x65}}}); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := seg.close(ipc.ReasonStall)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cs.end.Sub(cs.start), (time.Duration(len(dts)) * time.Second / 15).Truncate(time.Millisecond); got != want {
		t.Fatalf("duration %v, want %d frames at 15 fps (%v)", got, len(dts), want)
	}
	if info, _ := mp4meta.Read(cs.path); !info.Gap || info.CloseReason != ipc.ReasonStall {
		t.Fatalf("metadata %+v", info)
	}
}
