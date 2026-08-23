package main

import (
	"sync"

	"github.com/hashicorp/yamux"
)

type SessionRegistry struct {
	mu   sync.Mutex
	sess *yamux.Session
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
