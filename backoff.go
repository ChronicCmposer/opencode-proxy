package main

import (
	"math/rand"
	"time"
)

type Backoff struct {
	min, max time.Duration
	attempt  int
}

func NewBackoff(min, max time.Duration) *Backoff {
	return &Backoff{min: min, max: max}
}

// Next never returns more than b.max: jitter is added within the cap rather
// than on top of it, so callers can rely on b.max as a true upper bound.
func (b *Backoff) Next() time.Duration {
	base := b.nextBase()
	// rand.Int63n panics on n<=0, and base/5 underflows to 0 for sub-5ns
	// bases: skip jitter rather than risk it on an extreme configured value.
	jitterMax := int64(base) / 5
	if jitterMax <= 0 {
		return base
	}
	jitter := time.Duration(rand.Int63n(jitterMax))
	return min(base+jitter, b.max)
}

// nextBase returns the un-jittered delay for the current attempt, capped at
// b.max, and advances the attempt counter as long as it hasn't already
// reached the cap. Once min<<attempt reaches or overflows past b.max
// (overflow wraps a Duration's underlying int64 negative, so d<=0 also means
// "past max"), attempt stops advancing: shifting further would only ever
// yield b.max again, so there's no reason to keep counting.
func (b *Backoff) nextBase() time.Duration {
	d := b.min << b.attempt
	if d <= 0 || d > b.max {
		return b.max
	}
	b.attempt++
	return d
}

func (b *Backoff) Reset() {
	b.attempt = 0
}
