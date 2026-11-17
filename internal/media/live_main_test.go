package media_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/media/mediatest"
	"github.com/jxburros/buttercams/test/simulator"
)

const restreamToken = "restream-token"

// restreamRig uses the simulator in place of a worker's loopback restream:
// it serves /<camera-id> and asks for the restream's user and token.
func restreamRig(t *testing.T, audio string) *liveRig {
	t.Helper()
	r := newLiveRig(t, simulator.Stream{Path: "/cam1", Audio: audio, Username: ipc.RestreamUser, Password: restreamToken})
	r.hub.MainRetry = 200 * time.Millisecond
	r.assignMain(restreamToken)
	return r
}

func (r *liveRig) assignMain(token string) {
	cam := r.camera("cam1", "/not-used-by-live-view", "")
	r.hub.Assign([]config.Camera{cam}, map[string]string{"cam1": r.sim.Addr()}, token)
}

func anyState(v *mediatest.LiveViewer, want string) bool {
	for _, s := range v.States() {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// The main stream comes from the worker's restream, with audio only for the
// viewers that asked for it (spec 16.4).
func TestLiveMainStreamAndAudio(t *testing.T) {
	for _, tc := range []struct{ audio, mime string }{
		{"aac", "mp4a.40.2"}, {"pcmu", "mp4a.40.2"}, {"g726", "mp4a.40.2"}, {"opus", "opus"},
	} {
		t.Run(tc.audio, func(t *testing.T) {
			t.Parallel()
			r := restreamRig(t, tc.audio)
			silent := r.view("cam1", "main", false)
			waitFragments(t, silent, 2*simulator.FPS, 20*time.Second)
			if n := r.sim.Sessions("/cam1"); n != 1 {
				t.Fatalf("%d sessions on the restream", n)
			}

			// A listener joins the source that is already playing.
			loud := r.view("cam1", "main", true)
			if !loud.Wait(20*time.Second, func(v *mediatest.LiveViewer) bool {
				for _, i := range v.Inits() {
					if strings.Contains(i.Mime, tc.mime) {
						return true
					}
				}
				return false
			}) {
				t.Fatalf("the listener never got a header with audio: %+v", loud.Inits())
			}
			waitFragments(t, loud, loud.Fragments()+6*simulator.FPS, 30*time.Second)
			waitFragments(t, silent, 6*simulator.FPS, 30*time.Second)

			if n := r.sim.Sessions("/cam1"); n != 1 {
				t.Fatalf("%d sessions on the restream for two viewers", n)
			}
			for _, i := range silent.Inits() {
				if strings.Contains(i.Mime, "mp4a") || strings.Contains(i.Mime, "opus") {
					t.Fatalf("a viewer that did not ask for audio got %q", i.Mime)
				}
			}
			d := decode(t, silent.Bytes())
			checkContinuous(t, d, 6*simulator.FPS-4, 0.2)
			if d.audio != 0 {
				t.Fatalf("%d audio frames reached the silent viewer", d.audio)
			}
			// A listener is never started without sound and switched later:
			// a browser cannot add a track to a playing stream.
			streams := loud.Streams()
			if len(streams) != 1 {
				t.Fatalf("the listener got %d headers: %+v", len(streams), loud.Inits())
			}
			withAudio := decode(t, streams[len(streams)-1])
			checkContinuous(t, withAudio, 2*simulator.FPS, 0.2)
			if withAudio.audio < 20 {
				t.Fatalf("only %d audio frames decoded", withAudio.audio)
			}
		})
	}
}

// At the hourly rollover the hub connects to the camera's new session while
// the old one still plays and moves the viewers at a keyframe: one unbroken
// stream, no new header (spec 16.3).
func TestLiveMainSwitchesSessionsWithoutAHole(t *testing.T) {
	r := restreamRig(t, "aac")
	v := r.view("cam1", "main", true)
	waitFragments(t, v, 2*simulator.FPS, 20*time.Second)
	headers := len(v.Inits())

	r.hub.SourceChanged("cam1", 2)
	waitFor(t, 10*time.Second, "the second session", func() bool { return r.sim.Sessions("/cam1") == 2 })
	waitFor(t, 10*time.Second, "the old session to close", func() bool { return r.sim.Sessions("/cam1") == 1 })
	waitFragments(t, v, v.Fragments()+3*simulator.FPS, 20*time.Second)

	// And once more, to show the switch can repeat.
	r.hub.SourceChanged("cam1", 3)
	waitFor(t, 10*time.Second, "the third session", func() bool { return r.sim.Sessions("/cam1") == 2 })
	waitFor(t, 10*time.Second, "the second session to close", func() bool { return r.sim.Sessions("/cam1") == 1 })
	waitFragments(t, v, v.Fragments()+3*simulator.FPS, 20*time.Second)

	if len(v.Inits()) != headers || len(v.States()) > 1 {
		t.Fatalf("the switch must not show: inits %+v, states %q", v.Inits(), v.States())
	}
	streams := v.Streams()
	d := decode(t, streams[len(streams)-1])
	checkContinuous(t, d, 4*simulator.FPS, 0.5) // the fragment count includes audio fragments
	if d.audio < 50 {
		t.Fatalf("only %d audio frames across the switches", d.audio)
	}
	// Frames arrived without a pause longer than half a second.
	var last time.Time
	for _, e := range v.Events() {
		if e.Kind != "fragment" {
			continue
		}
		if !last.IsZero() && e.At.Sub(last) > 500*time.Millisecond {
			t.Fatalf("the viewer waited %s during a switch", e.At.Sub(last))
		}
		last = e.At
	}
}

// Without the notice, or when the camera reconnects after a failure, the old
// stream simply ends and the hub connects again (spec 16.3).
func TestLiveMainReconnectsWhenTheStreamEnds(t *testing.T) {
	r := restreamRig(t, "none")
	v := r.view("cam1", "main", false)
	waitFragments(t, v, simulator.FPS, 20*time.Second)

	r.sim.Kick("/cam1")
	before := v.Fragments()
	waitFragments(t, v, before+3*simulator.FPS, 20*time.Second)
	if len(v.Inits()) != 1 {
		t.Fatalf("inits after a reconnect: %+v", v.Inits())
	}
	checkContinuous(t, decode(t, v.Bytes()), 4*simulator.FPS-4, 0.2)

	// The restream goes away (live.mainStream switched off, or the worker
	// stopped): the viewer is told, and gets video back when it returns.
	r.hub.Assign([]config.Camera{r.camera("cam1", "/x", "")}, nil, "")
	if !v.Wait(5*time.Second, func(v *mediatest.LiveViewer) bool { return anyState(v, "main stream is not available") }) {
		t.Fatalf("states: %q", v.States())
	}
	waitFor(t, 5*time.Second, "the restream session to close", func() bool { return r.sim.Sessions("/cam1") == 0 })
	time.Sleep(300 * time.Millisecond)
	before = v.Fragments()
	r.assignMain(restreamToken)
	waitFragments(t, v, before+simulator.FPS, 20*time.Second)

	// An assignment that changes nothing for this source leaves it alone.
	before = len(v.States())
	r.assignMain(restreamToken)
	time.Sleep(500 * time.Millisecond)
	if len(v.States()) != before || r.sim.Sessions("/cam1") != 1 {
		t.Fatalf("an unchanged assignment disturbed the source: %q", v.States())
	}
}

func TestLiveMainWithAWrongToken(t *testing.T) {
	r := restreamRig(t, "none")
	r.assignMain("not-the-token")
	v := r.view("cam1", "main", false)
	if !v.Wait(10*time.Second, func(v *mediatest.LiveViewer) bool { return anyState(v, "refused") }) {
		t.Fatalf("states: %q", v.States())
	}
	if v.Fragments() != 0 {
		t.Fatal("video without the restream's token")
	}
	// The token arrives (the guardian re-sends the assignment): video.
	r.assignMain(restreamToken)
	waitFragments(t, v, 5, 20*time.Second)
}
