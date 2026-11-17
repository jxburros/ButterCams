package worker

import (
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/ipc"
)

// outbox carries messages to the guardian without ever making the sender
// wait (spec rule 1). Camera goroutines report segment openings and closings
// and events from the recording path; a write to stdout blocks when the
// guardian is slow to read (a long database transaction, an index rebuild),
// and a camera goroutine blocked there stops recording.
//
// Messages that must all arrive (segments, events, restream notices) keep
// their order in a queue. Statistics and heartbeats are coalesced: only the
// latest of each is kept, so a backlog cannot grow without bound. One
// goroutine writes to the pipe.
type outbox struct {
	out *ipc.Writer
	log func(err error)

	mu        sync.Mutex
	queue     []any
	stats     map[string]*ipc.CameraStats
	statOrder []string
	beat      *ipc.Heartbeat
	wake      chan struct{}
	busy      bool // the writer holds a batch it has not finished writing
}

func newOutbox(out *ipc.Writer, log func(error)) *outbox {
	o := &outbox{out: out, log: log, stats: map[string]*ipc.CameraStats{}, wake: make(chan struct{}, 1)}
	go o.run()
	return o
}

// post queues a message. It never blocks.
func (o *outbox) post(msg any) {
	o.mu.Lock()
	switch m := msg.(type) {
	case *ipc.CameraStats:
		if _, ok := o.stats[m.CameraID]; !ok {
			o.statOrder = append(o.statOrder, m.CameraID)
		}
		o.stats[m.CameraID] = m
	case *ipc.Heartbeat:
		o.beat = m
	default:
		o.queue = append(o.queue, msg)
	}
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// take returns everything pending, in the order to send it.
func (o *outbox) take() []any {
	o.mu.Lock()
	defer o.mu.Unlock()
	batch := o.queue
	o.queue = nil
	if o.beat != nil {
		batch = append(batch, o.beat)
		o.beat = nil
	}
	for _, id := range o.statOrder {
		batch = append(batch, o.stats[id])
	}
	if len(o.statOrder) > 0 {
		o.stats = map[string]*ipc.CameraStats{}
		o.statOrder = nil
	}
	o.busy = len(batch) > 0
	return batch
}

func (o *outbox) run() {
	for range o.wake {
		for {
			batch := o.take()
			if len(batch) == 0 {
				break
			}
			for _, m := range batch {
				if err := o.out.Send(m); err != nil && o.log != nil {
					o.log(err)
				}
			}
		}
	}
}

// flush waits up to d for everything queued so far to be written. The worker
// calls it before exiting so its last segment_closed messages arrive.
func (o *outbox) flush(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		o.mu.Lock()
		empty := !o.busy && len(o.queue) == 0 && o.beat == nil && len(o.statOrder) == 0
		o.mu.Unlock()
		if empty {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case o.wake <- struct{}{}:
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}
