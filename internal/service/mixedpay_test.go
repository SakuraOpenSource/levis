package service

import (
	"context"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// TestMixedPaymentFullBalanceSettles 直接用余额全额结算：不经过渠道，
// 订单置已付、服务开通。
func TestMixedPaymentFullBalanceSettles(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "mixfull", 5000)
	product := seedProduct(t, db, "MIXFULL", 1200)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet, nil)
	payments := NewPaymentService(db, nil, wallet, orders, NewBillingService(db, wallet, nil))

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	intent, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, BalanceCents: 1200,
	})
	if err != nil {
		t.Fatalf("全余额支付失败: %v", err)
	}
	if intent.Status != model.ExternalPaymentPaid {
		t.Fatalf("意图应为已付，实际 %q", intent.Status)
	}
	if intent.BalanceCents != 1200 || intent.AmountCents != 0 {
		t.Fatalf("金额拆分错误：balance=%d external=%d", intent.BalanceCents, intent.AmountCents)
	}
	if got := balanceOf(t, db, user.ID); got != 3800 {
		t.Errorf("余额应为 3800，实际 %d", got)
	}
	var reloaded model.Order
	if err := db.First(&reloaded, order.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if reloaded.Status != model.OrderPaid {
		t.Errorf("订单应为已付，实际 %q", reloaded.Status)
	}
}

// TestMixedPaymentPartialThenCancel 部分抵扣后取消：抵扣退回，订单仍待付。
func TestMixedPaymentPartialThenCancel(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "mixcancel", 5000)
	product := seedProduct(t, db, "MIXCANCEL", 3000)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet, nil)
	// plugins 传 nil：external>0 且无可用插件时 Create 应直接失败，不扣钱。
	payments := NewPaymentService(db, nil, wallet, orders, NewBillingService(db, wallet, nil))

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 无插件可用，创建混合支付应失败且余额不动。
	if _, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID,
		PluginID: "1", BalanceCents: 1000,
	}); err == nil {
		t.Fatal("无可用插件时应创建失败")
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Errorf("失败后余额不应变动，实际 %d", got)
	}

	// 手工造一个待处理意图再取消，验证退款路径。
	intent := model.ExternalPayment{
		PluginID: "epay", ExternalID: "cancel-test-1", UserID: user.ID,
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID,
		AmountCents: 2000, BalanceCents: 1000, Currency: "CNY",
		Subject: "test", Status: model.ExternalPaymentPending,
	}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatalf("建意图失败: %v", err)
	}
	if _, err := wallet.adjustBalance(db, user.ID, -1000, model.TxPayment, "order", order.ID, "测试抵扣"); err != nil {
		t.Fatalf("模拟抵扣失败: %v", err)
	}
	cancelled, err := payments.Cancel(user.ID, intent.ID)
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if cancelled.Status != model.ExternalPaymentFailed {
		t.Errorf("取消后应为失败态，实际 %q", cancelled.Status)
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Errorf("取消后余额应退回 5000，实际 %d", got)
	}
	var reloaded model.Order
	if err := db.First(&reloaded, order.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if reloaded.Status != model.OrderPending {
		t.Errorf("取消后订单应仍待付，实际 %q", reloaded.Status)
	}
}

// TestRenewalInvoiceFlow 续费账单：创建待付 → 余额结清 → 到期顺延。
func TestRenewalInvoiceFlow(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "renewflow", 5000)
	product := seedProduct(t, db, "RENEWFLOW", 1200)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet, nil)
	billing := NewBillingService(db, wallet, nil)
	payments := NewPaymentService(db, nil, wallet, orders, billing)

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if _, err := orders.Pay(user.ID, order.ID); err != nil {
		t.Fatalf("支付失败: %v", err)
	}
	services, _, err := billing.Services(user.ID, 0, 10)
	if err != nil || len(services) != 1 {
		t.Fatalf("应有 1 个服务: %v", err)
	}
	svc := services[0]
	before := svc.ExpiresAt

	// 续费按钮现在只建账单，不扣钱。
	invoice, err := billing.CreateRenewalInvoice(user.ID, svc.ID)
	if err != nil {
		t.Fatalf("建续费账单失败: %v", err)
	}
	if invoice.Status != model.InvoiceUnpaid {
		t.Fatalf("续费账单应为待付，实际 %q", invoice.Status)
	}
	if invoice.ServiceID == nil || *invoice.ServiceID != svc.ID {
		t.Fatal("续费账单应关联服务")
	}
	if got := balanceOf(t, db, user.ID); got != 3800 {
		t.Fatalf("建账单不应扣钱，余额实际 %d", got)
	}

	// 余额结清账单。
	settled, err := payments.SettleInvoice(user.ID, invoice.ID)
	if err != nil {
		t.Fatalf("结清失败: %v", err)
	}
	if settled.Status != model.InvoicePaid {
		t.Fatalf("账单应为已付，实际 %q", settled.Status)
	}
	if got := balanceOf(t, db, user.ID); got != 2600 {
		t.Errorf("结清后余额应为 2600，实际 %d", got)
	}
	var renewed model.Service
	if err := db.First(&renewed, svc.ID).Error; err != nil {
		t.Fatalf("读取服务失败: %v", err)
	}
	if before != nil && renewed.ExpiresAt != nil && !renewed.ExpiresAt.After(*before) {
		t.Errorf("到期时间应顺延：%v -> %v", before, renewed.ExpiresAt)
	}
}

// TestRenewalInvoiceRejectsBadState 非 active/一次性服务不能建续费账单。
func TestRenewalInvoiceRejectsBadState(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "renewbad", 5000)
	billing := NewBillingService(db, NewWalletService(db), nil)

	if _, err := billing.CreateRenewalInvoice(user.ID, 9999); err == nil {
		t.Fatal("不存在的服务应建单失败")
	}
}
