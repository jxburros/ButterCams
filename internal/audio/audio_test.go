package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/g711"
	aac "github.com/tphakala/go-aac"
)

func readPCM(t *testing.T, name string) []int16 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// The tables must agree with an independent G.711 implementation.
func TestG711MatchesReference(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	check := func(name string, got []int16, ref []byte) {
		for i := range got {
			want := int16(binary.BigEndian.Uint16(ref[2*i:]))
			if got[i] != want {
				t.Fatalf("%s: code %#02x decodes to %d, reference says %d", name, i, got[i], want)
			}
		}
	}
	check("mulaw", DecodeMulaw(nil, all), g711.DecodeMulaw(all))
	check("alaw", DecodeAlaw(nil, all), g711.DecodeAlaw(all))
}

// Decoded G.726 must match FFmpeg's decoder for every rate and both packings.
// The fixtures were produced with ffmpeg (see testdata/README.md).
func TestG726MatchesFFmpeg(t *testing.T) {
	for _, rate := range []int{16000, 24000, 32000, 40000} {
		for _, be := range []bool{true, false} {
			suffix := "le"
			if be {
				suffix = "be"
			}
			name := fmt.Sprintf("g726_%d_%s", rate/1000, suffix)
			t.Run(name, func(t *testing.T) {
				enc, err := os.ReadFile(filepath.Join("testdata", name+".bin"))
				if err != nil {
					t.Fatal(err)
				}
				want := readPCM(t, name+".pcm")
				dec, err := NewG726Decoder(rate, be)
				if err != nil {
					t.Fatal(err)
				}
				// FFmpeg leaks the zero predictor by 2^-8 at every rate; the
				// standard says 2^-9 at 40 kbit/s. Match FFmpeg here so the
				// rest of the 40 kbit/s path is checked against its output.
				dec.leak40 = 8
				// Feed in uneven chunks to exercise the bit accumulator.
				var got []int16
				for off := 0; off < len(enc); {
					n := 1 + off%7
					if off+n > len(enc) {
						n = len(enc) - off
					}
					got = dec.Decode(got, enc[off:off+n])
					off += n
				}
				if len(got) < len(want) {
					t.Fatalf("decoded %d samples, reference has %d", len(got), len(want))
				}
				var sig, noise float64
				mismatches := 0
				for i := range want {
					d := float64(got[i]) - float64(want[i])
					sig += float64(want[i]) * float64(want[i])
					noise += d * d
					if got[i] != want[i] {
						mismatches++
					}
				}
				snr := math.Inf(1)
				if noise > 0 {
					snr = 10 * math.Log10(sig/noise)
				}
				t.Logf("%s: %d samples, %d differ from ffmpeg, SNR %.1f dB", name, len(want), mismatches, snr)
				if snr < 25 {
					t.Fatalf("decoder output does not match the reference (SNR %.1f dB)", snr)
				}
			})
		}
	}
}

func TestG726RejectsUnknownRate(t *testing.T) {
	if _, err := NewG726Decoder(12345, false); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSupportedRate(t *testing.T) {
	for rate, want := range map[int]bool{8000: true, 16000: true, 24000: true, 48000: true, 44100: false, 11025: false, 0: false, 96000: false} {
		if SupportedRate(rate) != want {
			t.Errorf("SupportedRate(%d) != %v", rate, want)
		}
	}
}

func tone(n int, rate int, freq float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func TestTranscoderTimestampsAndBitrate(t *testing.T) {
	tr, err := NewTranscoder(8000)
	if err != nil {
		t.Fatal(err)
	}
	const startPTS = 80000 // 10 s into the stream, 8 kHz units
	pcm := tone(8000*4, 8000, 440)
	var frames []Frame
	for off := 0; off < len(pcm); off += 160 { // 20 ms packets
		f, err := tr.Write(pcm[off:off+160], startPTS+int64(off))
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f...)
	}
	if len(frames) < 180 {
		t.Fatalf("only %d frames for 4 s of audio", len(frames))
	}
	total := 0
	for i, f := range frames {
		if i > 0 && f.PTS-frames[i-1].PTS != FrameSize {
			t.Fatalf("frame %d: PTS step %d, want %d", i, f.PTS-frames[i-1].PTS, FrameSize)
		}
		total += len(f.Data)
	}
	// The first unit is encoder priming and starts one frame (plus the
	// resampler delay) before the first input sample.
	wantFirst := int64(startPTS*6) - int64(tr.up.delay()) - FrameSize
	if frames[0].PTS != wantFirst {
		t.Fatalf("first PTS %d, want %d", frames[0].PTS, wantFirst)
	}
	kbps := float64(total) * 8 / (float64(len(frames)) * FrameSize / OutRate) / 1000
	if kbps < 16 || kbps > 48 {
		t.Fatalf("bitrate %.1f kbit/s is far from the 32 kbit/s target", kbps)
	}
}

func TestTranscoderFillsSmallGapsAndRestartsOnJumps(t *testing.T) {
	tr, _ := NewTranscoder(8000)
	pcm := tone(1600, 8000, 300)
	collect := func(pts int64) []Frame {
		f, err := tr.Write(pcm, pts)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	var frames []Frame
	pts := int64(0)
	for i := 0; i < 10; i++ {
		frames = append(frames, collect(pts)...)
		pts += 1600
	}
	// 0.5 s hole: filled with silence, timeline stays continuous.
	pts += 4000
	for i := 0; i < 10; i++ {
		frames = append(frames, collect(pts)...)
		pts += 1600
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].PTS-frames[i-1].PTS != FrameSize {
			t.Fatalf("timeline broken at frame %d after a small gap", i)
		}
	}
	// 60 s jump: the encoder restarts and timestamps follow the input.
	pts += 60 * 8000
	before := frames[len(frames)-1].PTS
	var after []Frame
	for i := 0; i < 10; i++ {
		after = append(after, collect(pts)...)
		pts += 1600
	}
	if len(after) == 0 {
		t.Fatal("no frames after the jump")
	}
	if jump := after[0].PTS - before; jump < 59*OutRate {
		t.Fatalf("timestamps did not follow a 60 s jump (advanced %d)", jump)
	}
}

// The AAC we produce must be decodable by an independent decoder and must
// line up in time with what went in.
func TestTranscoderOutputDecodesWithFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	tr, _ := NewTranscoder(8000)
	pcm := tone(8000*3, 8000, 500)
	var adts bytes.Buffer
	n := 0
	for off := 0; off < len(pcm); off += 160 {
		frames, err := tr.Write(pcm[off:off+160], int64(off))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			h, err := aac.AppendADTSHeader(nil, OutRate, 1, len(f.Data))
			if err != nil {
				t.Fatal(err)
			}
			adts.Write(h)
			adts.Write(f.Data)
			n++
		}
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "a.aac")
	out := filepath.Join(dir, "a.pcm")
	os.WriteFile(in, adts.Bytes(), 0o644)
	if msg, err := exec.Command(ffmpeg, "-v", "error", "-y", "-i", in, "-f", "s16le", "-ar", "48000", "-ac", "1", out).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg could not decode the AAC: %v %s", err, msg)
	}
	raw, _ := os.ReadFile(out)
	dec := make([]float64, len(raw)/2)
	for i := range dec {
		dec[i] = float64(int16(binary.LittleEndian.Uint16(raw[2*i:])))
	}
	if len(dec) < n*FrameSize*9/10 {
		t.Fatalf("decoded %d samples from %d frames", len(dec), n)
	}
	// Energy must be concentrated at 500 Hz.
	var re, im, energy float64
	for i := 4800; i < len(dec)-4800; i++ {
		ph := 2 * math.Pi * 500 * float64(i) / OutRate
		re += dec[i] * math.Cos(ph)
		im += dec[i] * math.Sin(ph)
		energy += dec[i] * dec[i]
	}
	cnt := float64(len(dec) - 9600)
	toneEnergy := 2 * (re*re + im*im) / cnt
	if ratio := toneEnergy / energy; ratio < 0.9 {
		t.Fatalf("only %.0f%% of the decoded energy is at the input frequency", ratio*100)
	}
}
