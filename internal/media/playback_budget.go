package media

import (
	"sync"
	"time"
)

// bucket is the read budget (spec 16.9, rule 5): a token bucket shared by
// every playback read from the staging volume. When it is used up, readers
// wait; recorders keep writing at full speed.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second; 0 = unlimited
	tokens float64 // may go negative: the debt is slept off
	last   time.Time
}

func newBucket(bytesPerSec int64) *bucket {
	return &bucket{rate: float64(bytesPerSec), tokens: float64(bytesPerSec) / 4, last: time.Now()}
}

func (b *bucket) setRate(bytesPerSec int64) {
	b.mu.Lock()
	b.rate = float64(bytesPerSec)
	if b.tokens > b.rate/4 {
		b.tokens = b.rate / 4
	}
	b.mu.Unlock()
}

// take charges n bytes and sleeps until the budget allows them. The burst is
// a quarter of a second's worth, so a seek is answered at once.
func (b *bucket) take(n int) {
	b.mu.Lock()
	if b.rate <= 0 {
		b.mu.Unlock()
		return
	}
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	b.last = now
	if max := b.rate / 4; b.tokens > max {
		b.tokens = max
	}
	b.tokens -= float64(n)
	var wait time.Duration
	if b.tokens < 0 {
		wait = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}
