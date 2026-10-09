package service

import (
	"context"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// Losing the payment-level payout race must leave the losing request
// retryable (failed), not wedged in processing forever (review blocker 3).
func TestRefundLoserResetsToFailedNotProcessing(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "refund-loser", 5000)
	order := model.Order{OrderNo: "RL1", UserID: user.ID, Status: model.OrderPaid, TotalCents: 1000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	paid := model.ExternalPayment{
		PluginID: "epay", ExternalID: "rl-1", UserID: user.ID,
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID,
		AmountCents: 1000, Status: model.ExternalPaymentRefunding,
	}
	if err := db.Create(&paid).Error; err != nil {
		t.Fatal(err)
	}
	request := model.RefundRequest{
		OrderID: order.ID, UserID: user.ID, AmountCents: 1000,
		ChannelCents: 1000, BalanceCents: 0, PaymentID: paid.ID,
		Status: model.RefundApproved,
	}
	if err := db.Create(&request).Error; err != nil {
		t.Fatal(err)
	}

	svc := NewRefundService(db, nil, NewWalletService(db))
	if err := svc.execute(context.Background(), &request); err == nil {
		t.Fatal("losing the payout claim must surface a conflict")
	}
	var got model.RefundRequest
	if err := db.First(&got, request.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.RefundFailed {
		t.Fatalf("loser must be reset to failed for retryability, got %s", got.Status)
	}
	if got.FailReason == "" {
		t.Fatal("reset must record why the request did not pay out")
	}
}
