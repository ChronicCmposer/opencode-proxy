package main

import (
	"net"
	"testing"

	"github.com/hashicorp/yamux"
)

// newTestSession builds a *yamux.Session without a live tunnel: yamux frames
// its own connection state, so a session can exist perfectly well over one
// end of a net.Pipe with nothing driving the other end.
func newTestSession(t *testing.T) *yamux.Session {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { b.Close() })
	sess, err := yamux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestSessionRegistrySetReplacesAndClosesOld(t *testing.T) {
	var reg SessionRegistry

	first := newTestSession(t)
	reg.Set(first)
	if got := reg.Get(); got != first {
		t.Fatalf("Get() = %v, want first session", got)
	}

	second := newTestSession(t)
	reg.Set(second)
	if got := reg.Get(); got != second {
		t.Fatalf("Get() = %v, want second session", got)
	}
	if !first.IsClosed() {
		t.Fatal("Set() should close the session it replaced")
	}
}

func TestSessionRegistryClearOnlyClobbersMatchingSession(t *testing.T) {
	var reg SessionRegistry

	first := newTestSession(t)
	reg.Set(first)
	second := newTestSession(t)
	reg.Set(second)

	reg.Clear(first)
	if got := reg.Get(); got != second {
		t.Fatalf("Clear() with a stale session clobbered the current one: got %v, want second", got)
	}

	reg.Clear(second)
	if got := reg.Get(); got != nil {
		t.Fatalf("Get() after Clear(current) = %v, want nil", got)
	}
}

func TestSessionRegistryGetReturnsNilForClosedSession(t *testing.T) {
	var reg SessionRegistry

	sess := newTestSession(t)
	reg.Set(sess)
	sess.Close()

	if got := reg.Get(); got != nil {
		t.Fatalf("Get() = %v, want nil for a closed session", got)
	}
}
