package main

import (
	"net"
	"testing"

	"github.com/hashicorp/yamux"
)

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
