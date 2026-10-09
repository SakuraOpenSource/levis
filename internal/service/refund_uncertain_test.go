package service

import (
	"context"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

type uncertainRefundGateway struct {
	calls int
}

func (g *uncertainRefundGateway) RefundPayment(context.Context, string, *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error) {
	g.calls++
	return nil, context.DeadlineExceeded
}

func TestRefundTransportTimeoutRetainsPayoutClaim(t *testing.T) {
	fx := newRefundFixture(t)
	gateway := &uncertainRefundGateway{}
	svc := NewRefundService(fx.db, gateway, NewWalletService(fx.db))
	if _, err := svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	request, err := svc.Create(context.Background(), fx.userID, RefundCreateInput{PaymentID: fx.payID, Reason: "lost gateway reply"})
	if err != nil {
		t.Fatal(err)
	}
	var payment model.ExternalPayment
	if err := fx.db.First(&payment, fx.payID).Error; err != nil {
		t.Fatal(err)
	}
	if request.Status != model.RefundProcessing || payment.Status != model.ExternalPaymentRefunding {
		t.Fatalf("lost reply must retain both claims, request=%s payment=%s", request.Status, payment.Status)
	}
	if request.FailReason == "" {
		t.Fatal("unknown channel outcome must remain diagnosable")
	}
	if _, err := svc.RetryFailed(context.Background(), request.ID); err == nil {
		t.Fatal("unknown channel outcome must not be automatically replayed")
	}
	if gateway.calls != 1 {
		t.Fatalf("channel calls=%d; an uncertain payout must execute at most once", gateway.calls)
	}
	if got := balanceOf(t, fx.db, fx.userID); got != 0 {
		t.Fatalf("wallet cannot be refunded before reconciliation: %d", got)
	}
}
