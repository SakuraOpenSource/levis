package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"sync/atomic"
	"testing"
	"time"
)

type blockingRefund struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (f *blockingRefund) RefundPayment(context.Context, string, *pb.RefundPaymentRequest) (*pb.RefundPaymentReply, error) {
	f.calls.Add(1)
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.release
	return &pb.RefundPaymentReply{Ok: true}, nil
}
func TestRefundConcurrentExecutionCannotClaimApprovedTwice(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "refundconcurrency", 0)
	payment := model.ExternalPayment{UserID: u.ID, ExternalID: "paid", Status: model.ExternalPaymentPaid, PluginID: "channel", Purpose: model.ExternalPaymentPurposeOrder}
	db.Create(&payment)
	r := model.RefundRequest{RefundNo: "refund", UserID: u.ID, PaymentID: payment.ID, AmountCents: 200, ChannelCents: 100, BalanceCents: 100, Status: model.RefundApproved}
	db.Create(&r)
	f := &blockingRefund{entered: make(chan struct{}, 2), release: make(chan struct{})}
	s := NewRefundService(db, f, NewWalletService(db))
	done := make(chan error, 2)
	go func() { copy := r; done <- s.execute(context.Background(), &copy) }()
	<-f.entered
	go func() { copy := r; done <- s.execute(context.Background(), &copy) }()
	select {
	case <-f.entered:
		close(f.release)
		<-done
		<-done
		t.Fatal("second execution reached external money outlet")
	case <-time.After(100 * time.Millisecond):
	}
	close(f.release)
	<-done
	<-done
	if f.calls.Load() != 1 || balanceOf(t, db, u.ID) != 100 {
		t.Fatal("refund double-spent")
	}
	var got model.RefundRequest
	db.First(&got, r.ID)
	if got.Status != model.RefundCompleted {
		t.Fatal("losing execution overwrote completed", got.Status)
	}
}
