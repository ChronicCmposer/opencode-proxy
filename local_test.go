package main

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Next's jitter math skips itself for a base too small to divide meaningfully
// by 5 (see Next's comment); testBackoffMin never gets that small on its own,
// so this exercises it directly with a floor of 1ns.
func TestNextSkipsJitterForSubNanosecondBase(t *testing.T) {
	b := NewBackoff(1, time.Millisecond)
	if d := b.Next(); d != 1 {
		t.Fatalf("Next() = %v, want exactly the 1ns floor with no jitter added", d)
	}
}

// nextBase's overflow guard (d <= 0 || d > b.max) is only reachable once
// min<<attempt actually overflows time.Duration's underlying int64, which
// TestBackoffGrowsAndCaps's small bounds never approach. A floor of 1ns and a
// near-int64-max cap forces enough left-shifts to overflow within a bounded
// number of calls, so this can assert Next() never returns a non-positive or
// over-cap delay even once that happens.
func TestBackoffNeverReturnsNonPositiveOnOverflow(t *testing.T) {
	max := time.Duration(math.MaxInt64)
	b := NewBackoff(1, max)
	for i := 0; i < 128; i++ {
		d := b.Next()
		if d <= 0 {
			t.Fatalf("attempt %d: Next() = %v, want a positive delay", i, d)
		}
		if d > max {
			t.Fatalf("attempt %d: Next() = %v, want capped at %v", i, d, max)
		}
	}
}

func TestResetIfStableIgnoresFlappingSessions(t *testing.T) {
	b := NewBackoff(testBackoffMin, testBackoffMax)
	for i := 0; i < 5; i++ {
		b.Next()
	}
	grownAttempt := b.attempt

	b.ResetIfStable(testBackoffMin / 2)
	if b.attempt != grownAttempt {
		t.Fatalf("attempt = %d after a sub-floor uptime, want unchanged %d", b.attempt, grownAttempt)
	}

	b.ResetIfStable(testBackoffMin)
	if b.attempt != 0 {
		t.Fatalf("attempt = %d after an at-floor uptime, want reset to 0", b.attempt)
	}
}

// runLocalClientAndWait runs client in a goroutine, cancels it after letting
// it reach whatever state the caller is testing, and fails the test if Run
// doesn't return promptly — the only way to tell a genuine break from a
// break-turned-continue that keeps looping past cancellation.
func runLocalClientAndWait(t *testing.T, client *LocalClient, letRun time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	time.Sleep(letRun)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

// TestLocalClientRunStopsRetryingOnCancel exercises the break in Run's
// dial-failure branch: RemoteURL points at a closed port, so DialTunnel fails
// every attempt and Run sits in its backoff wait — the only thing that can
// end the loop is waitToRetry observing ctx cancelled.
func TestLocalClientRunStopsRetryingOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now, so every dial fails

	backoff := NewBackoff(time.Hour, time.Hour) // must not elapse before cancel does
	tunnelFactory := NewTunnelFactory(NewYamuxConfig(time.Minute, time.Minute), context.Background())
	client := NewLocalClient("ws://"+addr, nil, &http.Client{}, nil, tunnelFactory, backoff)

	runLocalClientAndWait(t, client, 50*time.Millisecond)
}

// TestLocalClientRunStopsOnCancelDuringSession exercises the break taken when
// ctx is cancelled while a session is active: a bare websocket server accepts
// the tunnel upgrade and then just holds the session open, so Run's
// server.Serve(sess) call blocks until cancellation closes it — the only way
// runTunnelSession can return "cancelled".
func TestLocalClientRunStopsOnCancelDuringSession(t *testing.T) {
	tunnelFactory := NewTunnelFactory(NewYamuxConfig(time.Minute, time.Minute), context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := tunnelFactory.AcceptTunnel(w, r)
		if err != nil {
			return
		}
		<-sess.CloseChan()
	}))
	t.Cleanup(srv.Close)
	remoteURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	backoff := NewBackoff(time.Hour, time.Hour)
	server := &http.Server{Handler: http.NotFoundHandler()} // safe to reuse — see run() in main.go
	client := NewLocalClient(remoteURL, nil, srv.Client(), server, tunnelFactory, backoff)

	runLocalClientAndWait(t, client, 100*time.Millisecond)
}
