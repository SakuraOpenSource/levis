package server

import (
	"net/http"
	"testing"
)

func TestLogoutDoesNotClaimSuccessWhenDurableWriteFails(t *testing.T) {
	rt, handler, _, _ := installedWithUsers(t, "logout-write-failure")
	session := loginAs(t, handler, "logout-write-failure", "password123")
	// Inject a database write failure in this disposable SQLite fixture without blocking the preceding auth lookup.
	if err := rt.DB().Exec(`CREATE TRIGGER audit_reject_revocation BEFORE INSERT ON revoked_tokens BEGIN SELECT RAISE(ABORT, 'injected revocation write failure'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	response := doAs(t, handler, http.MethodPost, "/api/auth/logout", nil, session)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("logout must report the failed durable write, got %d", response.Code)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.MaxAge < 0 {
			t.Fatal("logout cannot clear login cookies and imply successful revocation after a failed write")
		}
	}
}
