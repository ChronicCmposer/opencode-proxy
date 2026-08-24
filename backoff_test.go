package main

import (
	"testing"
	"time"
)

// Short bounds keep the 1:30 floor-to-cap ratio of the defaults, so the growth
// and capping behaviour under test is the same one production sees.
const (
	testBackoffMin = time.Millisecond
	testBackoffMax = 30 * time.Millisecond
)

func TestNewBackoffUsesInjectedBounds(t *testing.T) {
	b := NewBackoff(testBackoffMin, testBackoffMax)
	if b.min != testBackoffMin || b.max != testBackoffMax {
		t.Fatalf("bounds = (%v, %v), want (%v, %v)", b.min, b.max, testBackoffMin, testBackoffMax)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := NewBackoff(testBackoffMin, testBackoffMax)
	prevMin := b.min
	for i := 0; i < 10; i++ {
		d := b.Next()
		if d < prevMin {
			t.Fatalf("attempt %d: delay %v below floor %v", i, d, prevMin)
		}
		if d > b.max+b.max/5 { // allow for jitter headroom
			t.Fatalf("attempt %d: delay %v exceeds cap %v (+jitter)", i, d, b.max)
		}
	}
}

func TestBackoffResetReturnsToFloor(t *testing.T) {
	b := NewBackoff(testBackoffMin, testBackoffMax)
	for i := 0; i < 5; i++ {
		b.Next()
	}
	b.Reset()
	d := b.Next()
	if d < b.min || d > b.min+b.min/5 {
		t.Fatalf("delay after reset = %v, want near floor %v", d, b.min)
	}
}
