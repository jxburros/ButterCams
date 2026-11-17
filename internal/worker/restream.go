package worker

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jxburros/buttercams/internal/ipc"
	"github.com/jxburros/buttercams/internal/restream"
	"github.com/jxburros/buttercams/internal/stream"
)

// The loopback restream (spec 16.3) is the only live-view code in a worker.
// Everything in this file runs on the worker's message loop or on the
// restream's own goroutines; a camera's goroutine only publishes which
// sessions it has (publishSessions), with one atomic store.

// camRestream is the restream's side of a camera.
type camRestream struct {
	// sessions is the list of the camera's playing sessions, oldest first,
	// replaced as a whole by the camera's goroutine.
	sessions atomic.Pointer[[]restream.Session]

	mu      sync.Mutex // never taken by the camera's goroutine
	reader  *stream.Reader
	dropped uint64 // packets lost by readers that are gone
}

// droppedTotal returns how many packets the restream of this camera lost
// because its client was not keeping up.
func (r *camRestream) droppedTotal() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.dropped
	if r.reader != nil {
		n += r.reader.Dropped()
	}
	return n
}

// publishSessions tells the restream which sessions the camera has now, and
// reports a session that just started playing to the guardian, so that live
// view can move to it without a freeze (spec 16.3). It runs on the camera's
// goroutine and never waits: one atomic store, and a message only when a
// session is new.
func (c *Camera) publishSessions() {
	list := make([]restream.Session, 0, 2)
	for _, s := range []*session{c.cur, c.next} {
		if s != nil {
			list = append(list, restream.Session{ID: s.id, Video: s.videoFormat, Audio: s.audioFormat()})
		}
	}
	c.rs.sessions.Store(&list)
	if !c.w.restreamOn.Load() {
		return
	}
	for _, s := range list {
		if s.ID > c.announced {
			c.announced = s.ID
			c.w.send(&ipc.RestreamSession{CameraID: c.id, Session: s.ID})
		}
	}
}

// restreamSource gives the restream what it needs from the worker.
type restreamSource struct{ w *Worker }

func (r restreamSource) camera(id string) *Camera {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	return r.w.cams[id]
}

func (r restreamSource) Sessions(id string) ([]restream.Session, bool) {
	c := r.camera(id)
	if c == nil {
		return nil, false
	}
	if p := c.rs.sessions.Load(); p != nil {
		return *p, true
	}
	return nil, true
}

func (r restreamSource) Subscribe(id string, buffer int) *stream.Reader {
	c := r.camera(id)
	if c == nil {
		return nil
	}
	c.rs.mu.Lock()
	defer c.rs.mu.Unlock()
	if c.rs.reader != nil { // cannot happen: the restream runs one pump per camera
		c.rs.dropped += c.rs.reader.Dropped()
		c.Unsubscribe(c.rs.reader)
	}
	c.rs.reader = c.Subscribe("restream", buffer)
	return c.rs.reader
}

func (r restreamSource) Unsubscribe(id string, reader *stream.Reader) {
	c := r.camera(id)
	if c == nil {
		return // the camera was removed; its fan-out went with it
	}
	c.Unsubscribe(reader)
	c.rs.mu.Lock()
	c.rs.dropped += reader.Dropped()
	if c.rs.reader == reader {
		c.rs.reader = nil
	}
	c.rs.mu.Unlock()
}

func (r restreamSource) Failed(id, reason string) {
	r.w.send(&ipc.Event{CameraID: id, Severity: ipc.SevWarning, Code: "restream_failed",
		Message: fmt.Sprintf("Internal error in live view of the main stream: %s. Recording is not affected; "+
			"live view of this camera's main stream is off until the camera reconnects.", reason)})
}

// setRestream switches the worker's restream on or off (the guardian sends
// this after every assignment and when the setting changes) and reports the
// port. It runs on the worker's message loop.
func (w *Worker) setRestream(msg *ipc.Restream) {
	if rs := w.rs.Load(); msg.Enabled && rs != nil && w.rsToken == msg.Token {
		w.send(&ipc.RestreamReady{WorkerID: w.id, Port: rs.Port()}) // already running
		return
	}
	w.closeRestream()
	if !msg.Enabled {
		w.send(&ipc.RestreamReady{WorkerID: w.id})
		return
	}
	rs, err := restream.Start(restreamSource{w}, restream.Options{Token: msg.Token, Log: w.log, PumpHook: w.pumpHook})
	if err != nil {
		w.log.Warn("the restream could not start; live view of main streams is off", "error", err.Error())
		w.send(&ipc.RestreamReady{WorkerID: w.id, Error: err.Error()})
		return
	}
	w.rs.Store(rs)
	w.rsToken = msg.Token
	w.restreamOn.Store(true)
	w.log.Info("restream listening", "port", rs.Port())
	w.send(&ipc.RestreamReady{WorkerID: w.id, Port: rs.Port()})
}

// closeRestream removes every trace of the restream: listener, clients,
// readers and goroutines.
func (w *Worker) closeRestream() {
	rs := w.rs.Swap(nil)
	if rs == nil {
		return
	}
	w.rsToken = ""
	w.restreamOn.Store(false)
	go func() { // never hold up the message loop
		defer func() { recover() }()
		rs.Close()
	}()
}

// pumpHook lets tests stall the restream or make it fail.
func (w *Worker) pumpHook(cameraID string) {
	if h := w.testPumpHook.Load(); h != nil {
		(*h)(cameraID)
	}
}
