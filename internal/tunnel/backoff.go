package tunnel

import (
	"math/rand"
	"time"
)

type Backoff struct {
	min, max time.Duration
	attempt  int
}

func NewBackoff() *Backoff {
	return &Backoff{min: time.Second, max: 30 * time.Second}
}

func (b *Backoff) Next() time.Duration {
	d := b.min << b.attempt
	if d <= 0 || d > b.max { // left-shift overflow yields <=0, not a panic
		d = b.max
	} else {
		b.attempt++
	}
	jitter := time.Duration(rand.Int63n(int64(d) / 5))
	return d + jitter
}

func (b *Backoff) Reset() {
	b.attempt = 0
}
