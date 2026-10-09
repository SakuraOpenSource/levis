package service

import (
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func TestWalletOrderPaymentRejectsLiveInvoiceIntent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "payment-alias", 5000)
	product := model.Product{Name: "local product", PriceCents: 1000, BillingCyc: model.CycleMonthly, Stock: 10, Status: model.ProductActive}
	if err := db.Create(&product).Error; err != nil {
		t.Fatal(err)
	}
	order := model.Order{OrderNo: "ALIAS1", UserID: user.ID, Status: model.OrderPending, TotalCents: 1000}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.OrderItem{OrderID: order.ID, ProductID: product.ID, ProductName: product.Name, PriceCents: 1000, Quantity: 1, BillingCyc: model.CycleMonthly}).Error; err != nil {
		t.Fatal(err)
	}
	invoice := model.Invoice{InvoiceNo: "ALIASINV1", UserID: user.ID, OrderID: &order.ID, Status: model.InvoiceUnpaid, TotalCents: 1000}
	if err := db.Create(&invoice).Error; err != nil {
		t.Fatal(err)
	}
	intent := model.ExternalPayment{PluginID: "epay", ExternalID: "alias-channel", UserID: user.ID, Purpose: model.ExternalPaymentPurposeInvoice, TargetID: invoice.ID, AmountCents: 1000, Status: model.ExternalPaymentPending}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	orders := NewOrderService(db, nil, NewWalletService(db), nil)
	if _, err := orders.Pay(user.ID, order.ID); err == nil {
		t.Fatal("order and its invoice must share one live payment path; wallet must not race the open channel intent")
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Fatalf("a rejected alternative payment must preserve the wallet: %d", got)
	}
	var got model.Order
	if err := db.First(&got, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.OrderPending {
		t.Fatal("a live channel intent must keep the target pending")
	}
}
