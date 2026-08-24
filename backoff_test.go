package main

import "testing"

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := NewBackoff()
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
	b := NewBackoff()
	for i := 0; i < 5; i++ {
		b.Next()
	}
	b.Reset()
	d := b.Next()
	if d < b.min || d > b.min+b.min/5 {
		t.Fatalf("delay after reset = %v, want near floor %v", d, b.min)
	}
}
