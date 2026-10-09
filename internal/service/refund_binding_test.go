package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// This file guards the money-outlet invariants from the security audit:
// MONEY-02 (payment/order binding), MONEY-03 (payment-level refund claim),
// MONEY-04 (entitlement revocation on full refund).

// seedPaidOrder creates a paid order owned by userID.
func seedPaidOrder(t *testing.T, db *gorm.DB, userID uint, no string, total int64) *model.Order {
	t.Helper()
	order := model.Order{OrderNo: no, UserID: userID, Status: model.OrderPaid, TotalCents: total}
	if err := db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	return &order
}

// seedPaidPayment creates a paid external payment with the given purpose/target.
func seedPaidPayment(t *testing.T, db *gorm.DB, userID, targetID uint, purpose string, amount int64) *model.ExternalPayment {
	t.Helper()
	now := time.Now()
	pay := model.ExternalPayment{
		PluginID: "epay", ExternalID: fmt.Sprintf("ext-%d-%s", targetID, purpose), UserID: userID,
		Purpose: purpose, TargetID: targetID, AmountCents: amount, Currency: "CNY",
		Subject: "test", Status: model.ExternalPaymentPaid, PaidAt: &now,
	}
	if err := db.Create(&pay).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}
	return &pay
}

// MONEY-02: a recharge payment must never enter the order-refund flow.
// The channel money would be returned while the wallet credit from the same
// recharge stays spendable — a direct loss.
func TestRefundRejectsRechargePaymentBoundToCheapOrder(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-recharge", 0)
	wallet := NewWalletService(db)
	plugins := &fakeRefundPlugins{}
	svc := NewRefundService(db, plugins, wallet)

	recharge := seedPaidPayment(t, db, user.ID, 0, model.ExternalPaymentPurposeRecharge, 100000)
	order := seedPaidOrder(t, db, user.ID, "RCH1", 100)

	_, err := svc.Create(context.Background(), user.ID, RefundCreateInput{
		PaymentID: recharge.ID, OrderID: order.ID, Reason: "mixed attack",
	})
	if err == nil {
		t.Fatal("a recharge payment combined with an unrelated paid order must be rejected")
	}
	// Nothing may have been created or paid out.
	var count int64
	db.Model(&model.RefundRequest{}).Count(&count)
	if count != 0 {
		t.Fatalf("no refund request may be created, got %d", count)
	}
	if len(plugins.amounts) != 0 {
		t.Fatalf("channel must not be touched, got %v", plugins.amounts)
	}
}

// MONEY-02: the refunded payment must have settled exactly the selected order.
func TestRefundRejectsPaymentTargetMismatch(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-mismatch", 0)
	plugins := &fakeRefundPlugins{}
	svc := NewRefundService(db, plugins, NewWalletService(db))

	orderA := seedPaidOrder(t, db, user.ID, "MIS1", 5000)
	orderB := seedPaidOrder(t, db, user.ID, "MIS2", 100)
	payA := seedPaidPayment(t, db, user.ID, orderA.ID, model.ExternalPaymentPurposeOrder, 5000)

	_, err := svc.Create(context.Background(), user.ID, RefundCreateInput{
		PaymentID: payA.ID, OrderID: orderB.ID, Reason: "mismatch",
	})
	if err == nil {
		t.Fatal("payment for order A must not refund order B")
	}
}

// MONEY-02: renewal-purpose payments are not order payments either.
func TestRefundRejectsRenewalPurposePayment(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-renewal-purpose", 0)
	order := seedPaidOrder(t, db, user.ID, "RNP1", 100)
	renewal := seedPaidPayment(t, db, user.ID, 42, model.ExternalPaymentPurposeRenewal, 300)

	_, err := NewRefundService(db, &fakeRefundPlugins{}, NewWalletService(db)).
		Create(context.Background(), user.ID, RefundCreateInput{
			PaymentID: renewal.ID, OrderID: order.ID, Reason: "x",
		})
	if err == nil {
		t.Fatal("renewal-purpose payment must not enter order refund")
	}
}

// MONEY-02: after a refund completed for a payment, the already-refunded order
// must not accept a second request through the payment branch.
func TestRefundRejectsSecondRequestAfterCompletion(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{PaymentID: fx.payID, Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	// The completed refund must have flipped the payment out of "paid".
	var pay model.ExternalPayment
	if err := fx.db.First(&pay, fx.payID).Error; err != nil {
		t.Fatal(err)
	}
	if pay.Status == model.ExternalPaymentPaid {
		t.Fatalf("payment must not remain payable after a completed refund, status=%s", pay.Status)
	}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{PaymentID: fx.payID, Reason: "second"}); err == nil {
		t.Fatal("second refund request on the same payment must be rejected")
	}
}

// moneyProbePlugins records the payment status observed while the channel
// refund RPC is in flight.
type moneyProbePlugins struct {
	statusAtCall []string
	err          error
	calls        atomic.Int32
}

func (m *moneyProbePlugins) RefundPayment(_ context.Context, _ string, _ *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error) {
	m.calls.Add(1)
	return &pb.RefundPaymentReply{Ok: m.err == nil, Error: errStr(m.err)}, nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// MONEY-02: the payout claim (paid -> refunding) must happen BEFORE the channel
// RPC leaves the process.
func TestRefundClaimsPaymentBeforeChannelPayout(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-claim-order", 0)
	order := seedPaidOrder(t, db, user.ID, "CLM1", 5000)
	pay := seedPaidPayment(t, db, user.ID, order.ID, model.ExternalPaymentPurposeOrder, 5000)

	var calls atomic.Int32
	plugins := &claimProbePlugins{db: db, payID: pay.ID, calls: &calls}
	svc := NewRefundService(db, plugins, NewWalletService(db))
	if _, err := svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), user.ID, RefundCreateInput{PaymentID: pay.ID, Reason: "claim"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("channel refund was never invoked; probe is broken")
	}
	// Completion must mark the payment refunded.
	var got model.ExternalPayment
	if err := db.First(&got, pay.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentRefunded {
		t.Fatalf("payment status after completed refund = %q, want %q", got.Status, model.ExternalPaymentRefunded)
	}
}

// claimProbePlugins asserts at RPC time that the payment row is already claimed.
type claimProbePlugins struct {
	db    *gorm.DB
	payID uint
	calls *atomic.Int32
}

func (c *claimProbePlugins) RefundPayment(_ context.Context, _ string, _ *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error) {
	c.calls.Add(1)
	var pay model.ExternalPayment
	if err := c.db.First(&pay, c.payID).Error; err != nil {
		return nil, err
	}
	if pay.Status != model.ExternalPaymentRefunding {
		// Fail the channel call so the test surfaces through the refund failing.
		return &pb.RefundPaymentReply{Ok: false, Error: fmt.Sprintf("payment not claimed before payout: %s", pay.Status)}, nil
	}
	return &pb.RefundPaymentReply{Ok: true}, nil
}

// MONEY-03: two refund requests on the same payment — only the first execution
// may reach the channel; the second must be stopped by the payment claim.
func TestRefundSecondRequestCannotReachChannel(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-two-req", 0)
	order := seedPaidOrder(t, db, user.ID, "TWO1", 5000)
	pay := seedPaidPayment(t, db, user.ID, order.ID, model.ExternalPaymentPurposeOrder, 5000)

	plugins := &fakeRefundPlugins{}
	svc := NewRefundService(db, plugins, NewWalletService(db))

	mk := func(no string) *model.RefundRequest {
		item := model.RefundRequest{
			RefundNo: no, UserID: user.ID, PaymentID: pay.ID, OrderID: order.ID,
			AmountCents: 5000, ChannelCents: 5000, Status: model.RefundApproved,
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
		return &item
	}
	first, second := mk("R1"), mk("R2")

	if err := svc.execute(context.Background(), first); err != nil {
		t.Fatalf("first execution failed: %v", err)
	}
	if len(plugins.amounts) != 1 {
		t.Fatalf("first execution must call the channel exactly once, got %d", len(plugins.amounts))
	}
	before := balanceOf(t, db, user.ID)
	if err := svc.execute(context.Background(), second); err == nil {
		t.Fatal("second execution on the same payment must fail")
	}
	if len(plugins.amounts) != 1 {
		t.Fatalf("second execution must not reach the channel, calls=%d", len(plugins.amounts))
	}
	if got := balanceOf(t, db, user.ID); got != before {
		t.Fatalf("second execution must not move money: %d -> %d", before, got)
	}
	var gotSecond model.RefundRequest
	if err := db.First(&gotSecond, second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotSecond.Status == model.RefundCompleted {
		t.Fatal("second request must not complete")
	}
}

// MONEY-03: channel failure must release the payment claim so a retry can
// re-claim and re-drive.
func TestRefundChannelFailureReleasesPaymentClaim(t *testing.T) {
	fx := newRefundFixture(t)
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	fx.plugins.reply = &pb.RefundPaymentReply{Ok: false, Error: "channel down"}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{PaymentID: fx.payID, Reason: "fail"}); err != nil {
		t.Fatal(err)
	}
	var pay model.ExternalPayment
	if err := fx.db.First(&pay, fx.payID).Error; err != nil {
		t.Fatal(err)
	}
	if pay.Status != model.ExternalPaymentPaid {
		t.Fatalf("channel failure must release the claim back to paid, got %s", pay.Status)
	}
}

// MONEY-04: a completed full refund must revoke the entitlements funded by the
// order in the same transaction.
func TestRefundCompletionSuspendsServiceEntitlements(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-entitle", 0)
	order := seedPaidOrder(t, db, user.ID, "ENT1", 5000)
	pay := seedPaidPayment(t, db, user.ID, order.ID, model.ExternalPaymentPurposeOrder, 5000)
	svc := model.Service{
		UserID: user.ID, ProductID: 1, OrderID: order.ID, Name: "ent",
		Status: model.ServiceActive, BillingCyc: model.CycleMonthly,
		PriceCents: 5000, AutoRenew: true, ExpiresAt: ptrTime(time.Now().Add(20 * 24 * time.Hour)),
	}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}

	refundSvc := NewRefundService(db, &fakeRefundPlugins{}, NewWalletService(db))
	if _, err := refundSvc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := refundSvc.Create(context.Background(), user.ID, RefundCreateInput{PaymentID: pay.ID, Reason: "entitlement"}); err != nil {
		t.Fatal(err)
	}

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceSuspended {
		t.Fatalf("service must be suspended after full refund, got %s", got.Status)
	}
	if got.SuspendReason != "refund" {
		t.Fatalf("suspend reason must record the refund revocation, got %q", got.SuspendReason)
	}
	if got.AutoRenew {
		t.Fatal("auto-renew must be switched off when the refund completes")
	}
	// A refund-suspended service must not be renewable (keeps the fence closed).
	if _, err := NewBillingService(db, NewWalletService(db), nil).Renew(user.ID, svc.ID); err == nil {
		t.Fatal("refund-suspended service must not be renewable")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

var _ = errors.New
