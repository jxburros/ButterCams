// Package mediatest holds the test viewers for live view and playback: Go
// clients that do what the browser's player does and keep what it would
// append to its SourceBuffer, so that ffmpeg can decode it (V2 plan,
// section 4).
package mediatest

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LiveInit is an "init" message of the live WebSocket.
type LiveInit struct {
	Mime   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// LiveEvent is one message as it arrived.
type LiveEvent struct {
	At    time.Time
	Kind  string // "init", "state", "header" (initialization segment) or "fragment"
	Bytes int
	State string // the message of a "state" event
}

// LiveViewer is a live view client. Everything it receives is recorded.
type LiveViewer struct {
	conn *websocket.Conn
	done chan struct{}

	mu      sync.Mutex
	events  []LiveEvent
	inits   []LiveInit
	streams [][]byte // one per initialization segment: the header and the fragments after it
	expect  bool     // the next binary message is an initialization segment
	err     error
}

// DialLive opens a live stream, for example
// ws://127.0.0.1:1234/live/cam/sub, and starts recording it.
func DialLive(url string, header http.Header) (*LiveViewer, *http.Response, error) {
	c, res, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return nil, res, err
	}
	v := &LiveViewer{conn: c, done: make(chan struct{})}
	go v.read()
	return v, res, nil
}

// DialLiveStalled opens a live stream and never reads from it: a viewer
// whose browser tab is frozen or whose network is gone. Its receive buffer
// is kept small so that the server notices quickly.
func DialLiveStalled(url string, header http.Header) (*websocket.Conn, error) {
	d := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   1024,
		NetDial: func(network, addr string) (net.Conn, error) {
			c, err := net.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetReadBuffer(4096)
			}
			return c, nil
		},
	}
	c, _, err := d.Dial(url, header)
	return c, err
}

func (v *LiveViewer) read() {
	defer close(v.done)
	for {
		typ, data, err := v.conn.ReadMessage()
		now := time.Now()
		v.mu.Lock()
		if err != nil {
			v.err = err
			v.mu.Unlock()
			return
		}
		switch {
		case typ == websocket.TextMessage:
			var msg struct {
				Type    string `json:"type"`
				State   string `json:"state"`
				Message string `json:"message"`
				LiveInit
			}
			if json.Unmarshal(data, &msg) != nil {
				v.events = append(v.events, LiveEvent{At: now, Kind: "invalid", Bytes: len(data)})
				break
			}
			if msg.Type == "init" {
				v.inits = append(v.inits, msg.LiveInit)
				v.expect = true
				v.events = append(v.events, LiveEvent{At: now, Kind: "init", Bytes: len(data)})
			} else {
				v.events = append(v.events, LiveEvent{At: now, Kind: msg.Type, Bytes: len(data), State: msg.Message})
			}
		case v.expect:
			v.expect = false
			v.streams = append(v.streams, append([]byte(nil), data...))
			v.events = append(v.events, LiveEvent{At: now, Kind: "header", Bytes: len(data)})
		default:
			if n := len(v.streams); n > 0 {
				v.streams[n-1] = append(v.streams[n-1], data...)
			}
			v.events = append(v.events, LiveEvent{At: now, Kind: "fragment", Bytes: len(data)})
		}
		v.mu.Unlock()
	}
}

// Close leaves the stream.
func (v *LiveViewer) Close() {
	v.conn.Close()
	<-v.done
}

// Closed reports whether the server ended the stream.
func (v *LiveViewer) Closed() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

// Events returns everything received so far, in order.
func (v *LiveViewer) Events() []LiveEvent {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]LiveEvent(nil), v.events...)
}

// Inits returns the "init" messages received so far.
func (v *LiveViewer) Inits() []LiveInit {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]LiveInit(nil), v.inits...)
}

// States returns the messages of the "state" events received so far.
func (v *LiveViewer) States() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, e := range v.events {
		if e.Kind == "state" {
			out = append(out, e.State)
		}
	}
	return out
}

// Fragments returns how many media fragments have arrived.
func (v *LiveViewer) Fragments() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, e := range v.events {
		if e.Kind == "fragment" {
			n++
		}
	}
	return n
}

// Streams returns what a browser would have appended, split at every
// initialization segment: each element is a playable fragmented MP4.
func (v *LiveViewer) Streams() [][]byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([][]byte, len(v.streams))
	for i, s := range v.streams {
		out[i] = append([]byte(nil), s...)
	}
	return out
}

// Bytes returns everything appended, initialization segments included, as
// one byte stream.
func (v *LiveViewer) Bytes() []byte {
	return bytes.Join(v.Streams(), nil)
}

// Wait polls until cond is true or the timeout passes.
func (v *LiveViewer) Wait(timeout time.Duration, cond func(*LiveViewer) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond(v) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
