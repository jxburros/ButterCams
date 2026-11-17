package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/test/simulator"
)

func probeCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func hasWarning(list []string, want string) bool {
	for _, w := range list {
		if w == want {
			return true
		}
	}
	return false
}

// "Test connection" also tests the sub stream and reports its codec and
// resolution (spec 16.8).
func TestProbeReportsSubStream(t *testing.T) {
	t.Parallel()
	sim := startSim(t,
		simulator.Stream{Path: "/cam"},
		simulator.Stream{Path: "/cam/sub", Small: true},
	)
	cam := simCamera(t, sim, "cam", "/cam")
	sub := simulator.SubPath("/cam")
	cam.SubStreamPath = &sub

	res := Probe(probeCtx(t), cam, "")
	if !res.OK || res.VideoCodec != "h264" || res.Width != 320 || res.Height != 240 {
		t.Fatalf("main stream: %+v", res)
	}
	if res.Sub == nil || !res.Sub.OK || res.Sub.VideoCodec != "h264" || res.Sub.Width != 160 || res.Sub.Height != 120 {
		t.Fatalf("sub stream: %+v", res.Sub)
	}
	if len(res.Sub.Warnings) != 0 || hasWarning(res.Warnings, NoSubWarning) {
		t.Fatalf("unexpected warnings: %v / %v", res.Warnings, res.Sub.Warnings)
	}
}

func TestProbeWarnsAboutH265AndMissingSubStream(t *testing.T) {
	t.Parallel()
	sim := startSim(t,
		simulator.Stream{Path: "/cam"},
		simulator.Stream{Path: "/cam/sub", Video: "h265"},
		simulator.Stream{Path: "/other/sub", Refuse: true},
	)
	cam := simCamera(t, sim, "cam", "/cam")

	// No sub stream: the form recommends setting one.
	res := Probe(probeCtx(t), cam, "")
	if !res.OK || res.Sub != nil || !hasWarning(res.Warnings, NoSubWarning) {
		t.Fatalf("without a sub stream: %+v", res)
	}

	// An H.265 sub stream works but is warned about.
	sub := "/cam/sub"
	cam.SubStreamPath = &sub
	res = Probe(probeCtx(t), cam, "")
	if res.Sub == nil || !res.Sub.OK || res.Sub.VideoCodec != "h265" || !hasWarning(res.Sub.Warnings, SubH265Warning) {
		t.Fatalf("H.265 sub stream: %+v", res.Sub)
	}

	// A sub stream that refuses: the main result stands, the sub one fails.
	refused := "/other/sub"
	cam.SubStreamPath = &refused
	res = Probe(probeCtx(t), cam, "")
	if !res.OK || res.Sub == nil || res.Sub.OK || res.Sub.Error == "" {
		t.Fatalf("refused sub stream: %+v / %+v", res, res.Sub)
	}
	if strings.Contains(res.Sub.Error, "rtsp://") {
		t.Fatalf("the error leaks an address: %q", res.Sub.Error)
	}
}

// Changing the sub stream path never restarts a recording (V2 plan M0).
func TestSubStreamPathIsNotARecorderChange(t *testing.T) {
	a := config.Camera{ID: "cam", Name: "cam", Enabled: true, Host: "10.0.0.2", Port: 554, Path: "/main", Transport: "tcp"}
	b := a
	sub := "/sub"
	b.SubStreamPath = &sub
	if recorderChanged(a, b) {
		t.Fatal("a sub stream change must not reconnect the recorder")
	}
	b = a
	b.Path = "/other"
	if !recorderChanged(a, b) {
		t.Fatal("a main path change must reconnect the recorder")
	}
}
