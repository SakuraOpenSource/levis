package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// This file guards the pending-intent invariants from the security audit:
// MONEY-01 (cancel/compensation must CAS-claim the intent before returning the
// wallet deduction) and MONEY-05 (one live payment path per order; late
// callbacks must be recorded, never silently acknowledged).

// seedPendingIntent creates a pending mixed payment with a wallet deduction
// already recorded, returning the intent.
func seedPendingIntent(t *testing.T, db *gorm.DB, userID, orderID uint, external, balance int64, ext string) *model.ExternalPayment {
	t.Helper()
	intent := model.ExternalPayment{
		PluginID: "epay", ExternalID: ext, UserID: userID,
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: orderID,
		AmountCents: external, BalanceCents: balance, Currency: "CNY",
		Subject: "mixed", Status: model.ExternalPaymentPending,
	}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if balance > 0 {
		wallet := NewWalletService(db)
		if _, err := wallet.adjustBalance(db, userID, -balance, model.TxPayment, "order", orderID, "deduct"); err != nil {
			t.Fatal(err)
		}
	}
	return &intent
}

// MONEY-01: two concurrent cancels of the same mixed intent must refund the
// wallet deduction exactly once.
func TestCancelConcurrentRefundsBalanceOnce(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "cancel-race", 3000)
	order := model.Order{OrderNo: "CR1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedPendingIntent(t, db, user.ID, order.ID, 2000, 1000, "cancel-race-1")
	before := balanceOf(t, db, user.ID) // 2000 after deduction

	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = payments.Cancel(user.ID, intent.ID)
		}(i)
	}
	wg.Wait()

	// Exactly one cancel must succeed with the CAS claim; the loser must
	// surface a conflict instead of refunding again.
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one concurrent cancel must win, got %d (errs=%v)", ok, errs)
	}
	after := balanceOf(t, db, user.ID)
	if after != 3000 {
		t.Fatalf("wallet must be refunded exactly once: before=%d after=%d", before, after)
	}
}

// MONEY-01: cancel must never overwrite a paid intent (e.g. the gateway
// finalized between the read and the write).
func TestCancelCannotOverwritePaidIntent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "cancel-paid", 3000)
	order := model.Order{OrderNo: "CP1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedPendingIntent(t, db, user.ID, order.ID, 2000, 1000, "cancel-paid-1")
	// The intent got paid after the user's cancel request was in flight.
	if err := db.Model(&model.ExternalPayment{}).Where("id = ?", intent.ID).
		Update("status", model.ExternalPaymentPaid).Error; err != nil {
		t.Fatal(err)
	}
	before := balanceOf(t, db, user.ID)

	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	if _, err := payments.Cancel(user.ID, intent.ID); err == nil {
		t.Fatal("canceling an already-paid intent must fail")
	}
	var got model.ExternalPayment
	if err := db.First(&got, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentPaid {
		t.Fatalf("paid status must survive the cancel, got %s", got.Status)
	}
	if balanceOf(t, db, user.ID) != before {
		t.Fatal("no wallet movement may happen for a rejected cancel")
	}
}

// fakeCreatePayPlugins lets tests inject a CreatePayment failure to exercise
// the compensation path of PaymentService.Create.
type fakeCreatePayPlugins struct {
	createErr error
	queries   int
}

// MONEY-01: creation failure compensation must not double-refund when the
// intent row already left pending (e.g. a concurrent finalize).
func TestCreateFailureCompensationClaimsIntent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "comp-race", 3000)
	order := model.Order{OrderNo: "COMP1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	// Simulate: intent created+deducted, channel creation failed, and before
	// the compensation ran, a late gateway callback finalized the intent.
	intent := seedPendingIntent(t, db, user.ID, order.ID, 2000, 1000, "comp-race-1")
	if err := db.Model(&model.ExternalPayment{}).Where("id = ?", intent.ID).
		Update("status", model.ExternalPaymentPaid).Error; err != nil {
		t.Fatal(err)
	}
	before := balanceOf(t, db, user.ID)

	// Run the compensation logic directly through the same helper the service
	// uses (claim-then-refund); it must see the claim fail and refund nothing.
	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	err := payments.compensateCreateFailure(&model.ExternalPayment{Base: model.Base{ID: intent.ID}}, user.ID)
	if err == nil {
		t.Fatal("compensation must fail when the intent is no longer pending")
	}
	if balanceOf(t, db, user.ID) != before {
		t.Fatalf("compensation must not move money without the claim: %d -> %d", before, balanceOf(t, db, user.ID))
	}
	var got model.ExternalPayment
	if err := db.First(&got, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentPaid {
		t.Fatalf("paid intent must not be overwritten by compensation, got %s", got.Status)
	}
}

// MONEY-05: a second live payment path for the same pending order must be
// rejected at creation time.
func TestCreateRejectsSecondPendingIntentPerOrder(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "dup-intent", 5000)
	order := model.Order{OrderNo: "DUP1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	seedPendingIntent(t, db, user.ID, order.ID, 2000, 1000, "dup-intent-1")

	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	_, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID,
	})
	if err == nil {
		t.Fatal("a second pending intent for the same order must be rejected")
	}
	if balanceOf(t, db, user.ID) != 4000 {
		t.Fatalf("rejected create must not touch the wallet, got %d", balanceOf(t, db, user.ID))
	}
}

// MONEY-05: after the first intent is cancelled (failed locally), creating a
// fresh intent must be allowed again.
func TestCreateAllowsNewIntentAfterCancel(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "recycle", 5000)
	order := model.Order{OrderNo: "REC1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedPendingIntent(t, db, user.ID, order.ID, 2000, 1000, "recycle-1")
	db.Model(&model.ExternalPayment{}).Where("id = ?", intent.ID).Update("status", model.ExternalPaymentFailed)

	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	// Balance-only create hits the "no plugin" error AFTER passing the
	// duplicate check; a duplicate would surface as a conflict about the
	// existing intent instead.
	_, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, BalanceCents: 500,
	})
	if err == nil {
		t.Fatal("expected plugin-unavailable failure after the duplicate check passed")
	}
	if _, ok := AsError(err); !ok || err.Error() == "" {
		t.Fatalf("unexpected error shape: %v", err)
	}
	// The important assertion: the error must NOT be the duplicate-intent
	// conflict — that would mean a failed intent blocks fresh payments.
	var bizErr *Error
	if errors.As(err, &bizErr) && bizErr.Status == 409 && strings.Contains(err.Error(), "进行中的支付") {
		t.Fatalf("a failed prior intent must not block a new one, got: %v", err)
	}
}

// MONEY-05: a paid callback arriving after the intent was cancelled must be
// recorded as a late state, not silently acknowledged as success.
func TestLateCallbackAfterCancelIsRecorded(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "late-cb", 3000)
	order := model.Order{OrderNo: "LATE1", UserID: user.ID, Status: model.OrderPending, TotalCents: 3000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedPendingIntent(t, db, user.ID, order.ID, 3000, 0, "late-cb-1")
	if _, err := NewPaymentService(db, nil, NewWalletService(db), nil, nil).Cancel(user.ID, intent.ID); err != nil {
		t.Fatal(err)
	}

	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	if err := payments.FinalizeCallback(context.Background(), "epay", "late-cb-1", 3000, "gw-late"); err != nil {
		t.Fatalf("late callback must not error the gateway reply: %v", err)
	}
	var got model.ExternalPayment
	if err := db.First(&got, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentLate {
		t.Fatalf("cancelled intent receiving a paid callback must be recorded as late, got %s", got.Status)
	}
	if got.PaidAmountCents != 3000 {
		t.Fatalf("late receipt amount must be preserved for reconciliation, got %d", got.PaidAmountCents)
	}
	if got.GatewayRef != "gw-late" {
		t.Fatalf("late receipt gateway ref must be preserved, got %s", got.GatewayRef)
	}
}

var _ = errors.New
var _ = fmt.Sprintf
var _ = pb.PaymentState_PAYMENT_STATE_PAID
var _ = time.Now
