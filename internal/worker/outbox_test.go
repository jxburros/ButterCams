package worker

import (
	"io"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
)

// A guardian that stops reading must never block a camera goroutine
// (spec rule 1). Segment messages keep their order; statistics coalesce.
func TestOutboxNeverBlocksAndKeepsOrder(t *testing.T) {
	pr, pw := io.Pipe()
	o := newOutbox(ipc.NewWriter(pw), nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			o.post(&ipc.CameraStats{CameraID: "cam", State: "recording"})
			o.post(&ipc.SegmentOpened{CameraID: "cam", Path: string(rune('a' + i%26))})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("post blocked while nobody read the pipe")
	}

	r := ipc.NewReader(pr)
	opened, stats := 0, 0
	for opened < 1000 {
		m, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		switch msg := m.(type) {
		case *ipc.SegmentOpened:
			if want := string(rune('a' + opened%26)); msg.Path != want {
				t.Fatalf("message %d out of order: %q, want %q", opened, msg.Path, want)
			}
			opened++
		case *ipc.CameraStats:
			stats++
		}
	}
	if stats > 10 {
		t.Fatalf("statistics were not coalesced: %d sent", stats)
	}
	go io.Copy(io.Discard, pr)
	if !o.flush(2 * time.Second) {
		t.Fatal("flush did not finish")
	}
}
