package auth

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// newRevTestDB opens a shared-cache in-memory DB with all models migrated.
func newRevTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatal(err)
	}
	return db
}

// P-AUTH-4: revocation must survive a process restart. A new RevocationList
// over the same database (simulating a fresh process) must still reject the
// jti revoked before the restart.
func TestRevocationSurvivesNewListInstance(t *testing.T) {
	db := newRevTestDB(t)
	first := NewPersistentRevocationList(db)
	expiry := time.Now().Add(time.Hour)
	first.Revoke("jti-1", expiry)
	first.Close()

	// Simulated restart / other replica: brand-new list on the same DB.
	second := NewPersistentRevocationList(db)
	defer second.Close()
	if !second.IsRevoked("jti-1") {
		t.Fatal("revocation must survive a new RevocationList on the same DB")
	}
	if second.IsRevoked("jti-other") {
		t.Fatal("unrelated jti must not be revoked")
	}
}

// P-AUTH-4: cleanup must drop rows whose natural expiry has passed.
func TestRevocationCleanupDropsExpiredRows(t *testing.T) {
	db := newRevTestDB(t)
	r := NewPersistentRevocationList(db)
	defer r.Close()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	r.Revoke("expired-jti", past)
	r.Revoke("live-jti", future)

	r.CleanupExpired(time.Now())

	if r.IsRevoked("expired-jti") {
		t.Fatal("expired revocation must be cleaned up")
	}
	if !r.IsRevoked("live-jti") {
		t.Fatal("live revocation must survive cleanup")
	}
}

// Revoke keeps the LONGEST window (same semantics as the in-memory list).
func TestRevocationKeepsLongestExpiry(t *testing.T) {
	db := newRevTestDB(t)
	r := NewPersistentRevocationList(db)
	defer r.Close()
	soon := time.Now().Add(10 * time.Minute)
	later := time.Now().Add(2 * time.Hour)
	r.Revoke("jti", soon)
	r.Revoke("jti", later)
	r.CleanupExpired(time.Now().Add(30 * time.Minute))
	if !r.IsRevoked("jti") {
		t.Fatal("revocation must keep the longest expiry, not the shortest")
	}
}
