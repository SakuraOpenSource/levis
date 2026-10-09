package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// lateCallbackGateway reports PAID only after the cancel path has visibly
// started, reproducing the observe-pending -> cancel -> finalize race.
type lateCallbackGateway struct {
	contractPaymentGateway
	cancelStarted chan struct{}
	allowPaid     chan struct{}
}

func (g *lateCallbackGateway) QueryPayment(ctx context.Context, r *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	return &pb.QueryPaymentReply{State: pb.PaymentState_PAYMENT_STATE_PENDING}, nil
}

func itoa(v uint) string { return fmt.Sprint(v) }

// MONEY-05 race: query observes pending, user cancels, then the verified paid
// callback lands. The final state must be a durable late record, never an
// eternal pending and never a silent success.
func TestLateCallbackRaceAfterCancelDuringQuery(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "late-race", 5000)
	product := seedProduct(t, db, "late-race-product", 1000)
	orders := NewOrderService(db, nil, NewWalletService(db), nil)
	order, err := orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &lateCallbackGateway{cancelStarted: make(chan struct{}), allowPaid: make(chan struct{})}
	payments, method := newContractPaymentService(t, db, &gateway.contractPaymentGateway)
	intent, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, PluginID: itoa(method.ID),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Phase 1: user cancel wins while the channel still considers it payable.
	if _, err := payments.Cancel(user.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	close(gateway.cancelStarted)

	// Phase 2: the late verified receipt arrives for a now-failed intent.
	if err := payments.FinalizeCallback(context.Background(), method.PluginID, intent.ExternalID, 1000, "gw-late-race"); err != nil {
		t.Fatalf("late callback must not error the gateway reply: %v", err)
	}
	var got model.ExternalPayment
	if err := db.First(&got, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentLate {
		t.Fatalf("cancelled intent must hold a durable late record, got %s", got.Status)
	}
	if got.PaidAmountCents != 1000 || got.GatewayRef != "gw-late-race" {
		t.Fatalf("late receipt must preserve amount and gateway identity, got %d/%s", got.PaidAmountCents, got.GatewayRef)
	}
	// No auto-credit: reconciliation is a human decision.
	if balanceOf(t, db, user.ID) != 5000 {
		t.Fatal("late money must not be auto-credited")
	}
	var svc model.Order
	if err := db.First(&svc, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if svc.Status != model.OrderPending {
		t.Fatal("late receipt must not provision or pay the order")
	}

	// Repeated late callbacks must not rewrite the receipt.
	if err := payments.FinalizeCallback(context.Background(), method.PluginID, intent.ExternalID, 500, "gw-late-race-2"); err != nil {
		t.Fatal(err)
	}
	var again model.ExternalPayment
	if err := db.First(&again, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if again.PaidAmountCents != 1000 || again.GatewayRef != "gw-late-race" {
		t.Fatalf("duplicate late callback changed the durable receipt: %d/%s", again.PaidAmountCents, again.GatewayRef)
	}
}

// A late callback racing a refunding/refunded intent must not resurrect it.
func TestLateCallbackCannotOverwriteRefundedIntent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "late-refunded", 5000)
	order := model.Order{OrderNo: "LR2", UserID: user.ID, Status: model.OrderPending, TotalCents: 1000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedPendingIntent(t, db, user.ID, order.ID, 1000, 0, "late-refunded-1")
	db.Model(&model.ExternalPayment{}).Where("id = ?", intent.ID).Update("status", model.ExternalPaymentRefunded)
	payments := NewPaymentService(db, nil, NewWalletService(db), nil, nil)
	if err := payments.FinalizeCallback(context.Background(), "epay", "late-refunded-1", 1000, "gw-after-refund"); err != nil {
		t.Fatal(err)
	}
	var got model.ExternalPayment
	if err := db.First(&got, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ExternalPaymentRefunded {
		t.Fatalf("refunded is terminal; late callback must not downgrade it, got %s", got.Status)
	}
	if got.PaidAmountCents != 0 {
		t.Fatalf("refunded receipt amounts must stay untouched, got %d", got.PaidAmountCents)
	}
}

// Concurrent duplicate callbacks for the same pending intent must settle once.
func TestDuplicateConcurrentCallbacksSettleOnce(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "dup-callback", 5000)
	product := seedProduct(t, db, "dup-callback-product", 1000)
	orders := NewOrderService(db, nil, NewWalletService(db), nil)
	order, err := orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	payments, method := newContractPaymentService(t, db, &contractPaymentGateway{})
	intent, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, PluginID: itoa(method.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = payments.FinalizeCallback(context.Background(), method.PluginID, intent.ExternalID, 1000, "gw-dup")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("callback %d errored: %v", i, err)
		}
	}
	var got model.Order
	if err := db.First(&got, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.OrderPaid {
		t.Fatalf("order must be paid exactly once, got %s", got.Status)
	}
	if n := countModel(t, db, &model.Service{}); n != 1 {
		t.Fatalf("duplicate callbacks provisioned %d services", n)
	}
	var stored model.ExternalPayment
	if err := db.First(&stored, intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ActiveTargetKey != nil {
		t.Fatal("paid intent must release its canonical target key")
	}
	if balanceOf(t, db, user.ID) != 5000 {
		t.Fatal("settlement must not debit the wallet on the channel path")
	}
}
