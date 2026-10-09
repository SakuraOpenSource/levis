package handler

import (
	"sync"
	"time"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// runtimeRevoker serves logout revocation across the install boundary.
//
// Why it exists (P-AUTH-4): the process starts uninstalled with no database
// handle, and the database appears only after the install flow calls
// Activate. A revocation store chosen once at boot would either be
// process-local forever (revocations lost on restart) or break the
// not-yet-installed boot. This wrapper starts with the process-local list and
// swaps to the database-backed PersistentRevocationList the first time the
// runtime exposes a DB. Revocations recorded pre-upgrade are replayed into
// the durable store so the upgrade itself cannot resurrect a logged-out
// token.
type runtimeRevoker struct {
	rt     *runtime.Runtime
	mu     sync.RWMutex
	memory *auth.RevocationList
	db     *auth.PersistentRevocationList
}

// newRuntimeRevoker builds the install-aware revoker.
func newRuntimeRevoker(rt *runtime.Runtime) *runtimeRevoker {
	return &runtimeRevoker{rt: rt, memory: auth.NewRevocationList()}
}

// current returns the store that should serve the call, upgrading to the
// persistent store on first sight of a database.
func (r *runtimeRevoker) current() auth.SessionRevoker {
	r.mu.RLock()
	if r.db != nil {
		defer r.mu.RUnlock()
		return r.db
	}
	r.mu.RUnlock()

	// Try to upgrade: a DB handle appearing means the install completed.
	if handle := r.rt.DB(); handle != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.db == nil {
			r.db = auth.NewPersistentRevocationList(handle)
			// Replay process-local revocations so the swap loses nothing.
			if pending := r.memory.Snapshot(); len(pending) > 0 {
				for jti, expiry := range pending {
					r.db.Revoke(jti, expiry)
				}
			}
			r.memory.Close()
		}
		return r.db
	}
	return r.memory
}

func (r *runtimeRevoker) Revoke(jti string, expiry time.Time) error {
	return r.current().Revoke(jti, expiry)
}

func (r *runtimeRevoker) IsRevoked(jti string) bool { return r.current().IsRevoked(jti) }

func (r *runtimeRevoker) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		r.db.Close()
		r.db = nil
	}
	r.memory.Close()
}
