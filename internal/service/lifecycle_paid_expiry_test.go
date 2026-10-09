package service

import (
	"context"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func TestLifecycleCannotPurgeRenewedSuspendedEntitlement(t *testing.T) {
	db := newTestDB(t)
	past := time.Now().Add(-100 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &past, 0, "renewed-suspended")
	markAutoSuspendedAt(t, db, svc, "expired", past)
	future := time.Now().Add(30 * 24 * time.Hour)
	// Paid renewals can remain suspended until upstream reconciliation; expiry, not suspended_at, protects the entitlement.
	if err := db.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{"expires_at": future, "next_due_at": future}).Error; err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())
	if host.hasAction(svc.UpstreamHostID, "HOST_ACTION_TERMINATE") {
		t.Fatal("future paid expiry must block irreversible purge while reconciliation is pending")
	}
	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status == model.ServiceTerminated {
		t.Fatal("paid entitlement must not be marked terminated")
	}
}
