package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/media"
	"github.com/jxburros/buttercams/internal/media/mediatest"
	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/worker"
	"github.com/jxburros/buttercams/test/simulator"
)

// ------------------------------------------------------------ test tooling

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder runs a real recorder worker against the camera simulator.
type recorder struct {
	t       *testing.T
	staging string
	in      *io.PipeWriter
	enc     *ipc.Writer
	done    chan struct{}

	mu   sync.Mutex
	msgs []any
}

func startRecorder(t *testing.T, st simulator.Stream) *recorder {
	t.Helper()
	sim, err := simulator.Start("127.0.0.1:0", []simulator.Stream{st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &recorder{t: t, staging: t.TempDir(), in: inW, enc: ipc.NewWriter(inW), done: make(chan struct{})}
	go func() {
		worker.Run(context.Background(), 1, inR, outW, quietLog())
		outW.Close()
		close(r.done)
	}()
	go func() {
		rd := ipc.NewReader(outR)
		for {
			m, err := rd.Next()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.msgs = append(r.msgs, m)
			r.mu.Unlock()
		}
	}()
	host, portStr, _ := net.SplitHostPort(sim.Addr())
	port, _ := strconv.Atoi(portStr)
	r.enc.Send(&ipc.Assign{StagingPath: r.staging, Cameras: []config.Camera{{
		ID: "cam", Name: "cam", Enabled: true, Host: host, Port: port, Path: st.Path,
		Transport: "tcp", RecordAudio: true, RecordingMode: "continuous",
	}}})
	t.Cleanup(r.stop)
	return r
}

func (r *recorder) find(pred func(any) bool) any {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, m := range r.msgs {
			if pred(m) {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("the recorder did not report in time")
	return nil
}

// opened returns the path of the file being written.
func (r *recorder) opened() string {
	return r.find(func(m any) bool { _, ok := m.(*ipc.SegmentOpened); return ok }).(*ipc.SegmentOpened).Path
}

func (r *recorder) stop() {
	select {
	case <-r.done:
		return
	default:
	}
	r.enc.Send(&ipc.Shutdown{})
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		r.t.Error("the recorder did not stop")
	}
	r.in.Close()
}

// closed stops the recorder and returns the finished file.
func (r *recorder) closed() string {
	r.stop()
	return r.find(func(m any) bool { _, ok := m.(*ipc.SegmentClosed); return ok }).(*ipc.SegmentClosed).Path
}

// record makes one closed recording of about d.
func record(t *testing.T, st simulator.Stream, d time.Duration) string {
	t.Helper()
	r := startRecorder(t, st)
	r.opened()
	time.Sleep(d)
	return r.closed()
}

// player is a playback server with a client, as the guardian's proxy and a
// browser would use it.
func player(t *testing.T) (*media.Playback, *mediatest.PlaybackClient) {
	t.Helper()
	p := media.NewPlayback(quietLog())
	srv := httptest.NewServer(media.NewPlaybackHandler(p))
	t.Cleanup(func() { srv.Close(); p.Close() })
	return p, &mediatest.PlaybackClient{URL: srv.URL}
}

type frame struct {
	PTS float64
	Key bool
}

type probed struct {
	video []frame
	audio int
	codec string
}

// probe decodes data with ffprobe and ffmpeg: what plays here plays in a
// browser's demuxer too. The test is skipped without ffmpeg.
func probe(t *testing.T, data []byte) probed {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	path := filepath.Join(t.TempDir(), "probe.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "-v", "error", "-show_frames", "-show_streams",
		"-show_entries", "frame=media_type,pts_time,key_frame:stream=codec_name,codec_type", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var doc struct {
		Frames []struct {
			MediaType string `json:"media_type"`
			PTS       string `json:"pts_time"`
			Key       int    `json:"key_frame"`
		} `json:"frames"`
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var p probed
	for _, s := range doc.Streams {
		if s.CodecType == "video" {
			p.codec = s.CodecName
		}
	}
	for _, f := range doc.Frames {
		if f.MediaType == "video" {
			pts, _ := strconv.ParseFloat(f.PTS, 64)
			p.video = append(p.video, frame{pts, f.Key == 1})
		} else {
			p.audio++
		}
	}
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		msg, err := exec.Command(ff, "-v", "error", "-xerror", "-i", path, "-f", "null", "-").CombinedOutput()
		if err != nil || len(strings.TrimSpace(string(msg))) > 0 {
			t.Fatalf("ffmpeg could not decode the stream cleanly: %v %s", err, msg)
		}
	}
	return p
}

func one(path string) mediatest.File { return mediatest.File{Paths: []string{path}} }

// checkIndex verifies the index against the file: the chunks cover the file
// from the end of the header to the close marker exactly once.
func checkIndex(t *testing.T, path string, ix mediatest.PlayIndex) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Init[0] != 0 || ix.Init[1] <= 0 || len(ix.Chunks) == 0 {
		t.Fatalf("index: %+v", ix)
	}
	pos := float64(ix.Init[1])
	var at float64
	for i, c := range ix.Chunks {
		if c[2] != pos || c[3] <= 0 || c[1] <= 0 {
			t.Fatalf("chunk %d is %v; the previous one ended at byte %v", i, c, pos)
		}
		if d := c[0] - at; d > 0.02 || d < -0.02 {
			t.Fatalf("chunk %d starts at %v ms; the previous one ended at %v ms", i, c[0], at)
		}
		if string(data[int(c[2])+4:int(c[2])+8]) != "moof" {
			t.Fatalf("chunk %d does not start with a fragment", i)
		}
		pos += c[3]
		at = c[0] + c[1]
	}
	if d := at - ix.DurationMs; d > 0.02 || d < -0.02 {
		t.Fatalf("the chunks last %v ms, the file %v ms", at, ix.DurationMs)
	}
	// What follows the last chunk is the close marker and the mfra index.
	end := int(pos)
	if string(data[end+4:end+8]) != "free" || end+mp4meta.TrailerSize >= len(data) ||
		string(data[end+mp4meta.TrailerSize+4:end+mp4meta.TrailerSize+8]) != "mfra" {
		t.Fatalf("the chunks end at byte %d of %d, which is not the close marker", end, len(data))
	}
	info, err := mp4meta.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Start.Equal(info.Start) || ix.Open {
		t.Fatalf("start %v open %v; the file says %v", ix.Start, ix.Open, info.Start)
	}
	if d := ix.DurationMs - float64(info.Dur90k)/90; d > 0.01 || d < -0.01 {
		t.Fatalf("duration %v ms; the file says %v ticks", ix.DurationMs, info.Dur90k)
	}
}

// ------------------------------------------------------------------ tests

var avcMIME = regexp.MustCompile(`^video/mp4; codecs="avc1\.[0-9a-f]{6}, mp4a\.40\.2"$`)

func TestPlaybackOfClosedFile(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam", Audio: "aac"}, 5*time.Second)
	p, c := player(t)

	ix, err := c.Index(one(path))
	if err != nil {
		t.Fatal(err)
	}
	if !avcMIME.MatchString(ix.MIME) || !ix.HasAudio || ix.Width != 320 || ix.Height != 240 ||
		ix.VideoMIME != strings.Replace(ix.MIME, ", mp4a.40.2", "", 1) {
		t.Fatalf("index: %+v", ix)
	}
	checkIndex(t, path, ix)
	if len(ix.Chunks) < 4 {
		t.Fatalf("a 5 s recording with a keyframe every second has %d chunks", len(ix.Chunks))
	}

	// The header plus every chunk, fetched with range requests, is the
	// recording: same frames as the file itself.
	whole, _, err := c.Whole(one(path))
	if err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(path)
	if !bytes.Equal(whole, file[:len(whole)]) {
		t.Fatal("the ranges do not add up to the file")
	}
	got, want := probe(t, whole), probe(t, file)
	if len(got.video) != len(want.video) || got.audio != want.audio || len(got.video) < 60 || got.audio == 0 {
		t.Fatalf("played %d video and %d audio frames; the file has %d and %d", len(got.video), got.audio, len(want.video), want.audio)
	}
	// Starting at any chunk works: the header and the chunks from the third on.
	init, _ := c.Range(one(path), ix.Init[0], ix.Init[1])
	tail, err := c.Range(one(path), int64(ix.Chunks[2][2]), int64(ix.Chunks[len(ix.Chunks)-1][2]+ix.Chunks[len(ix.Chunks)-1][3]-ix.Chunks[2][2]))
	if err != nil {
		t.Fatal(err)
	}
	if part := probe(t, append(init, tail...)); len(part.video) == 0 || !part.video[0].Key || part.video[0].PTS < ix.Chunks[2][0]/1000-0.01 {
		t.Fatalf("playing from the third chunk: %+v", part.video[:1])
	}

	// Range requests only, and nothing past the end.
	res, err := c.Do("data", nil, one(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a request without a range: %d", res.StatusCode)
	}
	res, _ = c.Do("data", nil, one(path), http.Header{"Range": {"bytes=99999999-"}})
	res.Body.Close()
	if res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("a range past the end: %d", res.StatusCode)
	}
	res, _ = c.Do("data", nil, one(path), http.Header{"Range": {"bytes=0-9"}})
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || res.Header.Get("Cache-Control") != "private, max-age=3600" ||
		res.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("a range of a closed file: %d %v", res.StatusCode, res.Header)
	}
	// A file that is nowhere.
	res, _ = c.Do("index", nil, one(path+".gone"), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing file: %d", res.StatusCode)
	}
	// One handle serves all of this.
	if n := p.OpenFiles(); n != 1 {
		t.Fatalf("%d file handles open, want 1", n)
	}
}

func TestPlaybackOfH265(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam", Video: "h265"}, 4*time.Second)
	_, c := player(t)
	whole, ix, err := c.Whole(one(path))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^video/mp4; codecs="hvc1\.\d+\.[0-9A-F]+\.[LH]\d+(\.[0-9A-F]+)*"$`).MatchString(ix.MIME) || ix.HasAudio ||
		ix.VideoMIME != ix.MIME {
		t.Fatalf("H.265 MIME type %q", ix.MIME)
	}
	checkIndex(t, path, ix)
	file, _ := os.ReadFile(path)
	got, want := probe(t, whole), probe(t, file)
	if got.codec != "hevc" || len(got.video) != len(want.video) || len(got.video) < 45 {
		t.Fatalf("played %d %s frames; the file has %d", len(got.video), got.codec, len(want.video))
	}
	kf, err := c.Keyframes(one(path), 0, ix.DurationMs, true)
	if err != nil {
		t.Fatal(err)
	}
	if k := probe(t, kf); len(k.video) != len(ix.Chunks) {
		t.Fatalf("%d keyframes for %d chunks", len(k.video), len(ix.Chunks))
	}
}

// Keyframe-only fragments decode to exactly one picture per chunk at the
// chunk's time, and a thumbnail is a complete MP4 with one picture.
func TestKeyframesAndThumbnails(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam", Audio: "aac"}, 6*time.Second)
	_, c := player(t)
	ix, err := c.Index(one(path))
	if err != nil {
		t.Fatal(err)
	}
	n := len(ix.Chunks)

	all, err := c.Keyframes(one(path), 0, ix.DurationMs, true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(all, []byte("mp4a")) {
		t.Fatal("the keyframe stream declares an audio track")
	}
	p := probe(t, all)
	if len(p.video) != n || p.audio != 0 {
		t.Fatalf("%d pictures and %d audio frames for %d chunks", len(p.video), p.audio, n)
	}
	for i, f := range p.video {
		if d := f.PTS*1000 - ix.Chunks[i][0]; !f.Key || d > 1 || d < -1 {
			t.Fatalf("picture %d: key=%v at %v ms; its chunk starts at %v ms", i, f.Key, f.PTS*1000, ix.Chunks[i][0])
		}
	}

	// A range in the middle returns the chunks it touches; without init=1
	// the answer is fragments only, to append after the video-only header.
	from, to := ix.Chunks[1][0]+10, ix.Chunks[3][0]+10
	part, err := c.Keyframes(one(path), from, to, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(part[4:8]) != "moof" {
		t.Fatal("fragments expected")
	}
	headerLen := bytes.Index(all, []byte("moof")) - 4
	if got := probe(t, append(append([]byte(nil), all[:headerLen]...), part...)); len(got.video) != 3 ||
		got.video[0].PTS*1000-ix.Chunks[1][0] > 1 {
		t.Fatalf("a range over three chunks gave %d pictures: %+v", len(got.video), got.video)
	}
	for _, q := range []string{"", "from=5&to=1", "from=a&to=b"} {
		res, _ := http.NewRequest("GET", c.URL+"/play/keyframes?"+q, nil)
		res.Header.Set(ipc.PathsHeader, media.EncodePaths([]string{path}))
		r, err := http.DefaultClient.Do(res)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("keyframes?%s: %d", q, r.StatusCode)
		}
	}

	// Thumbnails: the keyframe at or before the time asked for.
	for _, tc := range []struct {
		t     float64
		chunk int
	}{{0, 0}, {ix.Chunks[2][0] + 400, 2}, {ix.Chunks[2][0], 2}, {ix.DurationMs + 5000, n - 1}, {-50, 0}} {
		thumb, hdr, err := c.Thumb(one(path), tc.t)
		if err != nil {
			t.Fatal(err)
		}
		got := probe(t, thumb)
		if len(got.video) != 1 || !got.video[0].Key || got.audio != 0 {
			t.Fatalf("thumbnail at %v ms: %+v", tc.t, got)
		}
		if d := got.video[0].PTS*1000 - ix.Chunks[tc.chunk][0]; d > 1 || d < -1 {
			t.Fatalf("thumbnail at %v ms shows the picture of %v ms, want chunk %d (%v ms)", tc.t, got.video[0].PTS*1000, tc.chunk, ix.Chunks[tc.chunk][0])
		}
		if hdr.Get("Cache-Control") != "private, max-age=86400" || hdr.Get("Content-Type") != "video/mp4" || len(thumb) > 200_000 {
			t.Fatalf("thumbnail headers %v, %d bytes", hdr, len(thumb))
		}
	}
}

// The hour being recorded: the index only ever lists finished fragments,
// what it lists can be fetched and decoded at once, and reading it does not
// get in the way of the recorder closing, renaming or deleting the file.
func TestPlaybackOfTheFileBeingWritten(t *testing.T) {
	t.Parallel()
	rec := startRecorder(t, simulator.Stream{Path: "/cam", Audio: "aac"})
	part := rec.opened()
	if !strings.HasSuffix(part, ".part.mp4") {
		t.Fatalf("the open file is %s", part)
	}
	final := strings.TrimSuffix(part, ".part.mp4") + ".mp4"
	p, c := player(t)
	p.SetIdleClose(300 * time.Millisecond)
	open := mediatest.File{Paths: []string{part, final}, Open: true}

	var snaps []mediatest.PlayIndex
	deadline := time.Now().Add(15 * time.Second)
	for len(snaps) < 12 && time.Now().Before(deadline) {
		ix, err := c.Index(open)
		if err != nil {
			time.Sleep(100 * time.Millisecond) // the header is not on disk yet
			continue
		}
		if !ix.Open {
			t.Fatal("the file being written must be reported as open")
		}
		if len(ix.Chunks) > 0 {
			last := ix.Chunks[len(ix.Chunks)-1]
			end := int64(last[2] + last[3])
			// Everything listed is there right now ...
			if _, err := c.Range(open, int64(last[2]), int64(last[3])); err != nil {
				t.Fatalf("a listed chunk cannot be fetched: %v", err)
			}
			// ... and nothing after it can be read, even though the
			// recorder has already written more.
			res, _ := c.Do("data", nil, open, http.Header{"Range": {"bytes=" + strconv.FormatInt(end, 10) + "-"}})
			res.Body.Close()
			if res.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				// Every request extends the index of the growing file, so a
				// fragment finished in between makes the byte readable.
				// That is fine only if the index now lists it.
				now, err := c.Index(open)
				grown := err == nil && len(now.Chunks) > 0 &&
					int64(now.Chunks[len(now.Chunks)-1][2]+now.Chunks[len(now.Chunks)-1][3]) > end
				if !grown {
					t.Fatalf("reading past the last finished fragment answered %d", res.StatusCode)
				}
			}
			res, _ = c.Do("data", nil, open, http.Header{"Range": {"bytes=0-9"}})
			res.Body.Close()
			if cc := res.Header.Get("Cache-Control"); res.StatusCode != http.StatusPartialContent || cc != "no-store" {
				t.Fatalf("a range of the open file: %d, Cache-Control %q", res.StatusCode, cc)
			}
			if len(snaps) == 0 || len(ix.Chunks) > len(snaps[len(snaps)-1].Chunks) {
				snaps = append(snaps, ix)
			}
		}
		time.Sleep(350 * time.Millisecond)
	}
	if len(snaps) < 4 {
		t.Fatalf("the index of the open file grew only %d times", len(snaps))
	}
	// What is there so far plays.
	soFar, ix, err := c.Whole(open)
	if err != nil {
		t.Fatal(err)
	}
	if got := probe(t, soFar); len(got.video) < 30 || got.audio == 0 {
		t.Fatalf("the open file played %d video and %d audio frames", len(got.video), got.audio)
	}
	// It reaches to within a few seconds of now (spec: about 5 s behind live).
	if behind := time.Since(ix.Start.Add(time.Duration(ix.DurationMs) * time.Millisecond)); behind > 4*time.Second {
		t.Fatalf("the open file's index ends %v behind live", behind)
	}

	// A viewer is in the middle of a download when the recorder closes the
	// file and renames it.
	res, err := c.Do("data", nil, open, http.Header{"Range": {"bytes=0-" + strconv.Itoa(len(soFar)-1)}})
	if err != nil || res.StatusCode != http.StatusPartialContent {
		t.Fatalf("%v %v", res, err)
	}
	head := make([]byte, 100)
	if _, err := io.ReadFull(res.Body, head); err != nil {
		t.Fatal(err)
	}
	if got := rec.closed(); got != final {
		t.Fatalf("closed as %s, want %s", got, final)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("the .part.mp4 is still there after the close")
	}
	rest, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || !bytes.Equal(append(head, rest...), soFar) {
		t.Fatalf("the download that was running during the rename ended badly: %v, %d of %d bytes", err, len(rest)+100, len(soFar))
	}

	// The same request as before now sees the finished file: closed, with
	// every earlier snapshot a prefix of it.
	done, err := c.Index(open)
	if err != nil {
		t.Fatal(err)
	}
	if done.Open {
		t.Fatal("the file is closed but still reported as open")
	}
	checkIndex(t, final, done)
	for _, s := range snaps {
		for i, ch := range s.Chunks {
			f := done.Chunks[i]
			if ch[0] != f[0] || ch[2] != f[2] || (i < len(s.Chunks)-1 && ch != f) || ch[3] > f[3] {
				t.Fatalf("a snapshot listed chunk %d as %v; the closed file has %v", i, ch, f)
			}
		}
	}
	// After the handle of the old name idled out, the closed name is used.
	time.Sleep(700 * time.Millisecond)
	if n := p.OpenFiles(); n != 0 {
		t.Fatalf("%d handles still open after the idle time", n)
	}
	again, err := c.Index(open)
	if err != nil || again.Open || len(again.Chunks) != len(done.Chunks) {
		t.Fatalf("index through the closed name: %+v %v", again, err)
	}

	// The mover deletes the staging copy while a viewer reads it.
	whole, _ := os.ReadFile(final)
	res, err = c.Do("data", nil, open, http.Header{"Range": {"bytes=0-"}})
	if err != nil || res.StatusCode != http.StatusPartialContent {
		t.Fatalf("%v %v", res, err)
	}
	io.ReadFull(res.Body, head)
	if err := os.Remove(final); err != nil {
		t.Fatalf("deleting a file that is being played: %v", err)
	}
	rest, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || !bytes.Equal(append(head, rest...), whole) {
		t.Fatalf("the read that was running during the delete ended badly: %v", err)
	}
	// The handle is given up soon after, releasing the file's space, and
	// the file is then gone for viewers too.
	time.Sleep(700 * time.Millisecond)
	if n := p.OpenFiles(); n != 0 {
		t.Fatalf("%d handles held on a deleted file", n)
	}
	res, _ = c.Do("index", nil, open, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a deleted file answered %d", res.StatusCode)
	}
}

// A copy on a share that hangs is given up after the limit and the next
// copy is used.
func TestHangingCopyFallsBackToTheNext(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam"}, 2*time.Second)
	p, c := player(t)
	release := make(chan struct{})
	defer close(release)
	p.SetOpenTimeout(300 * time.Millisecond)
	p.SetOpener(func(name string) (*os.File, error) {
		if strings.HasPrefix(name, `\\nas\`) {
			<-release
			return nil, os.ErrNotExist
		}
		return os.Open(name)
	})
	start := time.Now()
	ix, err := c.Index(mediatest.File{Paths: []string{`\\nas\share\missing.mp4`, filepath.Join(t.TempDir(), "nothing.mp4"), path}})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 2*time.Second {
		t.Fatalf("answered after %v; the limit is 300 ms", took)
	}
	checkIndex(t, path, ix)
	// All copies hang or are missing: 404, still within the limit.
	start = time.Now()
	res, _ := c.Do("index", nil, mediatest.File{Paths: []string{`\\nas\share\a.mp4`, `\\nas\other\a.mp4`}}, nil)
	res.Body.Close()
	if took := time.Since(start); res.StatusCode != http.StatusNotFound || took > 2*time.Second {
		t.Fatalf("no reachable copy: %d after %v", res.StatusCode, took)
	}
}

// Reads from the staging volume share one budget; other volumes do not
// count against it (spec 16.9, rule 5).
func TestReadBudgetAppliesToStagingOnly(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam", PadBytes: 6000}, 3*time.Second)
	data, _ := os.ReadFile(path)
	if len(data) < 250_000 {
		t.Fatalf("the test recording is only %d bytes", len(data))
	}
	elsewhere := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(elsewhere, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p, c := player(t)
	p.SetStagingRoots([]string{filepath.Dir(filepath.Dir(filepath.Dir(path)))})
	p.SetReadBudget(100_000) // bytes per second

	timeRead := func(f string) time.Duration {
		start := time.Now()
		got, err := c.Range(one(f), 0, 250_000)
		if err != nil || !bytes.Equal(got, data[:250_000]) {
			t.Fatalf("reading %s: %v", f, err)
		}
		return time.Since(start)
	}
	if took := timeRead(elsewhere); took > time.Second {
		t.Fatalf("a file outside staging took %v", took)
	}
	// 250 kB at 100 kB/s with a quarter second of burst: about 2 s.
	if took := timeRead(path); took < 1500*time.Millisecond || took > 6*time.Second {
		t.Fatalf("a staging file took %v at 100 kB/s; want about 2 s", took)
	}
}

func TestIndexCacheIsBoundedAndHandlesIdleOut(t *testing.T) {
	t.Parallel()
	path := record(t, simulator.Stream{Path: "/cam"}, 2*time.Second)
	data, _ := os.ReadFile(path)
	dir := t.TempDir()
	p, c := player(t)
	p.SetCacheSize(3)
	p.SetIdleClose(200 * time.Millisecond)
	var first mediatest.PlayIndex
	for i := 0; i < 8; i++ {
		copyPath := filepath.Join(dir, "copy"+strconv.Itoa(i)+".mp4")
		os.WriteFile(copyPath, data, 0o644)
		ix, err := c.Index(one(copyPath))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = ix
		} else if len(ix.Chunks) != len(first.Chunks) {
			t.Fatal("copies of one file have different indexes")
		}
	}
	if n := p.CachedIndexes(); n != 3 {
		t.Fatalf("%d indexes cached, the bound is 3", n)
	}
	if n := p.OpenFiles(); n != 8 {
		t.Fatalf("%d handles open right after use, want 8", n)
	}
	time.Sleep(600 * time.Millisecond)
	if n := p.OpenFiles(); n != 0 {
		t.Fatalf("%d handles open after the idle time", n)
	}
	// A file that changed (same name, other content) gets a fresh index.
	changed := filepath.Join(dir, "copy7.mp4")
	short := record(t, simulator.Stream{Path: "/cam"}, 4*time.Second)
	other, _ := os.ReadFile(short)
	os.WriteFile(changed, other, 0o644)
	ix, err := c.Index(one(changed))
	if err != nil || len(ix.Chunks) <= len(first.Chunks) {
		t.Fatalf("the index of a replaced file is stale: %d chunks, %v", len(ix.Chunks), err)
	}
}
