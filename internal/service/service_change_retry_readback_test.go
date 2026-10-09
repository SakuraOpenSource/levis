package service

import (
	"context"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func mustFutureExpiry() time.Time { return time.Now().UTC().AddDate(0, 1, 0) }

// MONEY-09: a DB read-back failure after a reservation must NOT release the
// service lease or refund the wallet — the remote mutation may already be in
// flight, so the reservation must stay durable for observation-based retry.
func TestRetryDBReadBackFailureRetainsLease(t *testing.T) {
	fx := newChangeFenceFixture(t, "readback-fail", mustFutureExpiry(), 1000, 2000)
	s := NewServiceChangeService(fx.db, fx.fake)
	// Reserve + attempt apply; the DB write that would finish the change is
	// rejected by the trigger, emulating a post-commit read-back failure.
	if err := fx.db.Exec("CREATE TRIGGER reject_finish BEFORE UPDATE OF product_id ON services BEGIN SELECT RAISE(FAIL, 'injected finish failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	_, err := s.Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID})
	if err == nil {
		t.Fatal("DB failure must be surfaced, not hidden")
	}
	var row model.ServiceChange
	if err := fx.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status == "applied" || row.Status == "failed" {
		t.Fatalf("uncertain outcome must stay recoverable, got %s", row.Status)
	}
	var svc model.Service
	if err := fx.db.First(&svc, fx.svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if svc.ChangePendingID == nil || *svc.ChangePendingID != row.ID {
		t.Fatal("a failed read-back must not release the service lease")
	}
	if got := balanceOf(t, fx.db, fx.user.ID); got >= 100000 {
		t.Fatalf("reservation debit must be retained for reconciliation, balance %d", got)
	}
	// Dropping the trigger lets observation-based retry finish without a
	// second remote mutation.
	if err := fx.db.Exec("DROP TRIGGER reject_finish").Error; err != nil {
		t.Fatal(err)
	}
	out, err := s.Retry(context.Background(), fx.user.ID, fx.svc.ID, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Change.Status != "applied" || fx.fake.calls != 1 {
		t.Fatalf("retry must reconcile by observation, calls=%d status=%s", fx.fake.calls, out.Change.Status)
	}
}

// MONEY-09: an explicit protocol rejection (definitive, pre-mutation) is the
// only path that refunds and releases; transport/DB failures are not.
func TestRetryDefinitiveRejectionRefunds(t *testing.T) {
	fx := newChangeFenceFixture(t, "definitive-reject", mustFutureExpiry(), 1000, 2000)
	fx.fake.reject = true
	s := NewServiceChangeService(fx.db, fx.fake)
	out, err := s.Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Change.Status != "failed" || balanceOf(t, fx.db, fx.user.ID) != 100000 {
		t.Fatalf("definitive rejection must refund once: %s %d", out.Change.Status, balanceOf(t, fx.db, fx.user.ID))
	}
	var svc model.Service
	if err := fx.db.First(&svc, fx.svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if svc.ChangePendingID != nil {
		t.Fatal("definitive rejection must release the lease")
	}
	_ = status.Error(codes.InvalidArgument, "shape only")
	_ = codes.OK
}
