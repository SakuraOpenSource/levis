package auth

import (
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func TestRevocationReadFailureRejectsUnknownSessions(t *testing.T) {
	db := newRevTestDB(t)
	revoker := NewPersistentRevocationList(db)
	t.Cleanup(revoker.Close)
	if err := db.Migrator().DropTable(&model.RevokedToken{}); err != nil {
		t.Fatal(err)
	}
	if !revoker.IsRevoked("unknown-session") {
		t.Fatal("unavailable revocation storage must not authorize an unknown session")
	}
}
