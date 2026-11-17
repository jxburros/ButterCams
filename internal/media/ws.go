package media

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Limits of one viewer's connection (spec 16.4).
const (
	// viewerQueueBytes and viewerQueueAge bound what waits for one viewer.
	// When either is passed the queue is emptied and the viewer resumes at
	// the next keyframe.
	viewerQueueBytes = 4 << 20
	viewerQueueAge   = 2 * time.Second
	// wsWriteTimeout is how long one write to a viewer may take.
	wsWriteTimeout = 10 * time.Second
	// wsPingInterval keeps a connection without video (camera offline) open
	// through proxies and tells a dead viewer from a quiet one.
	wsPingInterval = 15 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16 << 10,
	// The guardian checked the Origin header and is the only client.
	CheckOrigin: func(*http.Request) bool { return true },
}

// item is one WebSocket message waiting for a viewer.
type item struct {
	text bool
	data []byte
	at   time.Time
}

// viewer is one browser tile. The source appends to its queue and never
// waits; a goroutine of its own writes the queue to the socket.
type viewer struct {
	audio bool // asked for audio (single view)

	// Owned by the source, under its lock.
	// lastInit is the initialization segment this viewer was sent last; nil
	// before its first picture and after its queue overflowed.
	lastInit *initSeg

	mu     sync.Mutex
	q      []item
	bytes  int
	closed bool
	// joinAllowance lets the pictures replayed to a viewer that just joined
	// exceed the queue limit once: they are slices the source keeps anyway,
	// so this holds no extra memory. It ends at the viewer's first write.
	joinAllowance int
	notify        chan struct{}
	done          chan struct{}
}

func newViewer(audio bool) *viewer {
	return &viewer{audio: audio, notify: make(chan struct{}, 1), done: make(chan struct{})}
}

// push queues a message. It returns false when the queue overflowed and was
// emptied; the message is then not queued either.
func (v *viewer) push(text bool, data []byte, now time.Time) bool {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return true
	}
	if v.bytes+len(data) > viewerQueueBytes+v.joinAllowance || (len(v.q) > 0 && now.Sub(v.q[0].at) > viewerQueueAge) {
		v.q, v.bytes = nil, 0
		v.mu.Unlock()
		return false
	}
	v.q = append(v.q, item{text: text, data: data, at: now})
	v.bytes += len(data)
	v.mu.Unlock()
	select {
	case v.notify <- struct{}{}:
	default:
	}
	return true
}

// allowJoin lets the next n bytes of replayed pictures through (see
// joinAllowance).
func (v *viewer) allowJoin(n int) {
	v.mu.Lock()
	v.joinAllowance = n
	v.mu.Unlock()
}

// take removes and returns everything queued.
func (v *viewer) take() []item {
	v.mu.Lock()
	defer v.mu.Unlock()
	q := v.q
	v.q, v.bytes, v.joinAllowance = nil, 0, 0
	return q
}

// close ends the viewer; its writer stops and the socket is closed.
func (v *viewer) close() {
	v.mu.Lock()
	if !v.closed {
		v.closed = true
		v.q, v.bytes = nil, 0
		close(v.done)
	}
	v.mu.Unlock()
}

// serve writes the viewer's queue to the socket until the viewer leaves or
// is closed. It returns when the connection is finished.
func (v *viewer) serve(c *websocket.Conn) {
	defer c.Close()
	defer v.close()

	// The client sends nothing; reading is only how a closed connection and
	// control frames are noticed.
	go func() {
		defer func() { recover() }()
		defer v.close()
		c.SetReadLimit(4096)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-v.done:
			c.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, ""), time.Now().Add(time.Second))
			return
		case <-ping.C:
			if err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
				return
			}
		case <-v.notify:
			for _, it := range v.take() {
				typ := websocket.BinaryMessage
				if it.text {
					typ = websocket.TextMessage
				}
				c.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
				if err := c.WriteMessage(typ, it.data); err != nil {
					return
				}
			}
		}
	}
}
