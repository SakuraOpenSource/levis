package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"testing"
)

func affiliateFixture(t *testing.T) (*gorm.DB, *AffiliateService, *model.User, *model.User) {
	t.Helper()
	db := newTestDB(t)
	owner := seedUser(t, db, "affowner", 0)
	aff := NewAffiliateService(db)
	if _, err := aff.UpdateSettings(AffiliateSettings{Enabled: true, RateBPS: 777, MinWithdrawalCents: 100}); err != nil {
		t.Fatal(err)
	}
	summary, err := aff.Join(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	buyer, err := NewUserService(db).Register(RegisterRequest{Username: "affbuyer", Email: "affbuyer@example.com", Password: "password123", ReferralCode: summary.Code})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(buyer).Update("balance_cents", 10000).Error; err != nil {
		t.Fatal(err)
	}
	return db, aff, owner, buyer
}
func TestAffiliateActualRefundReversesCommissionOnce(t *testing.T) {
	db, aff, owner, buyer := affiliateFixture(t)
	product := seedProduct(t, db, "affrefund", 1999)
	svc := buyService(t, db, buyer, product)
	refunds := NewRefundService(db, nil, NewWalletService(db))
	req, err := refunds.Create(context.Background(), buyer.ID, RefundCreateInput{ServiceID: svc.ID, Reason: "real wallet refund"})
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := aff.Summary(owner.ID)
	if summary.BalanceCents != 155 {
		t.Fatal("pending review must not reverse")
	}
	out, err := refunds.Review(context.Background(), 99, req.ID, RefundReviewInput{Approve: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != model.RefundCompleted {
		t.Fatalf("refund=%+v", out)
	}
	summary, _ = aff.Summary(owner.ID)
	if summary.BalanceCents != 0 || summary.TotalEarnedCents != 0 {
		t.Fatalf("completed refund not reversed: %+v", summary)
	}
	if balanceOf(t, db, buyer.ID) != 10000 {
		t.Fatal("wallet refund did not execute")
	}
	if _, err = refunds.Review(context.Background(), 99, req.ID, RefundReviewInput{Approve: true}); err == nil {
		t.Fatal("repeat refund accepted")
	}
	var c model.AffiliateCommission
	db.First(&c)
	if c.ReversedCents != 155 {
		t.Fatalf("reversal=%+v", c)
	}
	var n int64
	db.Model(&model.AffiliateReversal{}).Count(&n)
	if n != 1 {
		t.Fatalf("reversal ledger count=%d", n)
	}
}

func TestAffiliateRealPaymentIntegerCommission(t *testing.T) {
	db, aff, owner, buyer := affiliateFixture(t)
	product := seedProduct(t, db, "affpaid", 1999)
	cart := NewCartService(db)
	orders := NewOrderService(db, cart, NewWalletService(db), nil)
	if err := cart.Add(buyer.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	order, err := orders.CreateFromCart(buyer.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := aff.Summary(owner.ID)
	if summary.BalanceCents != 0 {
		t.Fatal("unpaid commission")
	}
	if _, err = orders.Pay(buyer.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	summary, err = aff.Summary(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BalanceCents != 155 || summary.TotalEarnedCents != 155 {
		t.Fatalf("integer commission missing: %+v", summary)
	}
	if _, err = orders.Pay(buyer.ID, order.ID); err == nil {
		t.Fatal("duplicate payment accepted")
	}
	if _, err = orders.PayExternal(buyer.ID, order.ID); err == nil {
		t.Fatal("duplicate external payment accepted")
	}
	var rows []model.AffiliateCommission
	if err = db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AmountCents != 155 || rows[0].PaidCents != 1999 {
		t.Fatalf("ledger=%+v", rows)
	}
	free := seedProduct(t, db, "afffree", 0)
	buyService(t, db, buyer, free)
	summary, _ = aff.Summary(owner.ID)
	if summary.BalanceCents != 155 {
		t.Fatal("zero-price earns commission")
	}
}
