package worker

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// Track identifies the stream a packet belongs to.
type Track uint8

// Tracks.
const (
	TrackVideo Track = iota
	TrackAudio
)

// Packet is one RTP packet exactly as received from a camera, with the timing
// information needed to use it.
type Packet struct {
	Session  uint64 // the RTSP session that produced it
	Track    Track
	RTP      *rtp.Packet
	PTS      int64 // presentation time in clock-rate units; valid when PTSValid
	PTSValid bool
	Recv     time.Time // wall clock (with monotonic reading) at receipt
}

// Reader receives a camera's packets on a bounded channel.
type Reader struct {
	Name    string
	C       chan *Packet
	dropped atomic.Uint64
}

// Dropped returns how many packets were discarded because the reader was not
// keeping up.
func (r *Reader) Dropped() uint64 { return r.dropped.Load() }

// Fanout distributes a camera's packets to registered readers (spec 15).
//
// Publish never blocks: a reader whose channel is full simply loses the
// packet and its drop counter goes up. In v1 the only reader is the camera's
// file writer; later versions register more readers through the same function
// without touching the recorder.
type Fanout struct {
	mu      sync.RWMutex
	readers []*Reader
}

// Register adds a reader with a channel of the given capacity.
func (f *Fanout) Register(name string, buffer int) *Reader {
	r := &Reader{Name: name, C: make(chan *Packet, buffer)}
	f.mu.Lock()
	f.readers = append(f.readers, r)
	f.mu.Unlock()
	return r
}

// Unregister removes a reader. Its channel is left open; the reader just
// stops receiving.
func (f *Fanout) Unregister(r *Reader) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, x := range f.readers {
		if x == r {
			f.readers = append(f.readers[:i:i], f.readers[i+1:]...)
			return
		}
	}
}

// Publish hands a packet to every reader without ever waiting.
func (f *Fanout) Publish(p *Packet) {
	f.mu.RLock()
	for _, r := range f.readers {
		select {
		case r.C <- p:
		default:
			r.dropped.Add(1)
		}
	}
	f.mu.RUnlock()
}
