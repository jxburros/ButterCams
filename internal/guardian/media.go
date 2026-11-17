package guardian

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/winsvc"
)

// The media process (spec 16.2): live view and playback for browsers. It is
// supervised like a worker, but nothing about recording depends on it.
const (
	MediaHeartbeatTimeout = 15 * time.Second
	// MediaMemoryLimit is the memory use above which the media process is
	// restarted. Unlike a worker it is restarted at once: viewers reconnect.
	MediaMemoryLimit = 1 << 30
	// MediaDownWarning is how long the media process may be down before the
	// health check reports a Warning. It is never Critical: recording is not
	// affected.
	MediaDownWarning = 2 * time.Minute
	// shareReconnect is how often playback re-establishes the connection to
	// a network share that needs credentials.
	shareReconnect = time.Minute
)

type mediaState struct {
	proc      *Proc
	token     string // of the current run; empty until it has started
	port      int    // 0 until media_ready
	stats     ipc.MediaStats
	downSince time.Time // when the process was last not ready; zero while ready

	restreamToken string
	restreams     map[int]int // worker id → loopback port of its restream

	shares map[string]time.Time // destination id → last ConnectShare
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is unavailable
	}
	return hex.EncodeToString(b)
}

// liveOn reports whether live view and playback are switched on.
func liveOn(cfg *config.Config) bool { return cfg.Live.Enabled }

// restreamOn reports whether workers should offer their main streams.
func restreamOn(cfg *config.Config) bool { return cfg.Live.Enabled && cfg.Live.MainStream }

// syncMedia starts or stops the media process to match the configuration.
func (g *Guardian) syncMedia() {
	cfg := g.config()
	g.mu.Lock()
	proc := g.media.proc
	switch {
	case liveOn(cfg) && proc == nil:
		proc = newProc(ProcSpec{
			Name:                "media",
			Exe:                 g.exe,
			Args:                []string{"media"},
			Env:                 []string{winsvc.EnvDataRoot + "=" + g.opts.DataRoot},
			HeartbeatTimeout:    MediaHeartbeatTimeout,
			BelowNormalPriority: true,
			OnStart:             func(p *Proc) { g.onMediaStart() },
			OnMessage:           func(p *Proc, msg any) { g.onMediaMessage(msg) },
			OnRestart:           func(p *Proc, reason string, loop bool) { g.onMediaRestart(reason, loop) },
		}, g.log, g.childLog("media"))
		proc.job = g.job
		g.media.proc = proc
		g.media.downSince = time.Now()
		g.mu.Unlock()
		proc.Start()
	case !liveOn(cfg) && proc != nil:
		g.media.proc = nil
		g.media.token, g.media.port = "", 0
		g.media.stats = ipc.MediaStats{}
		g.media.downSince = time.Time{}
		g.mu.Unlock()
		go g.safely("stop-media", func() { proc.Stop(3 * time.Second) })
	default:
		g.mu.Unlock()
	}
}

// onMediaStart gives a freshly started media process its token and cameras.
func (g *Guardian) onMediaStart() {
	g.mu.Lock()
	g.media.token = newToken()
	g.media.port = 0
	g.mu.Unlock()
	g.sendAssignMedia()
}

// sendAssignMedia brings the media process up to date: cameras, where their
// main streams can be read, and the staging folders. It is sent on every
// change of any of these; the media process applies the difference.
func (g *Guardian) sendAssignMedia() {
	cfg := g.config()
	roots := g.stagingRoots()
	g.mu.Lock()
	proc, token := g.media.proc, g.media.token
	msg := &ipc.AssignMedia{Token: token, Cameras: cfg.Cameras, Restreams: map[string]string{},
		RestreamToken: g.media.restreamToken, StagingRoots: roots}
	if restreamOn(cfg) {
		for camID, workerID := range g.camWorker {
			if port := g.media.restreams[workerID]; port != 0 {
				msg.Restreams[camID] = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			}
		}
	}
	g.mu.Unlock()
	if proc == nil || token == "" {
		return
	}
	proc.Send(msg)
}

func (g *Guardian) onMediaMessage(msg any) {
	switch m := msg.(type) {
	case *ipc.MediaReady:
		g.mu.Lock()
		g.media.port = m.Port
		g.media.downSince = time.Time{}
		g.mu.Unlock()
		g.log.Info("media process ready", "port", m.Port)
	case *ipc.MediaStats:
		g.mu.Lock()
		g.media.stats = *m
		g.mu.Unlock()
	case *ipc.Event:
		data := map[string]any{"message": m.Message}
		for k, v := range m.Data {
			data[k] = v
		}
		g.event(index.Event{TS: m.TS.Time, Source: "media", Type: m.Code, Severity: m.Severity, CameraID: m.CameraID, Data: data})
	}
}

func (g *Guardian) onMediaRestart(reason string, loop bool) {
	g.mu.Lock()
	// The token belongs to the run that ended. Without one nothing is sent
	// to the next run until its own start handler has made a new token and
	// reset the port, so a late assign cannot leave the port unknown.
	g.media.token = ""
	g.media.port = 0
	g.media.stats = ipc.MediaStats{}
	if g.media.downSince.IsZero() {
		g.media.downSince = time.Now()
	}
	g.mu.Unlock()
	if loop {
		g.event(index.Event{Source: "media", Type: "media_crash_loop", Severity: ipc.SevWarning,
			Data: map[string]any{"message": fmt.Sprintf("The live view process keeps restarting (%s). Recording is not affected.", reason), "reason": reason}})
		return
	}
	g.event(index.Event{Source: "media", Type: "media_restarted", Severity: ipc.SevInfo,
		Data: map[string]any{"message": fmt.Sprintf("The live view process was restarted (%s). Recording is not affected.", reason), "reason": reason}})
}

// restartHeavyMedia restarts the media process when it uses too much memory.
func (g *Guardian) restartHeavyMedia() {
	g.mu.Lock()
	proc := g.media.proc
	g.mu.Unlock()
	if proc != nil && proc.MemBytes() > MediaMemoryLimit {
		proc.Restart("memory use above 1 GB", 2*time.Second)
	}
}

// mediaHealth returns the health reason for a media process that has been
// down too long, if any.
func (g *Guardian) mediaHealth(now time.Time) (api.Reason, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.media.proc == nil || g.media.downSince.IsZero() || now.Sub(g.media.downSince) < MediaDownWarning {
		return api.Reason{}, false
	}
	return api.Reason{Severity: ipc.SevWarning, Code: "media_down",
		Message: "Live view and playback are not available: the live view process has been down for " +
			roundDuration(now.Sub(g.media.downSince)) + ". Recording is not affected."}, true
}

// ------------------------------------------------------- worker restreams

// sendRestream tells a worker whether to offer its loopback restream.
func (g *Guardian) sendRestream(p *Proc) {
	g.mu.Lock()
	token := g.media.restreamToken
	g.mu.Unlock()
	p.Send(&ipc.Restream{Enabled: restreamOn(g.config()), Token: token})
}

func (g *Guardian) onRestreamReady(workerID int, m *ipc.RestreamReady) {
	g.mu.Lock()
	if m.Port == 0 {
		delete(g.media.restreams, workerID)
	} else {
		g.media.restreams[workerID] = m.Port
	}
	g.mu.Unlock()
	if m.Error != "" {
		g.log.Warn("a worker could not start its restream; main-stream live view is off for its cameras",
			"worker", workerID, "error", m.Error)
	}
	g.sendAssignMedia()
}

// forgetRestream drops a worker's restream address when the worker stops.
func (g *Guardian) forgetRestream(workerID int) {
	g.mu.Lock()
	_, had := g.media.restreams[workerID]
	delete(g.media.restreams, workerID)
	g.mu.Unlock()
	if had {
		g.sendAssignMedia()
	}
}

func (g *Guardian) onRestreamSession(m *ipc.RestreamSession) {
	g.mu.Lock()
	proc := g.media.proc
	g.mu.Unlock()
	if proc != nil {
		proc.Send(&ipc.SourceChanged{CameraID: m.CameraID, Session: m.Session})
	}
}

// ------------------------------------------------------------ api.Backend

// Media returns the address of the media process and the token of its run.
func (g *Guardian) Media() (addr, token string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.media.proc == nil || g.media.port == 0 || g.media.token == "" || !g.media.proc.Running() {
		return "", "", false
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(g.media.port)), g.media.token, true
}

// MediaStatus reports on the media process.
func (g *Guardian) MediaStatus() api.MediaStatus {
	enabled := liveOn(g.config())
	g.mu.Lock()
	defer g.mu.Unlock()
	st := api.MediaStatus{Enabled: enabled, State: "off", Sources: []ipc.MediaSource{}}
	if g.media.proc == nil {
		return st
	}
	st.State = "starting"
	if g.media.port != 0 && g.media.proc.Running() {
		st.State = "running"
		st.Viewers, st.Overflows = g.media.stats.Viewers, g.media.stats.Overflows
		st.Sources = append(st.Sources, g.media.stats.Sources...)
		sort.Slice(st.Sources, func(i, j int) bool {
			if st.Sources[i].CameraID != st.Sources[j].CameraID {
				return st.Sources[i].CameraID < st.Sources[j].CameraID
			}
			return st.Sources[i].Stream < st.Sources[j].Stream
		})
	}
	st.MemBytes = g.media.proc.MemBytes()
	return st
}

// candidate is one place a segment's file may be read from.
type candidate struct {
	path string
	rank int // 0 staging, 1 local destination, 2 network share
	dest *config.Destination
}

// segmentCandidates lists where a segment can be read, nearest first:
// staging, then local destinations, then network shares (spec 11.3). For the
// segment still being recorded it is the .part.mp4 in staging, followed by
// its closed name in case the file is closed in this very moment.
func (g *Guardian) segmentCandidates(seg index.Segment) ([]candidate, error) {
	rel, ok := naming.RelPath(seg.FileName)
	if !ok {
		return nil, errors.New("not a recording name")
	}
	var cands []candidate
	if !seg.Closed() {
		dir, name := filepath.Split(rel)
		for _, root := range g.stagingRoots() {
			cands = append(cands, candidate{path: filepath.Join(root, dir, naming.PartName(name))})
		}
		for _, root := range g.stagingRoots() {
			cands = append(cands, candidate{path: filepath.Join(root, rel)})
		}
		return cands, nil
	}
	cfg := g.config()
	for _, c := range seg.Copies {
		if c.State != index.CopyVerified {
			continue
		}
		if c.DestinationID == ipc.StagingID {
			for _, root := range g.stagingRoots() {
				cands = append(cands, candidate{path: filepath.Join(root, rel)})
			}
			continue
		}
		d, ok := cfg.Destination(c.DestinationID)
		if !ok {
			continue
		}
		rank := 1
		if d.Type == config.DestUNC {
			rank = 2
		}
		dc := d
		cands = append(cands, candidate{path: filepath.Join(d.Path, rel), rank: rank, dest: &dc})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	return cands, nil
}

// SegmentPaths resolves a segment to the paths the media process may read
// it from. Shares that need credentials are connected here, in the guardian,
// because the media process holds none; the connection belongs to the logon
// session, so the media process can then open the file.
func (g *Guardian) SegmentPaths(ctx context.Context, id int64) (index.Segment, []string, error) {
	seg, err := g.db.Segment(id)
	if err != nil {
		return seg, nil, err
	}
	cands, err := g.segmentCandidates(seg)
	if err != nil {
		return seg, nil, err
	}
	paths := make([]string, 0, len(cands))
	for _, c := range cands {
		paths = append(paths, c.path)
		if c.dest == nil || c.dest.Type != config.DestUNC || c.dest.Username == "" {
			continue
		}
		g.mu.Lock()
		due := time.Since(g.media.shares[c.dest.ID]) > shareReconnect
		if due {
			g.media.shares[c.dest.ID] = time.Now()
		}
		g.mu.Unlock()
		if !due {
			continue
		}
		// A share that is down can take long to answer; it gets a moment and
		// then carries on in the background.
		done := make(chan struct{})
		go func(d config.Destination) {
			defer close(done)
			defer func() { recover() }()
			if pw, err := config.Unprotect(d.PasswordEnc); err == nil {
				winsvc.ConnectShare(d.Path, d.Username, pw)
			}
		}(*c.dest)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return seg, nil, ctx.Err()
		}
	}
	return seg, paths, nil
}
