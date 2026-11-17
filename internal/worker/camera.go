package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/mp4meta"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/stream"
)

// Hourly rollover timing (spec 6.5).
const (
	// RolloverLead is how long before the hour the second session is opened.
	RolloverLead = 10 * time.Second
	// RolloverGrace is how long after the hour the new session may still take
	// over before the old session starts the new file itself.
	RolloverGrace = 15 * time.Second
	// RefreshRetry is the interval at which a failed feed refresh is retried.
	RefreshRetry = 5 * time.Minute
	// refreshWindow bounds one refresh retry.
	refreshWindow = 25 * time.Second
	// redialDelay spaces repeated attempts to open the second session.
	redialDelay = 3 * time.Second
	// writerQueue is the capacity of the file writer's packet channel: several
	// seconds of a high-bitrate stream. It only fills when the disk stops.
	writerQueue = 4096
)

// outcome is why a recording run ended.
type outcome int

const (
	outStall    outcome = iota // the stream failed: close the file, back off, reconnect
	outStop                    // the camera was removed or the worker is shutting down
	outReconfig                // the configuration changed: reconnect now
	outBreak                   // break-before-make at the hour (single-session camera)
	outWrite                   // writing the file failed
)

// rollState tracks the make-before-break hand-over to a second session.
type rollState struct {
	boundary    time.Time // next top of the hour
	attempted   time.Time // boundary for which the hourly hand-over was started
	pending     bool      // a hand-over is in progress
	switchAfter time.Time // take over at the first keyframe received at or after this
	deadline    time.Time // give up on the hand-over after this
	refreshAt   time.Time // when to retry a refresh that failed; zero = none
	redialAt    time.Time // earliest time for the next dial attempt

	dialing    bool
	dialCh     chan dialResult
	dialCancel context.CancelFunc
	hourly     bool // the pending hand-over is the hourly one (not a retry)
}

type dialResult struct {
	sess *session
	err  *dialError
}

// camStats is the part of a camera's state the stats reporter reads.
type camStats struct {
	state      string
	stateSince time.Time
	message    string

	videoCodec string
	audioCodec string
	audioNote  string
	width      int
	height     int
	kfMs       int
	file       string
	fileBytes  int64
	single     bool

	frames     uint64
	bytes      uint64
	lastPacket time.Time
}

// Camera records one camera. All recording state belongs to its goroutine.
type Camera struct {
	w   *Worker
	id  string
	log *slog.Logger

	fan    *stream.Fanout
	reader *stream.Reader
	rs     camRestream // what the loopback restream knows about this camera

	// requests from the worker, picked up by the goroutine
	wake       chan struct{}
	pendingCfg atomic.Pointer[config.Camera]
	pendingDir atomic.Pointer[string]
	restartReq atomic.Pointer[string]
	stopReq    atomic.Bool
	done       chan struct{}

	// goroutine-owned
	cfg         config.Camera
	password    string
	passwordErr string
	staging     string
	backoff     *Backoff
	single      bool
	gapNext     bool
	hadFailure  bool
	used        map[string]bool
	jumpPending bool

	cur      *session
	proc     *stream.Processor
	next     *session
	nextProc *stream.Processor
	seg      *segment
	roll     rollState

	recordingSince time.Time
	clockRefWall   time.Time     // wall clock reading at the last anchor
	clockRefMono   time.Duration // monotonic reading at the same instant
	lastKeyDTS     int64
	prevBytes      uint64
	lastDropped    uint64
	lastDropWarn   time.Time
	audioWarned    string
	announced      uint64 // newest session reported to the guardian for the restream

	mu sync.Mutex
	st camStats
}

func newCamera(w *Worker, cfg config.Camera, staging string, usedNames []string) *Camera {
	c := &Camera{
		w:       w,
		id:      cfg.ID,
		log:     w.log.With("camera", cfg.ID),
		fan:     &stream.Fanout{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		backoff: NewBackoff(),
		staging: staging,
		used:    map[string]bool{},
		gapNext: true, // the first file after a start is never continuous
	}
	for _, name := range usedNames {
		c.used[name] = true
	}
	c.reader = c.fan.Register("file-writer", writerQueue)
	c.applyConfig(cfg)
	c.st.state = ipc.StateConnecting
	if !cfg.Enabled {
		c.st.state = ipc.StateDisabled
	}
	c.st.stateSince = time.Now()
	return c
}

// Subscribe registers an additional reader of this camera's packets. The
// reader gets a bounded channel that drops packets when full; the recorder
// never waits for it (spec 15).
func (c *Camera) Subscribe(name string, buffer int) *stream.Reader {
	return c.fan.Register(name, buffer)
}

// Unsubscribe removes a reader added with Subscribe.
func (c *Camera) Unsubscribe(r *stream.Reader) { c.fan.Unregister(r) }

func (c *Camera) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Update replaces the camera's configuration.
func (c *Camera) Update(cfg config.Camera) {
	c.pendingCfg.Store(&cfg)
	c.poke()
}

// SetStaging changes the folder used for the next file.
func (c *Camera) SetStaging(dir string) {
	c.pendingDir.Store(&dir)
	c.poke()
}

// Restart makes the camera close its file and reconnect.
func (c *Camera) Restart(reason string) {
	c.restartReq.Store(&reason)
	c.poke()
}

// Stop asks the camera to finalize its file and end.
func (c *Camera) Stop() {
	c.stopReq.Store(true)
	c.poke()
}

// anchorClock remembers the current relation between the wall clock and the
// monotonic clock.
func (c *Camera) anchorClock(wall time.Time) {
	c.clockRefWall = wall.Round(0)
	c.clockRefMono = time.Since(processStart)
}

// clockJumped reports whether the wall clock moved by more than
// ClockJumpThreshold against the monotonic clock since the last anchor
// (spec 6.6): an NTP step or someone changing the time.
func (c *Camera) clockJumped(wall time.Time) bool {
	mono := time.Since(processStart)
	drift := wall.Round(0).Sub(c.clockRefWall) - (mono - c.clockRefMono)
	if drift > ClockJumpThreshold || drift < -ClockJumpThreshold {
		c.anchorClock(wall)
		return true
	}
	if mono-c.clockRefMono > time.Minute {
		c.anchorClock(wall) // re-anchor so slow NTP slewing never adds up
	}
	return false
}

// onClockJump schedules a new file at the next keyframe and restarts the
// hourly schedule from the corrected time.
func (c *Camera) onClockJump(wall time.Time) {
	c.log.Warn("system clock changed; starting a new file at the next keyframe")
	// The worker reports the change once for the whole process; this one
	// belongs to the camera, so the timeline can show it (spec 16.5).
	c.emit(ipc.SevInfo, "clock_jump", "The system clock changed; a new file is started with the corrected time.", nil)
	c.jumpPending = c.seg != nil
	c.dropNext()
	c.roll = rollState{boundary: naming.NextBoundary(wall)}
}

func (c *Camera) applyConfig(cfg config.Camera) {
	c.cfg = cfg
	c.password, c.passwordErr = "", ""
	if cfg.PasswordEnc != "" {
		pw, err := config.Unprotect(cfg.PasswordEnc)
		if err != nil {
			c.passwordErr = "The saved password cannot be read on this computer. Enter the camera password again."
		}
		c.password = pw
	}
}

// recorderChanged reports whether a config change affects the connection.
func recorderChanged(a, b config.Camera) bool {
	return a.Enabled != b.Enabled || a.Host != b.Host || a.Port != b.Port || a.Path != b.Path ||
		a.Username != b.Username || a.PasswordEnc != b.PasswordEnc || a.Transport != b.Transport ||
		a.RecordAudio != b.RecordAudio
}

// ctlAction is what the goroutine must do after reading its requests.
type ctlAction int

const (
	ctlNone ctlAction = iota
	ctlStop
	ctlReconfig
	ctlRestart
)

func (c *Camera) readRequests() (ctlAction, string) {
	if c.stopReq.Load() {
		return ctlStop, ""
	}
	if dir := c.pendingDir.Swap(nil); dir != nil {
		c.staging = *dir
	}
	action, msg := ctlNone, ""
	if cfg := c.pendingCfg.Swap(nil); cfg != nil {
		changed := recorderChanged(c.cfg, *cfg)
		c.applyConfig(*cfg)
		if changed {
			action = ctlReconfig
		}
	}
	if r := c.restartReq.Swap(nil); r != nil && action == ctlNone {
		action, msg = ctlRestart, *r
	}
	return action, msg
}

func (c *Camera) setState(state, message string) {
	c.mu.Lock()
	changed := c.st.state != state
	if changed {
		c.st.state = state
		c.st.stateSince = time.Now()
	}
	c.st.message = message
	c.mu.Unlock()
	if changed {
		c.log.Info("camera state", "state", state, "message", message)
		c.w.reportNow(c.id) // the guardian learns of every state change at once
	}
}

func (c *Camera) emit(severity, code, message string, data map[string]any) {
	c.w.send(&ipc.Event{CameraID: c.id, Severity: severity, Code: code, Message: message, Data: data})
}

// run is the camera goroutine.
func (c *Camera) run() {
	defer close(c.done)
	c.recoverParts()
	for {
		if c.cycle() {
			return
		}
	}
}

// cycle performs one connect-and-record run. It returns true when the camera
// must end. A panic anywhere in the camera's code is recovered here, logged
// and treated as a stream failure for this camera only (spec 4.2).
func (c *Camera) cycle() (stop bool) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("camera goroutine panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			c.emit(ipc.SevWarning, "camera_panic", fmt.Sprintf("Internal error in the recorder: %v. The camera is reconnecting.", r), nil)
			c.abortAll()
			c.gapNext, c.hadFailure = true, true
			c.setState(ipc.StateBackoff, "internal error; reconnecting")
			stop = c.wait(c.backoff.Next())
		}
	}()

	switch action, _ := c.readRequests(); action {
	case ctlStop:
		return true
	}
	if !c.cfg.Enabled {
		c.setState(ipc.StateDisabled, "")
		return c.wait(time.Hour)
	}
	if c.passwordErr != "" {
		c.setState(ipc.StateError, c.passwordErr)
		return c.wait(ErrorRetry)
	}

	c.setState(ipc.StateConnecting, c.currentMessage())
	sess, derr, act := c.dial()
	switch act {
	case ctlStop:
		return true
	case ctlReconfig, ctlRestart:
		return false
	}
	if derr != nil {
		return c.dialFailed(derr)
	}

	c.cur = sess
	c.prevBytes = 0
	proc, err := stream.NewProcessor(sess.videoFormat, sess.audioFormat())
	if err != nil {
		c.closeSessions()
		c.setState(ipc.StateError, err.Error())
		return c.wait(ErrorRetry)
	}
	c.proc = proc
	c.publishSessions()
	c.noteAudio()

	out, msg := c.record()
	switch out {
	case outStop:
		c.finish(ipc.ReasonShutdown)
		return true
	case outReconfig:
		c.finish(ipc.ReasonShutdown)
		c.gapNext = true
		return false
	case outBreak:
		c.finish(ipc.ReasonHourRollover)
		c.gapNext = true
		return false
	}
	// stall or write failure
	c.finish(ipc.ReasonStall)
	c.gapNext, c.hadFailure = true, true
	if out == outWrite {
		c.emit(ipc.SevCritical, "write_failed", "Writing the recording failed: "+msg, nil)
	} else {
		c.emit(ipc.SevWarning, "stream_stalled", msg, nil)
	}
	c.setState(ipc.StateBackoff, msg)
	return c.wait(c.backoff.Next())
}

func (c *Camera) currentMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.message
}

func (c *Camera) dialFailed(derr *dialError) (stop bool) {
	first := !c.hadFailure
	c.hadFailure = true
	c.gapNext = true
	switch derr.kind {
	case dialAuth:
		c.setState(ipc.StateError, "Authentication failed")
		if first {
			c.emit(ipc.SevWarning, "auth_failed", "Authentication failed: check the camera user name and password.", nil)
		}
		return c.wait(ErrorRetry)
	case dialUnsupported:
		c.setState(ipc.StateError, derr.msg)
		if first {
			c.emit(ipc.SevWarning, "unsupported_codec", derr.msg, nil)
		}
		return c.wait(ErrorRetry)
	}
	c.setState(ipc.StateBackoff, derr.msg)
	if first {
		c.emit(ipc.SevWarning, "camera_unreachable", "Cannot connect: "+derr.msg, nil)
	}
	return c.wait(c.backoff.Next())
}

// wait sleeps for d or until a request arrives. It returns true to stop.
func (c *Camera) wait(d time.Duration) (stop bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return false
		case <-c.wake:
			switch action, _ := c.readRequests(); action {
			case ctlStop:
				return true
			case ctlReconfig, ctlRestart:
				return false
			}
		}
	}
}

// dial connects the primary session while staying responsive to requests.
func (c *Camera) dial() (*session, *dialError, ctlAction) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan dialResult, 1)
	opt := dialOptions{id: c.w.nextSessionID(), cam: c.cfg, password: c.password, publish: c.fan.Publish, now: c.w.now}
	go func() {
		s, err := dialSession(ctx, opt)
		var de *dialError
		if err != nil {
			de = classifyDialError(err)
		}
		ch <- dialResult{s, de}
	}()
	for {
		select {
		case r := <-ch:
			return r.sess, r.err, ctlNone
		case <-c.wake:
			if action, _ := c.readRequests(); action != ctlNone {
				cancel()
				if r := <-ch; r.sess != nil {
					r.sess.close()
				}
				return nil, nil, action
			}
		}
	}
}

func (c *Camera) noteAudio() {
	note := c.cur.audioNote
	if c.proc.AudioErr != "" {
		note = c.proc.AudioErr
	}
	audioCodec := ""
	if c.proc.AudioParams != nil {
		audioCodec = c.proc.AudioParams.Name
	}
	c.mu.Lock()
	c.st.videoCodec = c.cur.videoCodec
	c.st.audioCodec = audioCodec
	c.st.audioNote = note
	c.st.width, c.st.height = c.proc.Width, c.proc.Height
	c.mu.Unlock()
	if note != "" && note != c.audioWarned {
		c.audioWarned = note
		c.log.Warn("audio is not being recorded", "reason", note)
		c.emit(ipc.SevWarning, "audio_not_recorded", note, nil)
	}
}

// record runs until the session must be given up.
func (c *Camera) record() (outcome, string) {
	now := c.w.now()
	c.anchorClock(now)
	c.roll = rollState{boundary: naming.NextBoundary(now)}
	c.lastKeyDTS = 0
	c.lastDropped = c.reader.Dropped()
	// Packets queued by an earlier session are stale.
	for len(c.reader.C) > 0 {
		<-c.reader.C
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		var nextDone <-chan struct{}
		if c.next != nil {
			nextDone = c.next.done
		}
		select {
		case pkt := <-c.reader.C:
			if out, msg, done := c.handlePacket(pkt); done {
				return out, msg
			}

		case res := <-c.roll.dialCh:
			c.nextDialed(res)

		case <-c.cur.done:
			if c.next == nil {
				return outStall, "stream ended: " + c.sessionError(c.cur)
			}
			// The camera dropped the first session once the second one was
			// playing. Continue on the second and remember that this camera
			// cannot hold two sessions.
			if !c.single && time.Since(c.next.playedAt) < 2*RolloverLead {
				c.markSingle("the camera closed the first session when a second one connected")
			}
			c.closeSegment(ipc.ReasonStall)
			c.gapNext = true
			c.cur.close()
			c.cur, c.proc = c.next, c.nextProc
			c.next, c.nextProc = nil, nil
			c.publishSessions()
			c.prevBytes = c.cur.bytes.Load()
			c.roll.pending = false
			c.roll.refreshAt = time.Time{}
			c.lastKeyDTS = 0
			c.setState(ipc.StateConnecting, "")
			c.noteAudio()

		case <-nextDone:
			c.log.Warn("second session ended before it could take over", "error", c.sessionError(c.next))
			c.dropNext()

		case <-ticker.C:
			if out, msg, done := c.tick(); done {
				return out, msg
			}

		case <-c.wake:
			switch action, msg := c.readRequests(); action {
			case ctlStop:
				return outStop, ""
			case ctlReconfig:
				return outReconfig, ""
			case ctlRestart:
				return outStall, msg
			}
		}
	}
}

func (c *Camera) sessionError(s *session) string {
	if s.err == nil {
		return "connection closed"
	}
	return config.Redact(s.err.Error())
}

func (c *Camera) handlePacket(pkt *stream.Packet) (outcome, string, bool) {
	switch {
	case pkt.Session == c.cur.id:
		for _, u := range c.proc.Push(pkt) {
			if out, msg, done := c.handleSample(u); done {
				return out, msg, true
			}
		}
		if c.proc.AudioErr != "" && c.proc.AudioErr != c.audioWarned {
			c.noteAudio()
		}
	case c.next != nil && pkt.Session == c.next.id:
		for _, u := range c.nextProc.Push(pkt) {
			if c.next == nil { // took over earlier in this packet
				if out, msg, done := c.handleSample(u); done {
					return out, msg, true
				}
				continue
			}
			if u.Video && u.Key && !u.Recv.Before(c.roll.switchAfter) {
				if out, msg, done := c.switchTo(u); done {
					return out, msg, true
				}
			}
		}
	}
	return 0, "", false
}

// handleSample writes one sample of the current session.
func (c *Camera) handleSample(u stream.Sample) (outcome, string, bool) {
	if !u.Video {
		if c.seg != nil {
			c.seg.writeAudio(u)
		}
		return 0, "", false
	}

	c.mu.Lock()
	c.st.frames++
	if u.Key && c.lastKeyDTS != 0 && u.DTS > c.lastKeyDTS {
		c.st.kfMs = int((u.DTS - c.lastKeyDTS) * 1000 / mp4meta.VideoTimescale)
	}
	c.mu.Unlock()
	if u.Key {
		c.lastKeyDTS = u.DTS
	}

	switch {
	case c.seg == nil:
		if !u.Key {
			return 0, "", false
		}
		c.proc.TakeParamsDirty()
		if err := c.openSegment(u, c.gapNext); err != nil {
			return outWrite, err.Error(), true
		}
		c.gapNext = false
	case u.Key:
		// Check the clock here as well as in tick, so a jump across the hour
		// is recorded as a clock change and not as an ordinary rollover.
		// The wall clock is read now, not taken from the packet: a packet
		// that waited in the queue (a slow disk) would otherwise look like
		// a clock step, since the monotonic side is always "now".
		if wall := c.w.now(); c.clockJumped(wall) {
			c.onClockJump(wall)
		}
		reason := ""
		switch {
		case c.proc.TakeParamsDirty() && !c.proc.Params.Equal(c.seg.vp):
			reason = ipc.ReasonParamChange
		case c.jumpPending:
			reason = ipc.ReasonClockJump
		case !c.single && !c.roll.pending && !u.Recv.Before(c.seg.hourEnd):
			// The hour is over and no second session is taking over: start
			// the new hour's file on this session.
			reason = ipc.ReasonHourRollover
		}
		if reason != "" {
			c.jumpPending = false
			c.closeSegment(reason)
			if err := c.openSegment(u, false); err != nil {
				return outWrite, err.Error(), true
			}
		}
	}
	if err := c.seg.writeVideo(u); err != nil {
		return outWrite, err.Error(), true
	}
	return 0, "", false
}

// switchTo hands recording over to the second session at keyframe u: the old
// file is closed with everything received so far, the new file starts with u,
// and only then is the old session torn down (make-before-break).
func (c *Camera) switchTo(u stream.Sample) (outcome, string, bool) {
	c.closeSegment(ipc.ReasonHourRollover)
	old := c.cur
	c.cur, c.proc = c.next, c.nextProc
	c.next, c.nextProc = nil, nil
	c.publishSessions()
	c.prevBytes = c.cur.bytes.Load()
	c.roll.pending = false
	c.roll.refreshAt = time.Time{}
	c.roll.boundary = naming.NextBoundary(u.Recv)
	c.lastKeyDTS = u.DTS
	c.jumpPending = false
	c.proc.TakeParamsDirty()

	if err := c.openSegment(u, false); err != nil {
		go old.close()
		return outWrite, err.Error(), true
	}
	c.gapNext = false
	if err := c.seg.writeVideo(u); err != nil {
		go old.close()
		return outWrite, err.Error(), true
	}
	go old.close()
	c.mu.Lock()
	c.st.frames++
	c.mu.Unlock()
	c.noteAudio()
	c.log.Info("feed refreshed", "file", c.seg.name())
	return 0, "", false
}

func (c *Camera) nameTaken(final string) bool { return c.used[final] }

func (c *Camera) openSegment(u stream.Sample, gap bool) error {
	seg, err := openSegment(c.staging, c.id, u.Recv, c.proc.Params, c.proc.AudioParams, gap, c.nameTaken)
	if err != nil {
		return err
	}
	c.seg = seg
	final := filepath.Base(seg.finalPath)
	c.used[final] = true
	for name := range c.used { // forget names of hours long past
		if p, ok := naming.Parse(name); ok && seg.start.Sub(p.HourStart) > 3*time.Hour {
			delete(c.used, name)
		}
	}
	if c.recordingSince.IsZero() || gap {
		c.recordingSince = time.Now()
	}
	wasFailing := c.hadFailure
	c.setState(ipc.StateRecording, "")
	if wasFailing {
		c.hadFailure = false
		c.emit(ipc.SevInfo, "camera_recovered", "Recording resumed.", nil)
	}
	c.mu.Lock()
	c.st.file = seg.name()
	c.st.fileBytes = seg.bytes
	c.st.width, c.st.height = c.proc.Width, c.proc.Height
	c.mu.Unlock()
	c.w.send(&ipc.SegmentOpened{
		CameraID: c.id, Path: seg.partPath, Start: ipc.T(seg.start), Gap: gap,
		VideoCodec: seg.vp.Codec, AudioCodec: seg.audioName(),
	})
	return nil
}

// closeSegment finalizes the current file and reports it.
func (c *Camera) closeSegment(reason string) {
	if c.seg == nil {
		return
	}
	seg := c.seg
	c.seg = nil
	c.mu.Lock()
	c.st.file, c.st.fileBytes = "", 0
	c.mu.Unlock()
	cs, err := seg.close(reason)
	if err != nil {
		c.log.Error("closing the recording failed; repairing it", "file", seg.partPath, "error", err.Error())
		c.repairPartAsync(seg.partPath)
		return
	}
	c.w.send(&ipc.SegmentClosed{
		CameraID: c.id, Path: cs.path, Start: ipc.T(cs.start), End: ipc.T(cs.end), Bytes: cs.bytes,
		SHA256: cs.sha256, Gap: cs.gap, Reason: reason, VideoCodec: cs.videoCodec, AudioCodec: cs.audioCodec,
	})
}

func (c *Camera) closeSessions() {
	if c.roll.dialCancel != nil {
		c.roll.dialCancel()
	}
	if c.roll.dialing {
		ch := c.roll.dialCh
		go func() { // close the session if the dial still succeeds
			if r := <-ch; r.sess != nil {
				r.sess.close()
			}
		}()
	}
	c.roll = rollState{}
	if c.next != nil {
		go c.next.close()
	}
	if c.cur != nil {
		go c.cur.close()
	}
	c.cur, c.proc, c.next, c.nextProc = nil, nil, nil, nil
	c.publishSessions()
}

// finish closes the file cleanly and drops the sessions.
func (c *Camera) finish(reason string) {
	c.closeSegment(reason)
	c.closeSessions()
}

// abortAll is used after a panic: nothing about the open file is trusted, so
// it is closed as it is and repaired like a crash-cut file.
func (c *Camera) abortAll() {
	defer func() { recover() }()
	if c.seg != nil {
		seg := c.seg
		c.seg = nil
		seg.abort()
		c.repairPartAsync(seg.partPath)
	}
	c.closeSessions()
}

// tick runs four times a second: stall detection, clock changes and the
// hourly hand-over.
func (c *Camera) tick() (outcome, string, bool) {
	wall := c.w.now()

	last := c.cur.lastVideoTime()
	if c.cur.videoSilence() > StallTimeout {
		if c.seg != nil {
			return outStall, "stream stalled: no video for 10 seconds", true
		}
		return outStall, "no video received after connecting", true
	}

	if d := c.reader.Dropped(); d != c.lastDropped {
		lost := d - c.lastDropped
		c.lastDropped = d
		c.proc.Reset() // resynchronize at the next keyframe
		if wall.Sub(c.lastDropWarn) > time.Minute {
			c.lastDropWarn = wall
			c.emit(ipc.SevWarning, "packets_dropped",
				fmt.Sprintf("The disk could not keep up: %d packets were dropped.", lost), nil)
		}
	}

	if c.clockJumped(wall) {
		c.onClockJump(wall)
	}

	if c.seg != nil && c.backoff.Step() > 0 && time.Since(c.recordingSince) > StableAfter {
		c.backoff.Reset()
	}

	c.mu.Lock()
	c.st.lastPacket = last
	c.st.bytes += c.cur.bytes.Load() - c.prevBytes
	c.prevBytes = c.cur.bytes.Load()
	if c.seg != nil {
		c.st.fileBytes = c.seg.bytes
	}
	c.st.single = c.single
	c.mu.Unlock()

	r := &c.roll
	if c.single {
		if !wall.Before(r.boundary) {
			r.boundary = naming.NextBoundary(wall)
			if c.seg != nil {
				return outBreak, "", true
			}
		}
		return 0, "", false
	}

	// Open the second session ten seconds before the hour.
	if !r.pending && r.attempted != r.boundary && !wall.Before(r.boundary.Add(-RolloverLead)) {
		r.attempted = r.boundary
		r.pending, r.hourly = true, true
		r.switchAfter = r.boundary
		r.deadline = r.boundary.Add(RolloverGrace)
		r.refreshAt = time.Time{}
		c.startNextDial()
	}
	// Retry a refresh that failed at the hour.
	if !r.pending && !r.refreshAt.IsZero() && !wall.Before(r.refreshAt) {
		if wall.After(r.boundary.Add(-RolloverLead - refreshWindow)) {
			r.refreshAt = time.Time{} // the hourly hand-over is about to start anyway
		} else {
			r.pending, r.hourly = true, false
			r.switchAfter = wall
			r.deadline = wall.Add(refreshWindow)
			c.startNextDial()
		}
	}
	// Keep trying to connect while the hand-over window is open.
	if r.pending && c.next == nil && !r.dialing && !wall.Before(r.redialAt) && wall.Before(r.deadline.Add(-redialDelay)) {
		c.startNextDial()
	}
	if r.pending && wall.After(r.deadline) {
		hourly := r.hourly
		c.dropNext()
		r.pending = false
		if hourly {
			r.refreshAt = r.boundary.Add(RefreshRetry)
			c.log.Warn("second session was not ready by the hour; continuing on the existing session")
			c.emit(ipc.SevWarning, "refresh_failed",
				"The hourly feed refresh did not complete; recording continues on the existing connection and the refresh will be retried.", nil)
		} else {
			r.refreshAt = wall.Add(RefreshRetry - refreshWindow)
		}
	}
	if !r.pending && !wall.Before(r.boundary) {
		r.boundary = naming.NextBoundary(wall)
	}
	return 0, "", false
}

func (c *Camera) startNextDial() {
	r := &c.roll
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan dialResult, 1)
	r.dialing, r.dialCh, r.dialCancel = true, ch, cancel
	opt := dialOptions{id: c.w.nextSessionID(), cam: c.cfg, password: c.password, publish: c.fan.Publish, now: c.w.now}
	go func() {
		s, err := dialSession(ctx, opt)
		var de *dialError
		if err != nil {
			de = classifyDialError(err)
		}
		ch <- dialResult{s, de}
	}()
}

func (c *Camera) nextDialed(res dialResult) {
	r := &c.roll
	r.dialing, r.dialCh = false, nil
	if r.dialCancel != nil {
		r.dialCancel()
		r.dialCancel = nil
	}
	if res.err != nil {
		if res.err.kind == dialRefused {
			c.markSingle(res.err.msg)
			r.pending = false
			r.refreshAt = time.Time{}
			return
		}
		c.log.Warn("could not open the second session", "error", res.err.msg)
		r.redialAt = c.w.now().Add(redialDelay)
		return
	}
	if !r.pending {
		res.sess.close()
		return
	}
	proc, err := stream.NewProcessor(res.sess.videoFormat, res.sess.audioFormat())
	if err != nil {
		res.sess.close()
		return
	}
	c.next, c.nextProc = res.sess, proc
	c.publishSessions()
}

func (c *Camera) markSingle(why string) {
	if c.single {
		return
	}
	c.single = true
	c.log.Info("camera allows only one session; using break-before-make at the hour", "reason", why)
	c.emit(ipc.SevInfo, "single_session",
		"The camera allows only one connection. The stream is reconnected at each hour, leaving a short gap.", nil)
}

// dropNext abandons the second session.
func (c *Camera) dropNext() {
	r := &c.roll
	if r.dialCancel != nil {
		r.dialCancel()
		r.dialCancel = nil
	}
	if r.dialing {
		ch := r.dialCh
		go func() {
			if res := <-ch; res.sess != nil {
				res.sess.close()
			}
		}()
		r.dialing, r.dialCh = false, nil
	}
	if c.next != nil {
		go c.next.close()
		c.next, c.nextProc = nil, nil
		c.publishSessions()
	}
}

// recoverParts repairs .part.mp4 files left behind by a crash or power loss
// (spec 6.4). The list is taken before recording starts so the file about to
// be opened is never touched; the repair itself runs beside the recording.
func (c *Camera) recoverParts() {
	parts, _ := filepath.Glob(filepath.Join(c.staging, c.id, "*", "*"+naming.PartExt))
	var mine []string
	for _, p := range parts {
		if pp, ok := naming.Parse(filepath.Base(p)); ok && pp.Part && pp.CameraID == c.id {
			mine = append(mine, p)
			c.used[naming.FinalName(filepath.Base(p))] = true
		}
	}
	if len(mine) == 0 {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("recovering interrupted recordings panicked", "panic", fmt.Sprint(r))
			}
		}()
		for _, p := range mine {
			c.repairPart(p)
		}
	}()
}

// repairPartAsync repairs a file beside the recording: the repair re-reads
// the whole file to hash it, which must never hold up the next file.
func (c *Camera) repairPartAsync(part string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("repairing a recording panicked", "file", part, "panic", fmt.Sprint(r))
			}
		}()
		c.repairPart(part)
	}()
}

// repairPart turns an interrupted .part.mp4 into a closed recording.
func (c *Camera) repairPart(part string) {
	info, sum, err := mp4meta.Repair(part, ipc.ReasonCrashRecovered)
	if errors.Is(err, mp4meta.ErrNoFragments) {
		os.Remove(part) // nothing was recorded in it
		return
	}
	if err != nil {
		c.log.Error("could not repair interrupted recording", "file", part, "error", err.Error())
		return
	}
	final := filepath.Join(filepath.Dir(part), naming.FinalName(filepath.Base(part)))
	if _, err := os.Stat(final); err == nil {
		c.log.Error("cannot recover interrupted recording: the closed file already exists", "file", final)
		return
	}
	if err := os.Rename(part, final); err != nil {
		c.log.Error("could not rename recovered recording", "file", part, "error", err.Error())
		return
	}
	start := info.Start
	if start.IsZero() {
		if p, ok := naming.Parse(filepath.Base(final)); ok {
			start = p.HourStart
		}
	}
	c.log.Info("recovered interrupted recording", "file", final, "bytes", info.Size)
	c.w.send(&ipc.SegmentClosed{
		CameraID: c.id, Path: final, Start: ipc.T(start.In(time.Local)), End: ipc.T(mp4meta.EndOf(start, info.Dur90k).In(time.Local)),
		Bytes: info.Size, SHA256: sum, Gap: true, Reason: ipc.ReasonCrashRecovered,
		VideoCodec: info.VideoCodec, AudioCodec: info.AudioCodec,
	})
}

// stats returns the periodic report for this camera.
func (c *Camera) stats(prev *statsBase, now time.Time) *ipc.CameraStats {
	c.mu.Lock()
	st := c.st
	c.mu.Unlock()

	out := &ipc.CameraStats{
		CameraID:           c.id,
		State:              st.state,
		StateSince:         ipc.T(st.stateSince),
		Message:            st.message,
		VideoCodec:         st.videoCodec,
		AudioCodec:         st.audioCodec,
		AudioNote:          st.audioNote,
		Width:              st.width,
		Height:             st.height,
		KeyframeIntervalMs: st.kfMs,
		CurrentFile:        st.file,
		CurrentFileBytes:   st.fileBytes,
		SingleSession:      st.single,
		DroppedPackets:     c.reader.Dropped(),
		RestreamDropped:    c.rs.droppedTotal(),
	}
	if !st.lastPacket.IsZero() {
		out.LastPacketTS = ipc.T(st.lastPacket)
	}
	if prev == nil {
		return out // an immediate state-change report carries no rates
	}
	from := prev.at
	if st.stateSince.After(from) {
		from = st.stateSince // rates cover only the time spent recording
	}
	if dt := now.Sub(from).Seconds(); dt > 0.5 && !prev.at.IsZero() && st.state == ipc.StateRecording {
		out.FPS = float64(int(float64(st.frames-prev.frames)/dt*10+0.5)) / 10
		out.BitrateKbps = int(float64(st.bytes-prev.bytes) * 8 / dt / 1000)
	}
	prev.at, prev.frames, prev.bytes = now, st.frames, st.bytes
	return out
}

type statsBase struct {
	at     time.Time
	frames uint64
	bytes  uint64
}
