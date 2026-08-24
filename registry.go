package main

import (
	"sync"

	"github.com/hashicorp/yamux"
)

type SessionRegistry struct {
	mu   sync.Mutex
	sess *yamux.Session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{}
}

func (r *SessionRegistry) Set(sess *yamux.Session) {
	r.mu.Lock()
	old := r.sess
	r.sess = sess
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// Get returns the current session, or nil if none is connected. The
// returned session can still be closed by a concurrent Set/Clear right
// after Get returns it — callers don't need to guard against that: yamux's
// Open (and CloseChan) on an already-closed session simply fails/fires
// immediately, which the reverse proxy's normal dial-error handling already
// covers.
func (r *SessionRegistry) Get() *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil || r.sess.IsClosed() {
		return nil
	}
	return r.sess
}

func (r *SessionRegistry) Clear(sess *yamux.Session) {
	r.mu.Lock()
	if r.sess == sess { // don't clobber a newer session Set() already installed
		r.sess = nil
	}
	r.mu.Unlock()
}
