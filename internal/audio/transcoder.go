package audio

import (
	"fmt"
	"math"

	aac "github.com/tphakala/go-aac"
)

// Output format of the transcoder: AAC-LC, mono, 48 kHz, 32 kbit/s.
//
// The pure-Go encoder only supports 44.1 and 48 kHz, so 8 kHz and 16 kHz
// camera audio is upsampled to 48 kHz first (see docs/DECISIONS.md).
const (
	OutRate    = 48000
	OutBitrate = 32000
	FrameSize  = aac.FrameSize // samples per AAC access unit
)

// AudioSpecificConfig is the MPEG-4 AudioSpecificConfig of the output:
// AAC-LC (object type 2), 48 kHz (index 3), mono.
var AudioSpecificConfig = []byte{0x11, 0x88}

// Frame is one encoded AAC access unit.
type Frame struct {
	PTS  int64 // presentation time of its first sample, in OutRate units
	Data []byte
}

// SupportedRate reports whether PCM at the given rate can be transcoded.
func SupportedRate(rate int) bool {
	return rate > 0 && rate <= OutRate && OutRate%rate == 0
}

// upsampler is a polyphase windowed-sinc interpolator by an integer factor.
type upsampler struct {
	l      int
	taps   int // taps per phase
	phases [][]float32
	hist   []float32 // last `taps` input samples, newest last
}

const upsamplerTaps = 16

func newUpsampler(l int) *upsampler {
	u := &upsampler{l: l, taps: upsamplerTaps}
	n := l * u.taps
	center := float64(n-1) / 2
	cutoff := 0.92 // fraction of the source Nyquist frequency
	u.phases = make([][]float32, l)
	for p := 0; p < l; p++ {
		u.phases[p] = make([]float32, u.taps)
	}
	for i := 0; i < n; i++ {
		x := (float64(i) - center) / float64(l)
		var s float64
		if x == 0 {
			s = cutoff
		} else {
			s = math.Sin(math.Pi*cutoff*x) / (math.Pi * x)
		}
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) // Hann
		// tap i belongs to phase i%l and to input delay i/l
		u.phases[i%l][i/l] = float32(s * w)
	}
	u.hist = make([]float32, u.taps)
	return u
}

// delay is the group delay of the filter in output samples.
func (u *upsampler) delay() int { return (u.l*u.taps - 1) / 2 }

func (u *upsampler) reset() { clear(u.hist) }

// process appends l output samples per input sample to dst.
func (u *upsampler) process(dst []float32, in []float32) []float32 {
	if u.l == 1 {
		return append(dst, in...)
	}
	for _, x := range in {
		copy(u.hist, u.hist[1:])
		u.hist[u.taps-1] = x
		for p := 0; p < u.l; p++ {
			var acc float32
			ph := u.phases[p]
			for k := 0; k < u.taps; k++ {
				acc += ph[k] * u.hist[u.taps-1-k]
			}
			dst = append(dst, acc)
		}
	}
	return dst
}

// Transcoder turns 16-bit mono PCM at a telephone sample rate into AAC-LC
// frames with correct presentation times.
type Transcoder struct {
	srcRate int
	up      *upsampler
	enc     *aac.Encoder

	in      []float32
	pending []float32 // 48 kHz samples waiting for a full frame
	encBuf  []byte

	started bool
	next    int64 // expected PTS of the next input sample, source-rate units
	t0      int64 // PTS of output sample 0 of this run, OutRate units
	calls   int64 // EncodeFrame calls in this run
}

// NewTranscoder returns a transcoder for mono PCM at srcRate.
func NewTranscoder(srcRate int) (*Transcoder, error) {
	if !SupportedRate(srcRate) {
		return nil, fmt.Errorf("sample rate %d Hz cannot be converted to AAC", srcRate)
	}
	t := &Transcoder{srcRate: srcRate, up: newUpsampler(OutRate / srcRate)}
	cutoff := srcRate / 2
	if cutoff > 20000 {
		cutoff = 0 // let the encoder choose
	}
	enc, err := aac.NewEncoder(aac.EncoderConfig{
		SampleRate: OutRate,
		Channels:   1,
		Bitrate:    OutBitrate,
		Cutoff:     cutoff,
	})
	if err != nil {
		return nil, err
	}
	t.enc = enc
	return t, nil
}

func (t *Transcoder) cfg() aac.EncoderConfig {
	cutoff := t.srcRate / 2
	if cutoff > 20000 {
		cutoff = 0
	}
	return aac.EncoderConfig{SampleRate: OutRate, Channels: 1, Bitrate: OutBitrate, Cutoff: cutoff}
}

func (t *Transcoder) restart() error {
	t.started = false
	t.pending = t.pending[:0]
	t.calls = 0
	t.up.reset()
	return t.enc.Reset(t.cfg())
}

// Write feeds PCM samples whose first sample has presentation time pts, in
// source-rate units, and returns any AAC frames that became complete. A small
// hole in the input is filled with silence; a large jump restarts the encoder
// so that audio stays in sync with video.
func (t *Transcoder) Write(pcm []int16, pts int64) ([]Frame, error) {
	if len(pcm) == 0 {
		return nil, nil
	}
	l := int64(t.up.l)
	if t.started {
		gap := pts - t.next
		tolerance := int64(t.srcRate / 20) // 50 ms
		switch {
		case gap > tolerance && gap <= int64(2*t.srcRate):
			t.in = t.in[:0]
			for i := int64(0); i < gap; i++ {
				t.in = append(t.in, 0)
			}
			t.pending = t.up.process(t.pending, t.in)
		case gap > tolerance || gap < -tolerance:
			if err := t.restart(); err != nil {
				return nil, err
			}
		}
	}
	if !t.started {
		t.started = true
		t.t0 = pts*l - int64(t.up.delay())
	}
	t.next = pts + int64(len(pcm))

	t.in = t.in[:0]
	for _, s := range pcm {
		t.in = append(t.in, float32(s)/32768)
	}
	t.pending = t.up.process(t.pending, t.in)

	var out []Frame
	for len(t.pending) >= FrameSize {
		buf, err := t.enc.EncodeFrame(t.encBuf[:0], [][]float32{t.pending[:FrameSize]})
		if err != nil {
			return out, err
		}
		t.encBuf = buf[:0]
		t.calls++
		n := copy(t.pending, t.pending[FrameSize:])
		t.pending = t.pending[:n]
		if len(buf) == 0 {
			continue // the encoder holds back its first frame
		}
		// The access unit returned by call n (n ≥ 2) is unit j = n-2. Unit j
		// decodes to samples [j·1024, (j+1)·1024), which carry the input
		// delayed by one frame of encoder priming.
		j := t.calls - 2
		out = append(out, Frame{
			PTS:  t.t0 + (j-1)*FrameSize,
			Data: append([]byte(nil), buf...),
		})
	}
	return out, nil
}
