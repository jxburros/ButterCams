package worker

import (
	"math/rand"
	"time"
)

// Reconnect timing (spec 6.1).
const (
	// StallTimeout is how long a camera may send no video before it is
	// considered stalled.
	StallTimeout = 10 * time.Second
	// ConnectTimeout bounds RTSP DESCRIBE/SETUP/PLAY.
	ConnectTimeout = 10 * time.Second
	// ErrorRetry is the retry interval for configuration problems such as a
	// wrong password.
	ErrorRetry = 60 * time.Second
	// StableAfter is how long a recording must run before the backoff resets.
	StableAfter = 5 * time.Minute
)

var backoffSteps = []time.Duration{
	2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second,
}

// Backoff produces the reconnect delays 2, 4, 8, 16, 32, 60, 60 … seconds
// with ±20% jitter.
type Backoff struct {
	step   int
	jitter func() float64 // returns a value in [0,1)
}

// NewBackoff returns a Backoff with random jitter.
func NewBackoff() *Backoff { return &Backoff{jitter: rand.Float64} }

// Base returns the next delay without jitter and advances.
func (b *Backoff) base() time.Duration {
	d := backoffSteps[len(backoffSteps)-1]
	if b.step < len(backoffSteps) {
		d = backoffSteps[b.step]
	}
	b.step++
	return d
}

// Next returns the next delay.
func (b *Backoff) Next() time.Duration {
	d := b.base()
	j := b.jitter
	if j == nil {
		j = rand.Float64
	}
	factor := 0.8 + 0.4*j() // 0.8 … 1.2
	return time.Duration(float64(d) * factor)
}

// Reset returns to the first step. The camera calls it after StableAfter of
// uninterrupted recording.
func (b *Backoff) Reset() { b.step = 0 }

// Step returns how many delays have been handed out since the last reset.
func (b *Backoff) Step() int { return b.step }
