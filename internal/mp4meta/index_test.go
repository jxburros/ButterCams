package mp4meta_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/stream"
)

// testFile is a recording built the way the recorder builds one: header,
// fragments, close marker and mfra.
type testFile struct {
	data     []byte
	initSize int64
	marker   int64 // offset of the close marker
	// fragment boundaries and which of them start with a keyframe
	fragOff []int64
	fragKey []bool
	keyData [][]byte // the first sample of every keyframe fragment
	dur     int64
	start   time.Time
}

const frameTicks = 6000 // 15 fps

// buildFile writes fragments of framesPer frames each. keys says which
// fragments start with a keyframe.
func buildFile(t *testing.T, audio bool, keys []bool, framesPer int, closed bool) testFile {
	t.Helper()
	sps := []byte{0x67, 0x42, 0xc0, 0x0d, 0xd9, 0x01, 0x41, 0xfb, 0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0xc0, 0xf1, 0x42, 0x99, 0x60}
	pps := []byte{0x68, 0xcb, 0x83, 0xcb, 0x20}
	var ap *stream.AudioParams
	if audio {
		ap = &stream.AudioParams{Name: "aac", Timescale: 16000, Codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
			Type: mpeg4audio.ObjectTypeAACLC, SampleRate: 16000, ChannelConfig: 1, ChannelCount: 1}}}
	}
	header, err := stream.InitSegment(stream.VideoParams{Codec: "h264", SPS: sps, PPS: pps}, ap)
	if err != nil {
		t.Fatal(err)
	}
	f := testFile{start: time.Date(2026, 11, 14, 9, 0, 0, 212_000_000, time.FixedZone("", -6*3600))}
	f.data, err = mp4meta.AppendToMoov(header, mp4meta.Udta("cam", f.start, false))
	if err != nil {
		t.Fatal(err)
	}
	f.initSize = int64(len(f.data))

	var frags []mp4meta.Fragment
	for i, key := range keys {
		base := uint64(i * framesPer * frameTicks)
		var samples []mp4meta.FragSample
		var vdata []byte
		for j := 0; j < framesPer; j++ {
			size := 40 + i*7 + j
			frame := bytes.Repeat([]byte{byte(i*16 + j)}, size)
			samples = append(samples, mp4meta.FragSample{Duration: frameTicks, Size: uint32(size), NonSync: !(key && j == 0)})
			vdata = append(vdata, frame...)
			if key && j == 0 {
				f.keyData = append(f.keyData, frame)
			}
		}
		tracks := []mp4meta.FragTrack{{ID: mp4meta.VideoTrackID, BaseTime: base, Samples: samples}}
		var adata []byte
		if audio {
			adata = bytes.Repeat([]byte{0xAA}, 90)
			tracks = append(tracks, mp4meta.FragTrack{ID: mp4meta.AudioTrackID, BaseTime: base * 16000 / 90000,
				Samples: []mp4meta.FragSample{{Duration: 1024, Size: 90}}})
		}
		off := int64(len(f.data))
		f.fragOff = append(f.fragOff, off)
		f.fragKey = append(f.fragKey, key)
		f.data = mp4meta.AppendFragmentHeader(f.data, uint32(i+1), tracks)
		f.data = append(append(f.data, vdata...), adata...)
		if key {
			frags = append(frags, mp4meta.Fragment{Time: base, MoofOffset: uint64(off)})
		}
	}
	f.dur = int64(len(keys) * framesPer * frameTicks)
	f.marker = int64(len(f.data))
	if closed {
		f.data = append(f.data, mp4meta.Trailer("hour_rollover", f.dur, frags)...)
	}
	return f
}

// checkCover verifies that the chunks cover the bytes from the end of the
// header to the end of the fragments exactly once, each starting at a
// keyframe fragment.
func checkCover(t *testing.T, f testFile, ix *mp4meta.Index, end int64) {
	t.Helper()
	pos := f.initSize
	var keyOffs []int64
	for i, off := range f.fragOff {
		if f.fragKey[i] && off < end {
			keyOffs = append(keyOffs, off)
		}
	}
	if len(ix.Chunks) != len(keyOffs) {
		t.Fatalf("%d chunks, want %d", len(ix.Chunks), len(keyOffs))
	}
	var total uint64
	for i, c := range ix.Chunks {
		if c.Offset != pos || c.Offset != keyOffs[i] || c.Size <= 0 {
			t.Fatalf("chunk %d at %d (+%d); expected it at %d", i, c.Offset, c.Size, pos)
		}
		if c.Time != total {
			t.Fatalf("chunk %d starts at %d ticks, want %d", i, c.Time, total)
		}
		pos += c.Size
		total += c.Dur
	}
	if pos != end || ix.End != end {
		t.Fatalf("chunks end at %d and the index at %d; the fragments end at %d", pos, ix.End, end)
	}
	if int64(total) != ix.Dur90k {
		t.Fatalf("chunk durations add up to %d, the index says %d", total, ix.Dur90k)
	}
}

func TestReadIndexOfClosedFile(t *testing.T) {
	// The third and sixth fragments do not start with a keyframe (a camera
	// with a long keyframe interval): they belong to the chunk before them.
	keys := []bool{true, true, false, true, true, false}
	f := buildFile(t, true, keys, 15, true)
	ix, err := mp4meta.ReadIndex(bytes.NewReader(f.data), int64(len(f.data)))
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Closed || ix.InitSize != f.initSize || ix.Dur90k != f.dur || !ix.Start.Equal(f.start) {
		t.Fatalf("index %+v", ix)
	}
	checkCover(t, f, ix, f.marker)
	if ix.Chunks[1].Dur != 2*15*frameTicks || ix.Chunks[3].Dur != 2*15*frameTicks {
		t.Fatalf("a chunk of two fragments must last as long as both: %+v", ix.Chunks)
	}
	if ix.MIME() != `video/mp4; codecs="avc1.42c00d, mp4a.40.2"` || ix.VideoMIME() != `video/mp4; codecs="avc1.42c00d"` {
		t.Fatalf("MIME %s / %s", ix.MIME(), ix.VideoMIME())
	}
	if ix.Width <= 0 || ix.Height <= 0 || ix.VideoCodec != "h264" || ix.AudioCodec != "aac" || !ix.HasAudio() {
		t.Fatalf("tracks: %+v", ix)
	}
	// The video-only header declares no audio track and is still a header.
	if bytes.Contains(ix.VideoInit, []byte("mp4a")) || bytes.Contains(ix.VideoInit, []byte("soun")) ||
		!bytes.Contains(ix.VideoInit, []byte("avc1")) || !bytes.Contains(ix.VideoInit, []byte("mvex")) {
		t.Fatal("the video-only header is wrong")
	}
	vix, err := mp4meta.ReadHeader(bytes.NewReader(ix.VideoInit), int64(len(ix.VideoInit)))
	if err != nil || vix.HasAudio() || vix.VideoCodecString != ix.VideoCodecString || !vix.Start.Equal(f.start) {
		t.Fatalf("reading the video-only header back: %+v, %v", vix, err)
	}

	// Without audio.
	f = buildFile(t, false, []bool{true, true}, 5, true)
	ix, err = mp4meta.ReadIndex(bytes.NewReader(f.data), int64(len(f.data)))
	if err != nil || ix.HasAudio() || strings.Contains(ix.MIME(), "mp4a") {
		t.Fatalf("%+v %v", ix, err)
	}
	checkCover(t, f, ix, f.marker)
}

// A file without a usable mfra (cut off, or still being written) is scanned
// and gives the same chunks.
func TestReadIndexFallsBackToScanning(t *testing.T) {
	keys := []bool{true, false, true, true}
	closed := buildFile(t, true, keys, 15, true)
	want, err := mp4meta.ReadIndex(bytes.NewReader(closed.data), int64(len(closed.data)))
	if err != nil {
		t.Fatal(err)
	}
	open := buildFile(t, true, keys, 15, false)
	got, err := mp4meta.ReadIndex(bytes.NewReader(open.data), int64(len(open.data)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Closed || len(got.Chunks) != len(want.Chunks) || got.Dur90k != want.Dur90k || got.End != want.End {
		t.Fatalf("scanned index differs:\n got  %+v\n want %+v", got, want)
	}
	for i := range got.Chunks {
		if got.Chunks[i] != want.Chunks[i] {
			t.Fatalf("chunk %d: got %+v, want %+v", i, got.Chunks[i], want.Chunks[i])
		}
	}
	// A damaged mfra is not trusted.
	bad := append([]byte(nil), closed.data...)
	binary.BigEndian.PutUint64(bad[len(bad)-16-19+8:], 3) // last tfra entry: moof offset inside the header
	ix, err := mp4meta.ReadIndex(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	checkCover(t, closed, ix, closed.marker)
}

// The index of a file being written never lists a fragment the writer has
// not finished, whatever the length of the file at that moment, and it ends
// up equal to the index of the closed file.
func TestIndexOfGrowingFile(t *testing.T) {
	keys := []bool{true, true, false, true, false, true}
	f := buildFile(t, true, keys, 6, true)
	final, err := mp4meta.ReadIndex(bytes.NewReader(f.data), int64(len(f.data)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mp4meta.ReadHeader(bytes.NewReader(f.data[:f.initSize-1]), f.initSize-1); err != mp4meta.ErrNotReady {
		t.Fatalf("a header that is not written completely: %v", err)
	}
	var ix *mp4meta.Index
	for size := f.initSize; size <= int64(len(f.data)); size++ {
		r := bytes.NewReader(f.data[:size])
		if ix == nil {
			if ix, err = mp4meta.ReadHeader(r, size); err != nil {
				t.Fatal(err)
			}
		}
		before := len(ix.Chunks)
		mp4meta.ExtendIndex(r, size, ix)
		if len(ix.Chunks) < before || ix.End > size {
			t.Fatalf("at %d bytes the index went back or past the end: %+v", size, ix)
		}
		// Everything listed must be complete at this length and identical
		// to what the closed file has there.
		var complete int64 = f.initSize
		for i, off := range f.fragOff {
			end := f.marker
			if i+1 < len(f.fragOff) {
				end = f.fragOff[i+1]
			}
			if end <= size {
				complete = end
			}
			_ = off
		}
		if ix.End != complete {
			t.Fatalf("at %d bytes the index ends at %d; the last complete fragment ends at %d", size, ix.End, complete)
		}
		for i, c := range ix.Chunks {
			if c.Offset+c.Size > size {
				t.Fatalf("at %d bytes chunk %d reaches to %d", size, i, c.Offset+c.Size)
			}
			if c.Offset != final.Chunks[i].Offset || c.Time != final.Chunks[i].Time {
				t.Fatalf("at %d bytes chunk %d is %+v; the closed file has %+v", size, i, c, final.Chunks[i])
			}
		}
		if ix.Closed != (size >= f.marker+mp4meta.TrailerSize) {
			t.Fatalf("at %d bytes closed=%v (marker at %d)", size, ix.Closed, f.marker)
		}
	}
	if len(ix.Chunks) != len(final.Chunks) || ix.Dur90k != final.Dur90k || ix.End != final.End {
		t.Fatalf("grown index %+v differs from the closed one %+v", ix, final)
	}
	for i := range ix.Chunks {
		if ix.Chunks[i] != final.Chunks[i] {
			t.Fatalf("chunk %d: grown %+v, closed %+v", i, ix.Chunks[i], final.Chunks[i])
		}
	}
}

func TestKeyframeFragment(t *testing.T) {
	f := buildFile(t, true, []bool{true, false, true}, 15, true)
	r := bytes.NewReader(f.data)
	ix, err := mp4meta.ReadIndex(r, int64(len(f.data)))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range ix.Chunks {
		frag, err := mp4meta.KeyframeFragment(r, c, uint32(i+1))
		if err != nil {
			t.Fatal(err)
		}
		// One moof + mdat that holds exactly the chunk's first frame.
		moofSize := int(binary.BigEndian.Uint32(frag))
		if string(frag[4:8]) != "moof" || string(frag[moofSize+4:moofSize+8]) != "mdat" {
			t.Fatalf("chunk %d: not a fragment", i)
		}
		if !bytes.Equal(frag[moofSize+8:], f.keyData[i]) {
			t.Fatalf("chunk %d: the fragment does not hold the chunk's keyframe", i)
		}
		frags, next, _ := mp4meta.ScanFragments(bytes.NewReader(frag), 0, int64(len(frag)))
		if len(frags) != 1 || next != int64(len(frag)) || !frags[0].Key || frags[0].Time != c.Time || frags[0].Dur != c.Dur {
			t.Fatalf("chunk %d: fragment parsed as %+v; the chunk is %+v", i, frags, c)
		}
		if bytes.Contains(frag[:moofSize], []byte{0, 0, 0, mp4meta.AudioTrackID, 0, 0, 0, 20, 't', 'f', 'd', 't'}) {
			t.Fatalf("chunk %d: the keyframe fragment has an audio part", i)
		}
	}
	if _, err := mp4meta.KeyframeFragment(r, mp4meta.Chunk{Offset: 3, Size: 100}, 1); err == nil {
		t.Fatal("an offset that is not a fragment must be refused")
	}
}
