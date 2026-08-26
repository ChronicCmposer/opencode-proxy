package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// trackedCloser is a stand-in for the net.Conn raceCtx's stream-opening work
// returns: it records whether Close was called.
type trackedCloser struct{ closed chan struct{} }

func (c *trackedCloser) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// TestRaceCtxClosesLateResultOnCancel proves the fix for the stream leak: when
// ctx is cancelled first, the value work produces afterward must be closed
// rather than silently dropped, or every cancelled request leaks a yamux
// stream.
func TestRaceCtxClosesLateResultOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	tc := &trackedCloser{closed: make(chan struct{})}

	// work blocks until we release it, so cancellation is guaranteed to win.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := raceCtx(ctx, func() (*trackedCloser, error) {
		<-release
		return tc, nil
	})
	if err != context.Canceled {
		t.Fatalf("raceCtx err = %v, want context.Canceled", err)
	}

	close(release) // let the abandoned work finish and produce its value
	select {
	case <-tc.closed:
	case <-time.After(time.Second):
		t.Fatal("raceCtx leaked the late result: Close was never called")
	}
}

func TestYamuxRoleString(t *testing.T) {
	tests := []struct {
		role YamuxRole
		want string
	}{
		{YamuxRoleClient, "client"},
		{YamuxRoleServer, "server"},
	}
	for _, tt := range tests {
		if got := tt.role.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.role, got, tt.want)
		}
	}
}

// TestNewYamuxSessionRoleSetsStreamIDParity proves role actually selects
// yamux.Client vs yamux.Server rather than just picking one arbitrarily:
// yamux gives client-opened streams odd IDs and server-opened streams even
// ones, a real protocol-level difference the peer's own role has no bearing
// on (only the initiating side's nextStreamID counter is role-dependent), so
// this pairs each role against a throwaway peer whose only job is to keep its
// read/send loops running.
func TestNewYamuxSessionRoleSetsStreamIDParity(t *testing.T) {
	tests := []struct {
		role    YamuxRole
		wantOdd bool
	}{
		{YamuxRoleClient, true},
		{YamuxRoleServer, false},
	}
	for _, tt := range tests {
		t.Run(tt.role.String(), func(t *testing.T) {
			a, b := net.Pipe()
			t.Cleanup(func() { a.Close() })
			t.Cleanup(func() { b.Close() })

			sess, err := NewYamuxSession(a, yamux.DefaultConfig(), tt.role)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sess.Close() })

			peer, err := yamux.Client(b, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { peer.Close() })

			stream, err := sess.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { stream.Close() })

			if odd := stream.StreamID()%2 == 1; odd != tt.wantOdd {
				t.Errorf("StreamID() = %d, want odd=%v for role %v", stream.StreamID(), tt.wantOdd, tt.role)
			}
		})
	}
}
