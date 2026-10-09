package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
)

// This file guards the lifecycle patrol against stale-snapshot enforcement
// (audit MONEY-10): a service renewed between the patrol's read and its
// destructive RPC must never be suspended or terminated by the old snapshot,
// and the destructive RPC must be gated on a claim of the exact expiry /
// suspended_at snapshot read at list time.

// hookHost wraps fakeHost and calls onManage synchronously before recording
// the action — the test performs its "user renews right now" mutation inside
// the patrol's RPC window.
type hookHost struct {
	fakeHost
	onManage func(action pb.HostAction)
}

func (h *hookHost) ManageHost(ctx context.Context, pluginID string, req *pb.ManageHostRequest) (*pb.ManageHostReply, error) {
	if h.onManage != nil {
		h.onManage(req.GetAction())
	}
	return h.fakeHost.ManageHost(ctx, pluginID, req)
}

// renewServiceDuringRPC performs a wallet renewal for svcID inside the
// patrol's SUSPEND/TERMINATE RPC — exactly the race window the audit found:
// the patrol already snapshotted the expired list, but the money landed after.
func renewServiceDuringRPC(db *gorm.DB, svcID uint, to time.Time) {
	// Simulate the committed renewal: status stays/becomes active with a
	// future expiry (mirrors BillingService.applyRenewalTx semantics).
	_ = db.Model(&model.Service{}).Where("id = ?", svcID).Updates(map[string]any{
		"status":         model.ServiceActive,
		"suspend_reason": "",
		"suspended_at":   nil,
		"expires_at":     to,
		"next_due_at":    to,
	})
}

// TestLifecycleSuspendSkipsServiceRenewedAfterListRead: the user renews inside
// the patrol's SUSPEND RPC window; the local write of suspended must fail the
// expiry claim and the patrol must roll the upstream suspend back.
func TestLifecycleSuspendSkipsServiceRenewedAfterListRead(t *testing.T) {
	db := newTestDB(t)
	expired := time.Now().Add(-1 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceActive, &expired, 0, "host-m10a")

	var renewed atomic.Bool
	host := &hookHost{}
	host.onManage = func(action pb.HostAction) {
		if action == pb.HostAction_HOST_ACTION_SUSPEND && renewed.CompareAndSwap(false, true) {
			renewServiceDuringRPC(db, svc.ID, time.Now().Add(30*24*time.Hour))
		}
	}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("service renewed inside the patrol window must stay active, got %s (reason=%s)", got.Status, got.SuspendReason)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.After(time.Now()) {
		t.Fatal("renewed expiry must survive the patrol round")
	}
	if host.hasAction("host-m10a", "HOST_ACTION_SUSPEND") && !host.hasAction("host-m10a", "HOST_ACTION_UNSUSPEND") {
		t.Fatal("patrol that suspended a just-renewed service must UNSUSPEND it back")
	}
}

// TestLifecycleSuspendClaimFailsWhenExpiryMoved: the expiry CAS must reject
// the local suspended write when expiry changed since the list read, even if
// the row still says active (active -> active renewal).
func TestLifecycleSuspendClaimFailsWhenExpiryMoved(t *testing.T) {
	db := newTestDB(t)
	expired := time.Now().Add(-1 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceActive, &expired, 0, "host-m10c")

	host := &hookHost{}
	host.onManage = func(action pb.HostAction) {
		if action == pb.HostAction_HOST_ACTION_SUSPEND {
			// Renewal keeps status active but moves expiry (early renewal
			// while the patrol was already in flight).
			future := time.Now().Add(15 * 24 * time.Hour)
			_ = db.Model(&model.Service{}).Where("id = ?", svc.ID).Update("expires_at", future)
		}
	}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("active->active renewal must not be overwritten to %s", got.Status)
	}
	if host.hasAction("host-m10c", "HOST_ACTION_SUSPEND") && !host.hasAction("host-m10c", "HOST_ACTION_UNSUSPEND") {
		t.Fatal("suspend of an active->active renewed service must be rolled back")
	}
}

// TestLifecycleTerminateSkipsRenewedService: TERMINATE must be gated on the
// exact suspended_at/expiry snapshot; a service renewed inside the purge RPC
// window must never be terminated or purged.
func TestLifecycleTerminateSkipsRenewedService(t *testing.T) {
	db := newTestDB(t)
	suspendedAt := time.Now().Add(-100 * time.Hour)
	expired := time.Now().Add(-100 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &expired, 0, "host-m10b")
	markAutoSuspendedAt(t, db, svc, "expired", suspendedAt)

	host := &hookHost{}
	terminated := atomic.Bool{}
	host.onManage = func(action pb.HostAction) {
		if action == pb.HostAction_HOST_ACTION_TERMINATE && terminated.CompareAndSwap(false, true) {
			renewServiceDuringRPC(db, svc.ID, time.Now().Add(30*24*time.Hour))
		}
	}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status == model.ServiceTerminated {
		t.Fatal("a service renewed inside the terminate window must never be terminated")
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("renewed service must remain active, got %s", got.Status)
	}
}

// TestLifecycleTerminateReevaluatesAfterRPCFailure: when the TERMINATE RPC
// fails, the patrol must re-read the service before writing terminated; a
// renewal that landed during the RPC must stop the local write.
func TestLifecycleTerminateReevaluatesAfterRPCFailure(t *testing.T) {
	db := newTestDB(t)
	suspendedAt := time.Now().Add(-100 * time.Hour)
	expired := time.Now().Add(-100 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &expired, 0, "host-m10d")
	markAutoSuspendedAt(t, db, svc, "expired", suspendedAt)

	host := &hookHost{}
	host.manageFail = map[string]error{"host-m10d": errFailOnce{}}
	host.onManage = func(action pb.HostAction) {
		if action == pb.HostAction_HOST_ACTION_TERMINATE {
			renewServiceDuringRPC(db, svc.ID, time.Now().Add(30*24*time.Hour))
		}
	}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status == model.ServiceTerminated {
		t.Fatal("termination must not be written from a stale snapshot after an RPC failure")
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("renewed service must stay active after failed terminate, got %s", got.Status)
	}
}

type errFailOnce struct{}

func (errFailOnce) Error() string { return "rpc failed once" }
