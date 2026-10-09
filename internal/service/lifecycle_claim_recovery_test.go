package service

import (
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// A crash between the claim and the terminal write used to leave
// suspend_reason stuck on the transient markers forever, silently removing the
// service from both the expiry and terminate lists. Run must self-heal them.
func TestLifecycleRunRecoversCrashedClaimMarkers(t *testing.T) {
	db := newTestDB(t)
	svc := &model.Service{
		UserID: 1, ProductID: 1, Status: model.ServiceActive,
		ExpiresAt:     ptrTime(time.Now().Add(-48 * time.Hour)),
		SuspendReason: suspendReasonEnforcing,
	}
	if err := db.Create(svc).Error; err != nil {
		t.Fatal(err)
	}
	dead := &model.Service{
		UserID: 2, ProductID: 1, Status: model.ServiceSuspended,
		ExpiresAt: ptrTime(time.Now().Add(-100 * time.Hour)), SuspendedAt: ptrTime(time.Now().Add(-24 * time.Hour)),
		SuspendReason: suspendReasonTerminating,
	}
	if err := db.Create(dead).Error; err != nil {
		t.Fatal(err)
	}

	s := NewLifecycleService(db, nil)
	s.Run(t.Context())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.SuspendReason == suspendReasonEnforcing {
		t.Fatal("a crashed enforcing marker must be reset so the next round can enforce again")
	}
	var got2 model.Service
	if err := db.First(&got2, dead.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got2.SuspendReason == suspendReasonTerminating {
		t.Fatal("a crashed terminating marker must return to expired so the purge list sees the row")
	}
}
