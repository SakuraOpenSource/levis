package server

import (
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
	"testing"
)

func TestAffiliateReservedWithdrawalAdminWalletSettlement(t *testing.T) {
	rt, h, admin, users := installedWithUsers(t, "publisher")
	aff := service.NewAffiliateService(rt.DB())
	if _, err := aff.UpdateSettings(service.AffiliateSettings{Enabled: true, RateBPS: 5000, MinWithdrawalCents: 100}); err != nil {
		t.Fatal(err)
	}
	ownerID := userIDByName(t, h, admin, "publisher")
	joined, err := aff.Join(ownerID)
	if err != nil {
		t.Fatal(err)
	}
	buyer, err := service.NewUserService(rt.DB()).Register(service.RegisterRequest{Username: "commissionbuyer", Email: "cb@example.com", Password: "password123", ReferralCode: joined.Code})
	if err != nil {
		t.Fatal(err)
	}
	rt.DB().Model(buyer).Update("balance_cents", 2000)
	productID := seedProductVia(t, h, admin, "aff-commission", 2000)
	cart := service.NewCartService(rt.DB())
	cart.Add(buyer.ID, service.AddRequest{ProductID: productID, Quantity: 1})
	orders := service.NewOrderService(rt.DB(), cart, service.NewWalletService(rt.DB()), nil)
	order, err := orders.CreateFromCart(buyer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = orders.Pay(buyer.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	r := doAs(t, h, "POST", "/api/affiliate/withdrawals", map[string]any{"amount_cents": 800, "account": "Levis wallet", "remark": "commission"}, users["publisher"])
	if r.Code != 200 {
		t.Fatalf("withdraw=%d %s", r.Code, r.Body.String())
	}
	var w struct {
		ID     uint   `json:"id"`
		Status string `json:"status"`
	}
	decodeJSON(t, r, &w)
	if w.Status != "pending" {
		t.Fatal(w)
	}
	r = doAs(t, h, "POST", "/api/affiliate/withdrawals", map[string]any{"amount_cents": 800, "account": "wallet"}, users["publisher"])
	if r.Code != 400 {
		t.Fatalf("double spend=%d", r.Code)
	}
	var summary struct {
		Balance  int64 `json:"balance_cents"`
		Reserved int64 `json:"pending_cents"`
	}
	r = doAs(t, h, "GET", "/api/affiliate", nil, users["publisher"])
	decodeJSON(t, r, &summary)
	if summary.Balance != 200 || summary.Reserved != 800 {
		t.Fatalf("reserve=%+v", summary)
	}
	review := "/api/admin/affiliate/withdrawals/" + itoa(w.ID) + "/review"
	if r = doAs(t, h, "POST", review, map[string]string{"action": "approve"}, users["publisher"]); r.Code != 403 {
		t.Fatal("user can review")
	}
	r = doAs(t, h, "POST", review, map[string]string{"action": "approve"}, admin)
	if r.Code != 200 {
		t.Fatalf("review=%d %s", r.Code, r.Body.String())
	}
	if r = doAs(t, h, "POST", review, map[string]string{"action": "approve"}, admin); r.Code != 409 {
		t.Fatalf("repeat review=%d", r.Code)
	}
	var owner model.User
	rt.DB().First(&owner, ownerID)
	if owner.BalanceCents != 800 {
		t.Fatalf("wallet settlement=%d", owner.BalanceCents)
	}
	r = doAs(t, h, "GET", "/api/affiliate/commissions", nil, users["publisher"])
	if r.Code != 200 {
		t.Fatalf("ledger=%d", r.Code)
	}
	r = doAs(t, h, "GET", "/api/admin/affiliate/withdrawals", nil, admin)
	if r.Code != 200 {
		t.Fatalf("admin list=%d", r.Code)
	}
}
