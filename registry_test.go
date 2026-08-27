package main

import (
	"net"
	"os"
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
	reg := NewSessionRegistry()

	first := newTestSession(t)
	if replacedLive := reg.Set(first, nil); replacedLive {
		t.Fatal("Set() into an empty registry should not report replacing a live session")
	}
	if got := reg.Get(); got != first {
		t.Fatalf("Get() = %v, want first session", got)
	}

	// Last-writer-wins: a reconnecting home reclaims the slot immediately, and
	// the replaced session (still live here) is closed and reported.
	second := newTestSession(t)
	if replacedLive := reg.Set(second, nil); !replacedLive {
		t.Fatal("Set() replacing a still-live session should report replacedLive=true")
	}
	if got := reg.Get(); got != second {
		t.Fatalf("Get() = %v, want second session", got)
	}
	if !first.IsClosed() {
		t.Fatal("Set() should close the session it replaced")
	}
}

func TestSessionRegistrySetReplacingClosedIsNotLive(t *testing.T) {
	reg := NewSessionRegistry()

	first := newTestSession(t)
	reg.Set(first, nil)
	first.Close() // a dropped connection: yamux marks the session closed

	second := newTestSession(t)
	if replacedLive := reg.Set(second, nil); replacedLive {
		t.Fatal("replacing an already-closed session should not report replacedLive=true")
	}
	if got := reg.Get(); got != second {
		t.Fatalf("Get() = %v, want second session", got)
	}
}

func TestSessionRegistryClearOnlyClobbersMatchingSession(t *testing.T) {
	reg := NewSessionRegistry()

	first := newTestSession(t)
	reg.Set(first, nil)
	second := newTestSession(t)
	reg.Set(second, nil)

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
	reg := NewSessionRegistry()

	sess := newTestSession(t)
	reg.Set(sess, nil)
	sess.Close()

	if got := reg.Get(); got != nil {
		t.Fatalf("Get() = %v, want nil for a closed session", got)
	}
}

func TestSessionRegistryCloseCurrentIfRevoked(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	tunnelLeaf := ca.issueTunnel(t, "home-mac")
	cert := certFromLeaf(t, tunnelLeaf)

	// nil revocation (feature off) never closes anything.
	reg := NewSessionRegistry()
	sess := newTestSession(t)
	reg.Set(sess, cert)
	if reg.CloseCurrentIfRevoked(nil) {
		t.Fatal("nil revocation should never close the tunnel")
	}

	// A revocation list that does not name this serial leaves it alone.
	dir := t.TempDir()
	listPath := writePEM(t, dir, "revoked.txt", []byte("# empty\n"))
	revocation, err := NewRevocationList(listPath)
	if err != nil {
		t.Fatal(err)
	}
	if reg.CloseCurrentIfRevoked(revocation) {
		t.Fatal("an un-revoked tunnel cert should not be closed")
	}
	if sess.IsClosed() {
		t.Fatal("session should still be open")
	}

	// Once the tunnel's serial is listed, the live session is torn down —
	// IsRevoked reloads the file, so no restart is needed.
	if err := os.WriteFile(listPath, []byte("serial="+serialOf(cert)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !reg.CloseCurrentIfRevoked(revocation) {
		t.Fatal("a revoked tunnel cert should close the live session")
	}
	if !sess.IsClosed() {
		t.Fatal("session should be closed after revocation")
	}
}
