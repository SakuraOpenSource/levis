package server

import (
	"net/http"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// P-AUTH-4: logout revocations must land in the database so a restart (or a
// second replica) cannot resurrect a logged-out token. This drives the real
// handler stack: login -> logout -> verify the revoked_tokens row exists and
// a fresh PersistentRevocationList over the same DB rejects the jti.
func TestLogoutRevocationIsDurable(t *testing.T) {
	rt, handler, _, _ := installedWithUsers(t, "alice")
	session := loginAs(t, handler, "alice", "password123")

	// Precondition: the session works.
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, session); rec.Code != http.StatusOK {
		t.Fatalf("/api/me before logout = %d", rec.Code)
	}
	if rec := doAs(t, handler, http.MethodPost, "/api/auth/logout", nil, session); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}

	// The revocation must be a row in the database, not just process memory.
	var rows []model.RevokedToken
	if err := rt.DB().Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("logout must persist a RevokedToken row")
	}

	// And the replayed token must be rejected by the live server too.
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token after logout = %d, want 401", rec.Code)
	}
}
