package tunnel

import (
	"math/rand"
	"time"
)

// Backoff computes reconnect delays: 1s, 2s, 4s, ... capped at 30s, with up to
// 20% jitter so a fleet of one client doesn't matter, but many clients
// wouldn't stampede a shared remote.
type Backoff struct {
	min, max time.Duration
	attempt  int
}

func NewBackoff() *Backoff {
	return &Backoff{min: time.Second, max: 30 * time.Second}
}

// Next returns the delay before the next attempt and advances internal state.
func (b *Backoff) Next() time.Duration {
	d := b.min << b.attempt
	if d <= 0 || d > b.max { // overflow or cap
		d = b.max
	} else {
		b.attempt++
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 5)) // up to 20%
	return d + jitter
}

// Reset clears attempt count after a successful connection.
func (b *Backoff) Reset() {
	b.attempt = 0
}
