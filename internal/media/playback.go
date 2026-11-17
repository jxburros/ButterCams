package media

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// Playback limits (spec 16.5 and 16.9).
const (
	// HandleIdleClose is how long an unused file handle is kept. Closing it
	// releases the space of a file that was deleted meanwhile.
	HandleIdleClose = 30 * time.Second
	// OpenTimeout bounds opening one candidate copy of a file. A share that
	// is down can hang for a long time; the next copy is tried instead.
	OpenTimeout = 3 * time.Second
	// IndexCacheSize is the number of closed files whose index is kept.
	IndexCacheSize = 256
	// ReadBudget is the total rate of playback reads from the staging
	// volume, in bytes per second. Recorders write there; viewers wait.
	ReadBudget = 40 << 20
	// MaxKeyframes is the most keyframe fragments one request returns.
	MaxKeyframes = 120
	// openIndexIdle is how long the index of a file being written is kept
	// after its last use.
	openIndexIdle = 2 * time.Minute
)

// Playback serves recorded files to the browser's player: a small index per
// file and plain byte ranges (spec 16.5). It keeps no per-viewer state.
type Playback struct {
	log *slog.Logger

	budget *bucket

	mu sync.Mutex
	// Tunables; tests shorten them through the setters.
	idleClose   time.Duration
	openTimeout time.Duration
	cacheSize   int
	// open opens a file for reading. It must share write and delete access.
	open func(path string) (*os.File, error)

	staging []string
	handles map[string]*handle
	cache   map[string]*list.Element // closed files: path → entry in lru
	lru     *list.List               // of *cacheEntry, most recent first
	growing map[string]*growingIndex // files being written, by path

	stop     chan struct{}
	wake     chan struct{} // restarts the janitor's wait after a setting changed
	stopOnce sync.Once
}

type handle struct {
	f        *os.File
	path     string
	refs     int
	lastUsed time.Time
}

type cacheEntry struct {
	path string
	size int64
	mod  time.Time
	ix   *mp4meta.Index
}

// growingIndex is the index of a file that is still being written. It is
// extended on each request and never goes back.
type growingIndex struct {
	mu       sync.Mutex
	ix       *mp4meta.Index
	lastUsed time.Time
}

// NewPlayback creates the playback file server.
func NewPlayback(log *slog.Logger) *Playback {
	p := &Playback{
		log:         log,
		idleClose:   HandleIdleClose,
		openTimeout: OpenTimeout,
		cacheSize:   IndexCacheSize,
		open:        winsvc.OpenShared,
		budget:      newBucket(ReadBudget),
		handles:     map[string]*handle{},
		cache:       map[string]*list.Element{},
		lru:         list.New(),
		growing:     map[string]*growingIndex{},
		stop:        make(chan struct{}),
		wake:        make(chan struct{}, 1),
	}
	go p.janitor()
	return p
}

// SetIdleClose changes how long an unused file handle is kept.
func (p *Playback) SetIdleClose(d time.Duration) {
	p.mu.Lock()
	p.idleClose = d
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// SetOpenTimeout changes how long one candidate copy may take to open.
func (p *Playback) SetOpenTimeout(d time.Duration) {
	p.mu.Lock()
	p.openTimeout = d
	p.mu.Unlock()
}

// SetCacheSize changes how many closed-file indexes are kept.
func (p *Playback) SetCacheSize(n int) {
	p.mu.Lock()
	p.cacheSize = n
	p.mu.Unlock()
}

// SetOpener replaces the function that opens files (tests).
func (p *Playback) SetOpener(open func(path string) (*os.File, error)) {
	p.mu.Lock()
	p.open = open
	p.mu.Unlock()
}

// SetReadBudget changes the rate of reads from the staging volume, in bytes
// per second.
func (p *Playback) SetReadBudget(bytesPerSec int64) { p.budget.setRate(bytesPerSec) }

// SetStagingRoots names the folders whose reads count against the read
// budget (spec 16.9, rule 5).
func (p *Playback) SetStagingRoots(roots []string) {
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			clean = append(clean, strings.ToLower(filepath.Clean(r))+string(filepath.Separator))
		}
	}
	p.mu.Lock()
	p.staging = clean
	p.mu.Unlock()
}

func (p *Playback) inStaging(path string) bool {
	path = strings.ToLower(filepath.Clean(path))
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, root := range p.staging {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}

// OpenFiles returns the number of file handles currently held open.
func (p *Playback) OpenFiles() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.handles)
}

// Close releases every file handle.
func (p *Playback) Close() {
	p.stopOnce.Do(func() { close(p.stop) })
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, h := range p.handles {
		h.f.Close()
		delete(p.handles, path)
	}
}

// janitor closes handles nobody has used for IdleClose and forgets the
// indexes of files that are no longer being watched while they grow.
func (p *Playback) janitor() {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("playback janitor panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	for {
		p.mu.Lock()
		every := p.idleClose / 4
		p.mu.Unlock()
		if every < 10*time.Millisecond {
			every = 10 * time.Millisecond
		}
		if every > 5*time.Second {
			every = 5 * time.Second
		}
		select {
		case <-p.stop:
			return
		case <-p.wake:
			continue
		case <-time.After(every):
		}
		p.sweep(time.Now())
	}
}

func (p *Playback) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, h := range p.handles {
		if h.refs == 0 && now.Sub(h.lastUsed) >= p.idleClose {
			h.f.Close()
			delete(p.handles, path)
		}
	}
	for path, g := range p.growing {
		g.mu.Lock()
		idle := now.Sub(g.lastUsed)
		g.mu.Unlock()
		if idle > openIndexIdle {
			delete(p.growing, path)
		}
	}
}

// ------------------------------------------------------------------ files

// file is one opened copy of a recording for the duration of a request.
type file struct {
	p       *Playback
	h       *handle
	path    string
	size    int64
	mod     time.Time
	staging bool
}

func (f *file) release() {
	f.p.mu.Lock()
	f.h.refs--
	f.h.lastUsed = time.Now()
	f.p.mu.Unlock()
}

// ReadAt reads from the file, waiting for the read budget when the file is
// on the staging volume.
func (f *file) ReadAt(b []byte, off int64) (int, error) {
	if f.staging {
		f.p.budget.take(len(b))
	}
	return f.h.f.ReadAt(b, off)
}

var errNoCopy = errors.New("no copy of the recording can be opened")

// acquire opens the first candidate that can be opened, each within
// OpenTimeout. Handles are shared between requests and kept for a while.
func (p *Playback) acquire(ctx context.Context, paths []string) (*file, error) {
	for _, path := range paths {
		p.mu.Lock()
		h := p.handles[path]
		if h != nil {
			h.refs++
		}
		timeout, open := p.openTimeout, p.open
		p.mu.Unlock()

		type result struct {
			f   *os.File
			st  os.FileInfo
			err error
		}
		ch := make(chan result, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					ch <- result{err: fmt.Errorf("opening panicked: %v", r)}
				}
			}()
			var res result
			if h != nil {
				res.f = h.f
			} else if res.f, res.err = open(path); res.err != nil {
				ch <- res
				return
			}
			res.st, res.err = res.f.Stat()
			if res.err == nil && res.st.IsDir() {
				res.err = errors.New("is a directory")
			}
			if res.err != nil && h == nil {
				res.f.Close()
				res.f = nil
			}
			ch <- res
		}()

		abandon := func() {
			if h != nil {
				p.mu.Lock()
				h.refs--
				p.mu.Unlock()
				return
			}
			go func() {
				if r := <-ch; r.f != nil {
					r.f.Close()
				}
			}()
		}
		timer := time.NewTimer(timeout)
		select {
		case res := <-ch:
			timer.Stop()
			if res.err != nil {
				if h != nil { // a pooled handle that stopped working
					p.mu.Lock()
					h.refs--
					if p.handles[path] == h && h.refs == 0 {
						h.f.Close()
						delete(p.handles, path)
					}
					p.mu.Unlock()
				}
				continue
			}
			p.mu.Lock()
			if h == nil {
				if existing := p.handles[path]; existing != nil {
					// Another request opened it meanwhile; one handle is enough.
					res.f.Close()
					h = existing
				} else {
					h = &handle{f: res.f, path: path}
					p.handles[path] = h
				}
				h.refs++
			}
			h.lastUsed = time.Now()
			p.mu.Unlock()
			return &file{p: p, h: h, path: path, size: res.st.Size(), mod: res.st.ModTime(), staging: p.inStaging(path)}, nil
		case <-timer.C:
			abandon()
			p.log.Warn("a copy of a recording did not open in time; trying the next one", "timeout", timeout.String())
		case <-ctx.Done():
			timer.Stop()
			abandon()
			return nil, ctx.Err()
		}
	}
	return nil, errNoCopy
}

// index returns the playback index of an opened file. The result must not
// be modified; for a growing file it is a snapshot.
func (p *Playback) index(f *file) (*mp4meta.Index, error) {
	if strings.HasSuffix(f.path, naming.PartExt) {
		return p.growingIndex(f)
	}
	p.mu.Lock()
	if el := p.cache[f.path]; el != nil {
		e := el.Value.(*cacheEntry)
		if e.size == f.size && e.mod.Equal(f.mod) {
			p.lru.MoveToFront(el)
			p.mu.Unlock()
			return e.ix, nil
		}
		p.lru.Remove(el)
		delete(p.cache, f.path)
	}
	p.mu.Unlock()

	ix, err := mp4meta.ReadIndex(f, f.size)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if el := p.cache[f.path]; el != nil {
		p.lru.Remove(el)
	}
	p.cache[f.path] = p.lru.PushFront(&cacheEntry{path: f.path, size: f.size, mod: f.mod, ix: ix})
	for p.lru.Len() > p.cacheSize {
		last := p.lru.Back()
		delete(p.cache, last.Value.(*cacheEntry).path)
		p.lru.Remove(last)
	}
	return ix, nil
}

// growingIndex returns the index of a file that is being written, extended
// up to the fragments complete right now.
func (p *Playback) growingIndex(f *file) (*mp4meta.Index, error) {
	p.mu.Lock()
	g := p.growing[f.path]
	if g == nil {
		g = &growingIndex{}
		p.growing[f.path] = g
	}
	p.mu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastUsed = time.Now()
	if g.ix == nil {
		ix, err := mp4meta.ReadHeader(f, f.size)
		if err != nil {
			return nil, err
		}
		g.ix = ix
	}
	mp4meta.ExtendIndex(f, f.size, g.ix)
	snap := *g.ix
	// A copy: ExtendIndex changes the last chunk in place as the file grows,
	// while this snapshot is read outside the lock.
	snap.Chunks = append([]mp4meta.Chunk(nil), g.ix.Chunks...)
	return &snap, nil
}

// CachedIndexes returns how many closed-file indexes are cached.
func (p *Playback) CachedIndexes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.Len()
}

// ------------------------------------------------------------------ handlers

// with opens the file of a request and its index, and answers the errors.
func (p *Playback) with(w http.ResponseWriter, r *http.Request, paths []string, serve func(f *file, ix *mp4meta.Index)) {
	f, err := p.acquire(r.Context(), paths)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		fail(w, http.StatusNotFound, "not_found", "No copy of this recording can be reached right now.")
		return
	}
	defer f.release()
	ix, err := p.index(f)
	switch {
	case errors.Is(err, mp4meta.ErrNotReady):
		fail(w, http.StatusNotFound, "not_ready", "This recording has only just started. Try again in a moment.")
		return
	case err != nil:
		p.log.Warn("a recording could not be read for playback", "file", filepath.Base(f.path), "error", err.Error())
		fail(w, http.StatusUnprocessableEntity, "unreadable", "This recording cannot be read for playback. Download the file instead.")
		return
	}
	serve(f, ix)
}

func cacheHeader(w http.ResponseWriter, ix *mp4meta.Index, maxAge int) {
	if ix.Closed {
		w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(maxAge))
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
}

// ms converts 90 kHz ticks to milliseconds, to the microsecond: precise
// enough to get the exact tick back, so files can be joined without a hole.
func ms(ticks uint64) float64 {
	return math.Round(float64(ticks)/90*1000) / 1000
}

// indexJSON is the answer of /play/index (PlayIndex in the web client).
type indexJSON struct {
	MIME string `json:"mime"`
	// VideoMIME is the type of the video track alone: what a player needs
	// for the keyframe-only stream, which has no audio.
	VideoMIME  string       `json:"videoMime"`
	Init       [2]int64     `json:"init"`
	Start      *string      `json:"start"`
	DurationMs float64      `json:"durationMs"`
	Chunks     [][4]float64 `json:"chunks"`
	Open       bool         `json:"open"`
	Width      int          `json:"width"`
	Height     int          `json:"height"`
	HasAudio   bool         `json:"hasAudio"`
}

// ServeIndex is GET /play/index. paths are the candidate locations of the
// file, nearest first; open is true for the file still being recorded.
func (p *Playback) ServeIndex(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	p.with(w, r, paths, func(f *file, ix *mp4meta.Index) {
		out := indexJSON{
			MIME: ix.MIME(), VideoMIME: ix.VideoMIME(), Init: [2]int64{0, ix.InitSize},
			DurationMs: ms(uint64(ix.Dur90k)), Chunks: make([][4]float64, 0, len(ix.Chunks)),
			Open: !ix.Closed, Width: ix.Width, Height: ix.Height, HasAudio: ix.HasAudio(),
		}
		if !ix.Start.IsZero() {
			s := ix.Start.Format(mp4meta.TimeFormat)
			out.Start = &s
		}
		for _, c := range ix.Chunks {
			out.Chunks = append(out.Chunks, [4]float64{ms(c.Time), ms(c.Dur), float64(c.Offset), float64(c.Size)})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		cacheHeader(w, ix, 3600)
		json.NewEncoder(w).Encode(out)
	})
}

// budgetReader charges sequential reads of a staging file to the budget.
type budgetReader struct {
	io.ReadSeeker
	b   *bucket
	ctx context.Context
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	b.b.take(len(p))
	return b.ReadSeeker.Read(p)
}

// ServeData is GET /play/data: byte ranges of the file. Of the file being
// written, only the header and the complete fragments can be read.
func (p *Playback) ServeData(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	if r.Header.Get("Range") == "" {
		fail(w, http.StatusBadRequest, "range_required", "Ask for a byte range of the recording.")
		return
	}
	p.with(w, r, paths, func(f *file, ix *mp4meta.Index) {
		limit := f.size
		if !ix.Closed {
			limit = ix.End
		}
		var body io.ReadSeeker = io.NewSectionReader(f.h.f, 0, limit)
		if f.staging {
			body = &budgetReader{ReadSeeker: body, b: p.budget, ctx: r.Context()}
		}
		w.Header().Set("Content-Type", "video/mp4")
		cacheHeader(w, ix, 3600)
		http.ServeContent(w, r, "", time.Time{}, body)
	})
}

// msParam reads a time in milliseconds from the query.
func msParam(r *http.Request, name string) (float64, bool) {
	v, err := strconv.ParseFloat(r.URL.Query().Get(name), 64)
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// ServeKeyframes is GET /play/keyframes?from=&to=: for every chunk that
// overlaps [from, to) (milliseconds from the start of the file), a fragment
// holding only its first frame, lasting as long as the chunk. With init=1
// the answer starts with the video-only initialization segment.
func (p *Playback) ServeKeyframes(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	from, ok1 := msParam(r, "from")
	to, ok2 := msParam(r, "to")
	if !ok1 || !ok2 || to <= from {
		fail(w, http.StatusBadRequest, "bad_request", "Give the range as from and to, in milliseconds from the start of the file.")
		return
	}
	p.with(w, r, paths, func(f *file, ix *mp4meta.Index) {
		// Each fragment is written as soon as it is read: an answer of up to
		// MaxKeyframes large keyframes is never held in memory at once.
		w.Header().Set("Content-Type", "video/mp4")
		cacheHeader(w, ix, 3600)
		if r.URL.Query().Get("init") == "1" {
			if _, err := w.Write(ix.VideoInit); err != nil {
				return
			}
		}
		n := 0
		for i, c := range ix.Chunks {
			if ms(c.Time+c.Dur) <= from || ms(c.Time) >= to {
				continue
			}
			if n == MaxKeyframes {
				break
			}
			frag, err := mp4meta.KeyframeFragment(f, c, uint32(i+1))
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				p.log.Debug("a keyframe could not be read", "file", filepath.Base(f.path), "error", err.Error())
				continue
			}
			if _, err := w.Write(frag); err != nil {
				return
			}
			n++
		}
	})
}

// ServeThumb is GET /play/thumb?t=: the keyframe at or before t as a
// complete small MP4 (video-only header plus one fragment).
func (p *Playback) ServeThumb(w http.ResponseWriter, r *http.Request, paths []string, open bool) {
	t, ok := msParam(r, "t")
	if !ok {
		fail(w, http.StatusBadRequest, "bad_request", "Give the time as t, in milliseconds from the start of the file.")
		return
	}
	p.with(w, r, paths, func(f *file, ix *mp4meta.Index) {
		if len(ix.Chunks) == 0 {
			fail(w, http.StatusNotFound, "not_ready", "This recording has only just started. Try again in a moment.")
			return
		}
		at := 0
		for i, c := range ix.Chunks {
			if ms(c.Time) > t {
				break
			}
			at = i
		}
		frag, err := mp4meta.KeyframeFragment(f, ix.Chunks[at], 1)
		if err != nil {
			fail(w, http.StatusUnprocessableEntity, "unreadable", "No picture can be read at this time.")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		cacheHeader(w, ix, 86400)
		w.Header().Set("Content-Length", strconv.Itoa(len(ix.VideoInit)+len(frag)))
		w.Write(ix.VideoInit)
		w.Write(frag)
	})
}
