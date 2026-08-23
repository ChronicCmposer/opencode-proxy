package remote

import (
	"sync"

	"github.com/hashicorp/yamux"
)

// sessionRegistry holds at most one active tunnel session. A new tunnel
// connection (e.g. after the Mac sleeps and wakes) replaces and closes
// whatever was there before, so the remote never has to reconcile two
// concurrent tunnels to the same home.
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
	if r.sess == sess {
		r.sess = nil
	}
	r.mu.Unlock()
}
