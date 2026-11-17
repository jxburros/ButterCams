package media

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/stream"
)

// Source states, as reported in media_stats.
const (
	statePlaying    = "playing"
	stateConnecting = "connecting"
	stateWaiting    = "waiting"
)

const (
	// maxGOPBytes bounds the pictures kept for viewers who join. A stream
	// whose keyframes are further apart than this makes new viewers wait
	// for its next keyframe instead.
	maxGOPBytes = 24 << 20
	// switchTimeout is how long a second session may take to deliver its
	// first keyframe before the make-before-break switch is abandoned.
	switchTimeout = 20 * time.Second
	// initAllowance covers the initialization segments sent with a replay.
	initAllowance = 64 << 10
)

type sourceKey struct {
	camera string
	stream string // "main" or "sub"
}

// frag is one fragment kept since the last keyframe.
type frag struct {
	data  []byte
	key   bool
	audio bool
}

type dialResult struct {
	c   *conn
	err *dialError
}

// source is one live stream of one camera, shared by all its viewers
// (spec 16.3). A goroutine of its own connects, reads and muxes; viewers
// only ever get messages appended to their queues.
type source struct {
	h   *Hub
	key sourceKey

	kick     chan struct{} // the assignment or the viewers changed
	switchCh chan struct{} // the camera has a new session to offer (main)
	quit     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// Owned by the goroutine.
	tgt      target
	cur      *conn
	next     *conn
	nextCh   chan dialResult
	nextStop context.CancelFunc
	nextAt   time.Time
	mux      muxer
	needKey  bool
	initConn uint64 // connection the initialization segments were built from
	back     backoff
	fail     string // why the last attempt failed

	mu      sync.Mutex
	viewers map[*viewer]struct{}
	state   string
	message string
	initV   *initSeg // video only
	initA   *initSeg // video and audio; nil while nobody listens
	// audioSoon is true while the stream has audio that a listener can get
	// but the header for it is not built yet. A listener who joins then is
	// held back for that moment rather than started without sound: a browser
	// cannot add a track to a stream it is already playing.
	audioSoon bool
	gop       []frag
	gopSize   int
	gopOK     bool
	linger    *time.Timer
}

func newSource(h *Hub, key sourceKey) *source {
	return &source{
		h: h, key: key,
		kick: make(chan struct{}, 1), switchCh: make(chan struct{}, 1),
		quit: make(chan struct{}), stopped: make(chan struct{}),
		viewers: map[*viewer]struct{}{},
		state:   stateConnecting, message: "Connecting to the camera…",
	}
}

func (s *source) poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// stop ends the source and disconnects its viewers.
func (s *source) stop() {
	s.stopOnce.Do(func() { close(s.quit) })
	s.mu.Lock()
	if s.linger != nil {
		s.linger.Stop()
	}
	for v := range s.viewers {
		v.close()
	}
	s.viewers = map[*viewer]struct{}{}
	s.mu.Unlock()
}

// ------------------------------------------------------------------ viewers

// attach adds a viewer. A source that is playing sends it the pictures since
// the last keyframe, so it shows something at once.
func (s *source) attach(v *viewer) {
	s.mu.Lock()
	if s.linger != nil {
		s.linger.Stop()
		s.linger = nil
	}
	s.viewers[v] = struct{}{}
	now := time.Now()
	if s.state == statePlaying && s.gopOK {
		v.allowJoin(s.gopSize + initAllowance)
		for _, f := range s.gop {
			s.sendLocked(v, f, now)
		}
	} else if s.state != statePlaying {
		v.push(true, stateMessage(s.message), now)
	}
	s.mu.Unlock()
	if v.audio {
		s.poke(s.kick) // the audio converter starts with the first listener
	}
}

// detach removes a viewer and reports how many are left.
func (s *source) detach(v *viewer) int {
	s.mu.Lock()
	delete(s.viewers, v)
	n := len(s.viewers)
	s.mu.Unlock()
	if v.audio {
		s.poke(s.kick)
	}
	return n
}

func (s *source) counts() (viewers, listeners int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for v := range s.viewers {
		if v.audio {
			listeners++
		}
	}
	return len(s.viewers), listeners
}

// sendLocked queues one fragment for one viewer. A viewer starts, and starts
// again after its queue overflowed, with an initialization segment followed
// by a keyframe; it changes to another initialization segment only at a
// keyframe, because browsers drop everything up to the next one after a new
// header.
func (s *source) sendLocked(v *viewer, f frag, now time.Time) {
	ok := true
	if f.audio {
		if v.lastInit == nil || v.lastInit != s.initA {
			return
		}
		ok = v.push(false, f.data, now)
	} else {
		if f.key {
			want := s.initV
			if v.audio && s.initA != nil {
				want = s.initA
			}
			if v.audio && v.lastInit == nil && s.initA == nil && s.audioSoon {
				return
			}
			if want != nil && v.lastInit != want {
				ok = v.push(true, want.message(), now) && v.push(false, want.data, now)
				v.lastInit = want
			}
		}
		if v.lastInit == nil {
			return
		}
		ok = ok && v.push(false, f.data, now)
	}
	if !ok {
		// A slow viewer: its queue was emptied. It resumes at the next
		// keyframe and affects nobody else (spec 16.4).
		v.lastInit = nil
		s.h.overflows.Add(1)
	}
}

func (s *source) broadcast(f frag) {
	now := time.Now()
	s.mu.Lock()
	if f.key && !f.audio {
		s.gop, s.gopSize, s.gopOK = s.gop[:0], 0, true
	}
	if s.gopOK {
		if s.gopSize+len(f.data) > maxGOPBytes {
			s.gop, s.gopSize, s.gopOK = nil, 0, false
		} else {
			s.gop = append(s.gop, f)
			s.gopSize += len(f.data)
		}
	}
	if s.state != statePlaying {
		s.state, s.message = statePlaying, ""
	}
	for v := range s.viewers {
		s.sendLocked(v, f, now)
	}
	s.mu.Unlock()
}

// setState records that there is no video and tells the viewers why.
func (s *source) setState(state, message string) {
	s.mu.Lock()
	changed := s.state != state || s.message != message
	s.state, s.message = state, message
	s.gop, s.gopSize, s.gopOK = nil, 0, false
	if changed {
		msg := stateMessage(message)
		now := time.Now()
		for v := range s.viewers {
			if !v.push(true, msg, now) {
				// Its queue was emptied, perhaps with the initialization
				// segment in it: it must get a new one with the next picture.
				v.lastInit = nil
				s.h.overflows.Add(1)
			}
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- goroutine

func (s *source) run() {
	defer close(s.stopped)
	for {
		if s.cycle() {
			return
		}
	}
}

func (s *source) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

// wait sleeps for d, or until the assignment changes, a new session is
// announced or the source stops. It returns true to stop.
func (s *source) wait(d time.Duration) (stop bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return false
		case <-s.switchCh:
			return false
		case <-s.kick:
			if s.h.targetOf(s.key) != s.tgt {
				return false
			}
		case <-s.quit:
			return true
		}
	}
}

// cycle is one attempt: connect, play until the stream ends. A panic is
// logged and treated as a failed attempt; it never leaves the goroutine.
func (s *source) cycle() (stop bool) {
	defer func() {
		if r := recover(); r != nil {
			s.h.log.Error("live source panicked", "camera", s.key.camera, "stream", s.key.stream,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			s.closeConns()
			s.setState(stateWaiting, "Live view had an internal error and is starting again.")
			stop = s.wait(2 * time.Second)
		}
	}()
	if s.quitting() {
		return true
	}
	s.tgt = s.h.targetOf(s.key)
	if !s.tgt.ok {
		s.fail = ""
		s.setState(stateWaiting, s.tgt.reason)
		return s.wait(time.Minute)
	}
	msg := s.fail
	if msg == "" {
		msg = "Connecting to the camera…"
	}
	s.setState(stateConnecting, msg)

	c, derr, stop := s.connect()
	if stop {
		return true
	}
	if c == nil {
		if derr == nil {
			return false // the assignment changed while connecting
		}
		s.fail = derr.msg
		s.setState(stateWaiting, derr.msg)
		return s.wait(s.retryDelay(derr.config))
	}
	s.cur, s.needKey = c, true

	why, again := s.play()
	played := time.Since(c.since)
	s.closeConns()
	if s.quitting() {
		return true
	}
	if again {
		return false
	}
	if played > stableAfter {
		s.back.reset()
	}
	s.fail = why
	s.setState(stateWaiting, why)
	return s.wait(s.retryDelay(false))
}

func (s *source) retryDelay(configProblem bool) time.Duration {
	if s.tgt.main {
		return s.h.MainRetry
	}
	if configProblem {
		return 30 * time.Second
	}
	return s.back.next()
}

// connect dials while staying responsive to changes.
func (s *source) connect() (*conn, *dialError, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan dialResult, 1)
	tgt, id := s.tgt, s.h.nextID()
	go func() {
		c, err := dial(ctx, tgt, id)
		var de *dialError
		if err != nil {
			de = classifyDial(err, tgt.main)
		}
		ch <- dialResult{c, de}
	}()
	abandon := func() {
		cancel()
		if r := <-ch; r.c != nil {
			r.c.close()
		}
	}
	for {
		select {
		case r := <-ch:
			return r.c, r.err, false
		case <-s.switchCh: // it is being connected already
		case <-s.kick:
			if s.h.targetOf(s.key) != s.tgt {
				abandon()
				return nil, nil, false
			}
		case <-s.quit:
			abandon()
			return nil, nil, true
		}
	}
}

func (s *source) closeConns() {
	s.dropNext()
	if s.cur != nil {
		go s.cur.close()
		s.cur = nil
	}
}

// dropNext abandons the second session of a switch.
func (s *source) dropNext() {
	if s.nextStop != nil {
		s.nextStop()
		s.nextStop = nil
	}
	if ch := s.nextCh; ch != nil {
		s.nextCh = nil
		go func() {
			if r := <-ch; r.c != nil {
				r.c.close()
			}
		}()
	}
	if s.next != nil {
		go s.next.close()
		s.next = nil
	}
}

// startSwitch opens a second connection while the first keeps playing
// (make-before-break, spec 16.3).
func (s *source) startSwitch() {
	if !s.tgt.main || s.next != nil || s.nextCh != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan dialResult, 1)
	tgt, id := s.tgt, s.h.nextID()
	s.nextCh, s.nextStop, s.nextAt = ch, cancel, time.Now()
	go func() {
		c, err := dial(ctx, tgt, id)
		var de *dialError
		if err != nil {
			de = classifyDial(err, true)
		}
		ch <- dialResult{c, de}
	}()
}

// play runs until the connection must be given up. It returns why, and
// whether to connect again at once.
func (s *source) play() (why string, again bool) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	s.syncAudio()
	for {
		var nextPkts chan *stream.Packet
		var nextDone chan struct{}
		if s.next != nil {
			nextPkts, nextDone = s.next.pkts, s.next.done
		}
		select {
		case p := <-s.cur.pkts:
			s.handle(s.cur, p)

		case p := <-nextPkts:
			s.handleNext(p)

		case r := <-s.nextCh:
			s.nextCh = nil
			if s.nextStop != nil {
				s.nextStop()
				s.nextStop = nil
			}
			// A refusal is normal: the announced session is the one being
			// played already.
			if r.c != nil {
				s.next = r.c
			}

		case <-s.cur.done:
			if s.next == nil {
				if s.nextCh != nil || s.tgt.main {
					return "The camera's stream is being connected again.", false
				}
				return "The camera closed the stream.", false
			}
			// The old stream ended before the new one showed a keyframe:
			// carry on with the new one; viewers resume at its next keyframe.
			go s.cur.close()
			s.cur, s.next, s.needKey = s.next, nil, true
			s.syncAudio()

		case <-nextDone:
			s.dropNext()

		case <-ticker.C:
			if s.cur.silence() > s.h.StallTimeout {
				return "The camera stopped sending video.", false
			}
			if d := s.cur.dropped.Load(); d != s.cur.lastDropped {
				s.cur.lastDropped = d
				s.cur.proc.Reset() // resynchronize at the next keyframe
				s.needKey = true
			}
			if (s.next != nil || s.nextCh != nil) && time.Since(s.nextAt) > switchTimeout {
				s.dropNext()
			}

		case <-s.switchCh:
			s.startSwitch()

		case <-s.kick:
			if s.h.targetOf(s.key) != s.tgt {
				return "", true
			}
			s.syncAudio()

		case <-s.quit:
			return "", false
		}
	}
}

// syncAudio creates the audio handler (and with it the G.711/G.726
// converter) when the first viewer listens, and removes it when the last
// one leaves (spec 16.4).
func (s *source) syncAudio() {
	c := s.cur
	_, listeners := s.counts()
	defer func() {
		// Listeners that were held back start now, from the pictures kept
		// since the last keyframe.
		now := time.Now()
		s.mu.Lock()
		s.audioSoon = c.audioFormat != nil && !c.audioOff
		if s.state == statePlaying && s.gopOK && (s.initA != nil || !s.audioSoon) {
			for v := range s.viewers {
				if v.audio && v.lastInit == nil {
					v.allowJoin(s.gopSize + initAllowance)
					for _, f := range s.gop {
						s.sendLocked(v, f, now)
					}
				}
			}
		}
		s.mu.Unlock()
	}()
	switch {
	case listeners > 0 && c.aproc == nil && c.audioFormat != nil && !c.audioOff:
		p, err := stream.NewProcessor(c.videoFormat, c.audioFormat)
		if err != nil || p.AudioParams == nil {
			c.audioOff = true
			return
		}
		c.aproc = p
		if s.initConn == c.id {
			s.buildInits(c)
		}
	case listeners == 0 && c.aproc != nil:
		c.aproc = nil
		s.mux.dropAudio()
		s.mu.Lock()
		s.initA = nil
		s.mu.Unlock()
	}
}

// buildInits makes the initialization segments for a connection's current
// parameters. Segments that did not change keep their identity, so viewers
// are not sent the same header twice.
func (s *source) buildInits(c *conn) {
	vp := c.proc.Params
	initV, err := buildInit(vp, nil, c.proc.Width, c.proc.Height)
	if err != nil {
		return
	}
	var initA *initSeg
	if c.aproc != nil && c.aproc.AudioParams != nil {
		if a, err := buildInit(vp, c.aproc.AudioParams, c.proc.Width, c.proc.Height); err == nil {
			initA = a
		}
	}
	s.mu.Lock()
	if !sameInit(s.initV, initV) {
		s.initV = initV
	}
	if initA == nil || !sameInit(s.initA, initA) {
		s.initA = initA
	}
	s.mu.Unlock()
	s.initConn = c.id
}

func (s *source) handle(c *conn, p *stream.Packet) {
	if p.Track == stream.TrackVideo {
		for _, u := range c.proc.Push(p) {
			s.onVideo(c, u)
		}
		return
	}
	if c.aproc == nil {
		return
	}
	if s.needKey {
		// After a reconnect the muxer's timeline still belongs to the old
		// connection until this one's first keyframe: audio placed now
		// would stay out of step with the video for good.
		return
	}
	ap := c.aproc.AudioParams
	for _, u := range c.aproc.Push(p) {
		if !u.Video && ap != nil && s.mux.audio(u, ap.Timescale) {
			s.flushAudio()
		}
	}
}

// handleNext watches the second session for its first keyframe and moves
// the viewers over at that frame.
func (s *source) handleNext(p *stream.Packet) {
	if p.Track != stream.TrackVideo {
		return
	}
	n := s.next
	for _, u := range n.proc.Push(p) {
		if s.cur == n {
			s.onVideo(n, u)
			continue
		}
		if !u.Key {
			continue
		}
		old := s.cur
		s.cur, s.next = n, nil
		s.needKey = false
		s.mux.dropAudio()
		go old.close()
		s.syncAudio()
		s.onVideo(n, u)
	}
}

func (s *source) onVideo(c *conn, u stream.Sample) {
	if s.needKey {
		if !u.Key {
			return
		}
		s.needKey = false
	}
	if u.Key && (s.initConn != c.id || c.proc.TakeParamsDirty()) {
		s.buildInits(c)
	}
	if s.initConn != c.id {
		return // no header could be built for this connection's parameters
	}
	s.broadcast(frag{data: s.mux.video(u, c.id), key: u.Key})
	s.flushAudio()
}

func (s *source) flushAudio() {
	if a := s.mux.flushAudio(); a != nil {
		s.broadcast(frag{data: a, audio: true})
	}
}

// ------------------------------------------------------------------ targets

// targetFor decides where a source connects to. It is called with the hub's
// lock held.
func targetFor(key sourceKey, cam config.Camera, known bool, restream, restreamToken string) target {
	if !known {
		return target{reason: "This camera no longer exists."}
	}
	if key.stream == "main" {
		if restream == "" {
			if !cam.Enabled {
				return target{reason: "Recording is turned off for this camera."}
			}
			return target{reason: "The main stream is not available right now."}
		}
		return target{ok: true, main: true, audio: true, url: "rtsp://" + restream + "/" + cam.ID,
			user: restreamUser, pass: restreamToken}
	}
	u, ok := cam.SubURL()
	if !ok {
		return target{reason: "This camera has no sub stream."}
	}
	pass := ""
	if cam.PasswordEnc != "" {
		pw, err := config.Unprotect(cam.PasswordEnc)
		if err != nil {
			return target{reason: "The saved camera password cannot be read on this computer."}
		}
		pass = pw
	}
	return target{ok: true, url: u, user: cam.Username, pass: pass, udp: cam.Transport == "udp"}
}
