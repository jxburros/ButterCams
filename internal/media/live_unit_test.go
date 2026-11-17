package media

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/stream"
)

func TestCodecStrings(t *testing.T) {
	// Baseline 3.0, High 3.1.
	if got := videoCodecString(stream.VideoParams{Codec: "h264", SPS: []byte{0x67, 0x42, 0xC0, 0x1E, 0xAA}}); got != "avc1.42C01E" {
		t.Fatalf("H.264: %s", got)
	}
	if got := videoCodecString(stream.VideoParams{Codec: "h264", SPS: []byte{0x67, 0x64, 0x00, 0x1F}}); got != "avc1.64001F" {
		t.Fatalf("H.264: %s", got)
	}
	// Main profile, main tier, level 3.1, progressive + frame-only +
	// non-packed (0xB0). The zero bytes of the constraint flags are followed
	// by an emulation prevention byte in a real SPS.
	sps := []byte{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0xB0, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x5D, 0xA0}
	if got := hevcCodecString(sps); got != "hvc1.1.6.L93.B0" {
		t.Fatalf("H.265 main: %s", got)
	}
	// Main 10, high tier, level 5.1, no constraint flags.
	sps = []byte{0x42, 0x01, 0x01, 0x22, 0x20, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x00, 0x99, 0xA0}
	if got := hevcCodecString(sps); got != "hvc1.2.4.H153" {
		t.Fatalf("H.265 main 10: %s", got)
	}
	if got := hevcCodecString([]byte{0x42, 0x01}); !strings.HasPrefix(got, "hvc1.") {
		t.Fatalf("a short SPS must still give a codec string: %s", got)
	}
}

// tfdt returns the base decode time of a fragment built by the muxer.
func tfdt(t *testing.T, f []byte) uint64 {
	t.Helper()
	i := bytes.Index(f, []byte("tfdt"))
	if i < 0 {
		t.Fatal("fragment without tfdt")
	}
	return binary.BigEndian.Uint64(f[i+8:])
}

// The live timeline never has a hole and never goes back: across a stall, a
// timestamp jump and the switch to another session it advances by one frame.
func TestMuxerKeepsOneTimeline(t *testing.T) {
	var m muxer
	au := [][]byte{{0x65, 1, 2, 3}}
	frame := func(dts int64, sess uint64, key bool) uint64 {
		f := m.video(stream.Sample{Video: true, PTS: dts, DTS: dts, Key: key, NALUs: au}, sess)
		if !bytes.Contains(f, []byte("moof")) || !bytes.HasSuffix(f, []byte{0, 0, 0, 4, 0x65, 1, 2, 3}) {
			t.Fatalf("fragment layout: %x", f)
		}
		return tfdt(t, f)
	}
	want := uint64(0)
	check := func(got uint64, step uint64, what string) {
		t.Helper()
		want += step
		if got != want {
			t.Fatalf("%s: time %d, want %d", what, got, want)
		}
	}
	check(frame(1_000_000, 1, true), 0, "first frame")
	check(frame(1_006_000, 1, false), 6000, "second frame")
	check(frame(1_012_000, 1, false), 6000, "third frame")
	check(frame(1_012_000+20*90000, 1, true), 6000, "after a stall")
	check(frame(1_012_000+20*90000+3000, 1, false), 3000, "a shorter frame interval")
	check(frame(500, 1, false), 3000, "timestamps went back")
	check(frame(77_000_000, 2, true), 3000, "another session")
	check(frame(77_003_000, 2, false), 3000, "the new session continues")
}

func TestMuxerAudioFollowsTheVideoTimeline(t *testing.T) {
	var m muxer
	if m.audio(stream.Sample{PTS: 1, Dur: 1024, Data: []byte{1}}, 48000) || m.flushAudio() != nil {
		t.Fatal("audio before the first picture must be dropped")
	}
	// Video at camera time 9 s; the audio clock shares the origin.
	m.video(stream.Sample{Video: true, PTS: 9 * 90000, DTS: 9 * 90000, Key: true, NALUs: [][]byte{{1}}}, 1)
	m.audio(stream.Sample{PTS: 9*48000 + 480, Dur: 1024, Data: []byte{1, 2}}, 48000)
	m.audio(stream.Sample{PTS: 9*48000 + 480 + 1024, Dur: 1024, Data: []byte{3}}, 48000)
	f := m.flushAudio()
	if f == nil || tfdt(t, f) != 480 || !bytes.HasSuffix(f, []byte{1, 2, 3}) {
		t.Fatalf("audio fragment: base %d", tfdt(t, f))
	}
	// A sample that would overlap what was sent is moved behind it.
	m.audio(stream.Sample{PTS: 9 * 48000, Dur: 1024, Data: []byte{4}}, 48000)
	if got := tfdt(t, m.flushAudio()); got != 480+2048 {
		t.Fatalf("overlapping audio placed at %d", got)
	}
	if m.flushAudio() != nil {
		t.Fatal("nothing pending")
	}
}

func TestViewerQueueOverflows(t *testing.T) {
	now := time.Now()
	v := newViewer(false)
	big := make([]byte, viewerQueueBytes/2)
	if !v.push(false, big, now) || !v.push(false, big, now) {
		t.Fatal("the queue must take 4 MB")
	}
	if v.push(false, []byte{1}, now) {
		t.Fatal("the queue took more than its limit")
	}
	if len(v.take()) != 0 {
		t.Fatal("an overflowing queue must be emptied")
	}
	// Age: something waiting for more than two seconds.
	if !v.push(false, []byte{1}, now) || !v.push(false, []byte{2}, now.Add(time.Second)) {
		t.Fatal("small messages refused")
	}
	if v.push(false, []byte{3}, now.Add(viewerQueueAge+time.Millisecond)) {
		t.Fatal("a queue older than two seconds must overflow")
	}
	v.close()
	if !v.push(false, []byte{1}, now) || len(v.take()) != 0 {
		t.Fatal("a closed viewer takes nothing and reports no overflow")
	}
}
