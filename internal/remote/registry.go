package remote

import (
	"sync"

	"github.com/hashicorp/yamux"
)

type sessionRegistry struct {
	mu   sync.Mutex
	sess *yamux.Session
}

func (r *sessionRegistry) set(sess *yamux.Session) {
	r.mu.Lock()
	old := r.sess
	r.sess = sess
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

func (r *sessionRegistry) get() *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil || r.sess.IsClosed() {
		return nil
	}
	return r.sess
}

func (r *sessionRegistry) clear(sess *yamux.Session) {
	r.mu.Lock()
	if r.sess == sess { // don't clobber a newer session set() already installed
		r.sess = nil
	}
	r.mu.Unlock()
}
