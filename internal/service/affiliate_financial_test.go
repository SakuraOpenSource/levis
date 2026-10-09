package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"sync"
	"testing"
)

func TestRefundCannotRefundUnpaidOrCompletedOrder(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "refundlimits", 10000)
	p := seedProduct(t, db, "product", 1000)
	s := NewRefundService(db, nil, NewWalletService(db))
	unpaid := model.Order{OrderNo: "unpaid", UserID: u.ID, TotalCents: 1000, Status: model.OrderPending}
	db.Create(&unpaid)
	if _, e := s.Create(context.Background(), u.ID, RefundCreateInput{OrderID: unpaid.ID, Reason: "unpaid"}); e == nil {
		t.Fatal("unpaid order refundable")
	}
	svc := buyService(t, db, u, p)
	r, e := s.Create(context.Background(), u.ID, RefundCreateInput{ServiceID: svc.ID, Reason: "first"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Review(context.Background(), 99, r.ID, RefundReviewInput{Approve: true}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Create(context.Background(), u.ID, RefundCreateInput{ServiceID: svc.ID, Reason: "again"}); e == nil {
		t.Fatal("completed order refundable again")
	}
}
func TestAffiliatePaidWithdrawalRefundDebtAndConcurrency(t *testing.T) {
	db, aff, owner, buyer := affiliateFixture(t)
	p := seedProduct(t, db, "paidwithdrawrefund", 10000)
	svc := buyService(t, db, buyer, p)
	admin := seedUser(t, db, "affadmin", 0)
	db.Model(admin).Update("role", model.RoleAdmin)
	w, e := aff.Withdraw(owner.ID, AffiliateWithdrawalInput{AmountCents: 777, Account: "wallet"})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); aff.Review(admin.ID, w.ID, AffiliateReviewInput{Action: "approve"}) }()
	}
	wg.Wait()
	if balanceOf(t, db, owner.ID) != 777 {
		t.Fatal("concurrent withdrawal paid more than once")
	}
	s := NewRefundService(db, nil, NewWalletService(db))
	r, e := s.Create(context.Background(), buyer.ID, RefundCreateInput{ServiceID: svc.ID, Reason: "refund after payout"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Review(context.Background(), admin.ID, r.ID, RefundReviewInput{Approve: true}); e != nil {
		t.Fatal(e)
	}
	summary, e := aff.Summary(owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	if summary.BalanceCents != -777 || summary.TotalEarnedCents != 0 || summary.PendingCents != 0 {
		t.Fatalf("paid withdrawal reversal liability lost %+v", summary)
	}
	if _, e = aff.Withdraw(owner.ID, AffiliateWithdrawalInput{AmountCents: 100, Account: "wallet"}); e == nil {
		t.Fatal("refund debt allowed spending")
	}
}
