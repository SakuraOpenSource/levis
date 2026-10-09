package auth

import (
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// PersistentRevocationList is the database-backed session revocation store.
//
// Why persistent (P-AUTH-4): JWTs stay valid until expiry no matter what the
// server does, so logout has to be recorded as server-side state. The older
// in-memory list lost every revocation on restart and could not serve a second
// replica — logging out would become reversible exactly when an operator
// restarts the process. Rows outlive their token's natural expiry only until
// the next sweep. An in-process cache in front of the table (see lookup)
// keeps the hot path free of a DB query per request while the table remains
// the source of truth.
type PersistentRevocationList struct {
	db   *gorm.DB
	mu   sync.RWMutex
	hot  map[string]time.Time // jti -> expiry, process-local acceleration
	once sync.Once
	stop chan struct{}
}

// NewPersistentRevocationList opens the durable revocation store and starts
// the background sweep that drops rows past their natural expiry.
func NewPersistentRevocationList(db *gorm.DB) *PersistentRevocationList {
	r := &PersistentRevocationList{
		db:   db,
		hot:  make(map[string]time.Time),
		stop: make(chan struct{}),
	}
	r.warmCache()
	go r.sweepLoop()
	return r
}

// warmCache loads live rows so the first requests after boot do not pay a DB
// roundtrip per lookup (correctness never depends on the cache: misses fall
// through to the table).
func (r *PersistentRevocationList) warmCache() {
	if r.db == nil {
		return
	}
	var rows []model.RevokedToken
	if err := r.db.Where("expires_at > ?", time.Now()).Find(&rows).Error; err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		r.hot[row.JTI] = row.ExpiresAt
	}
}

// Close stops the background sweep. Safe to call multiple times.
func (r *PersistentRevocationList) Close() {
	r.once.Do(func() { close(r.stop) })
}

// Revoke records jti as revoked until expiry. Re-revoking keeps the LONGEST
// window: a later expiry must not shorten an existing revocation, and the
// in-memory acceleration is updated in the same step.
func (r *PersistentRevocationList) Revoke(jti string, expiry time.Time) error {
	if jti == "" {
		return nil
	}
	if r.db == nil {
		return errors.New("revocation storage unavailable")
	}
	expiry = expiry.UTC()
	// GORM renders conflict handling for each supported dialect; a conditional update never shortens revocation.
	if err := r.db.Transaction(func(tx *gorm.DB) error {
		row := model.RevokedToken{JTI: jti, ExpiresAt: expiry}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		return tx.Model(&model.RevokedToken{}).Where("jti = ? AND expires_at < ?", jti, expiry).
			UpdateColumn("expires_at", expiry).Error
	}); err != nil {
		return err
	}
	r.mu.Lock()
	if until, ok := r.hot[jti]; !ok || expiry.After(until) {
		r.hot[jti] = expiry
	}
	r.mu.Unlock()
	return nil
}

// IsRevoked reports whether jti is revoked, consulting the process-local
// cache first and the table on miss (another replica may have revoked it).
func (r *PersistentRevocationList) IsRevoked(jti string) bool {
	if jti == "" || r.db == nil {
		return false
	}
	// Compare in UTC: rows are always written UTC; a local-time comparison on a
	// UTC+offset host would let revoked sessions back in hours before expiry.
	now := time.Now().UTC()
	r.mu.RLock()
	until, ok := r.hot[jti]
	r.mu.RUnlock()
	if ok && until.After(now) {
		return true
	}
	var row model.RevokedToken
	err := r.db.Where("jti = ? AND expires_at > ?", jti, now).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false
		}
		// Fail closed on storage errors: a read failure must not resurrect a revoked session.
		return true
	}
	r.mu.Lock()
	r.hot[jti] = row.ExpiresAt
	r.mu.Unlock()
	return true
}

// CleanupExpired removes rows whose token has naturally expired; kept public
// so tests can drive it deterministically.
func (r *PersistentRevocationList) CleanupExpired(now time.Time) {
	if r.db == nil {
		return
	}
	now = now.UTC()
	if err := r.db.Where("expires_at <= ?", now).Delete(&model.RevokedToken{}).Error; err != nil {
		return
	}
	r.mu.Lock()
	for jti, until := range r.hot {
		if !until.After(now) {
			delete(r.hot, jti)
		}
	}
	r.mu.Unlock()
}

// sweepLoop periodically drops expired rows so the table cannot grow forever.
func (r *PersistentRevocationList) sweepLoop() {
	ticker := time.NewTicker(revocationSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.CleanupExpired(time.Now())
		}
	}
}
