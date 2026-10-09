package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
)

func TestWalletAlternativePaymentPathsRejectLegacyIntents(t *testing.T) {
	for _, path := range []string{"invoice-order", "renewal-invoice", "renewal-direct", "invoice-renewal"} {
		t.Run(path, func(t *testing.T) {
			db := newTestDB(t)
			user := seedUser(t, db, "legacy-alias", 5000)
			wallet := NewWalletService(db)
			orders := NewOrderService(db, nil, wallet, nil)
			billing := NewBillingService(db, wallet, nil)
			payments := NewPaymentService(db, nil, wallet, orders, billing)
			var invoice *model.Invoice
			var svc *model.Service
			var order *model.Order
			var err error
			purpose := model.ExternalPaymentPurposeRenewal
			var targetID uint
			if path == "invoice-order" {
				product := seedProduct(t, db, "legacy-product", 1000)
				order, err = orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				invoice = &model.Invoice{}
				if err := db.First(invoice, "order_id = ?", order.ID).Error; err != nil {
					t.Fatal(err)
				}
				purpose, targetID = model.ExternalPaymentPurposeOrder, order.ID
			} else {
				svc = seedService(t, db, user.ID, "legacy-service", 1000)
				invoice, err = billing.CreateRenewalInvoice(user.ID, svc.ID)
				if err != nil {
					t.Fatal(err)
				}
				targetID = svc.ID
				if path == "renewal-invoice" {
					purpose, targetID = model.ExternalPaymentPurposeInvoice, invoice.ID
				}
			}
			// Omit ActiveTargetKey deliberately: upgrades must also fence legacy NULLs.
			intent := model.ExternalPayment{PluginID: "epay", ExternalID: "legacy-" + path,
				UserID: user.ID, Purpose: purpose, TargetID: targetID, AmountCents: 1000,
				Status: model.ExternalPaymentProcessing}
			if err := db.Create(&intent).Error; err != nil {
				t.Fatal(err)
			}
			if path == "renewal-invoice" || path == "renewal-direct" {
				_, err = billing.Renew(user.ID, svc.ID)
			} else {
				_, err = payments.SettleInvoice(user.ID, invoice.ID)
			}
			if err == nil {
				t.Fatal("wallet alternative must not settle a live canonical channel target")
			}
			if got := balanceOf(t, db, user.ID); got != 5000 {
				t.Fatalf("rejected alternative debited wallet: %d", got)
			}
			var got model.Invoice
			if err := db.First(&got, invoice.ID).Error; err != nil {
				t.Fatal(err)
			}
			if got.Status != model.InvoiceUnpaid {
				t.Fatalf("rejected alternative changed invoice: %s", got.Status)
			}
		})
	}
}

func TestPaymentCreateInvoiceRejectsLegacyOrderIntent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "create-alias", 5000)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, nil, wallet, nil)
	product := seedProduct(t, db, "create-alias-product", 1000)
	order, err := orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var invoice model.Invoice
	if err := db.First(&invoice, "order_id = ?", order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ExternalPayment{PluginID: "epay", ExternalID: "legacy-create-alias", UserID: user.ID,
		Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, AmountCents: 1000, Status: model.ExternalPaymentPending}).Error; err != nil {
		t.Fatal(err)
	}
	payments := NewPaymentService(db, nil, wallet, orders, NewBillingService(db, wallet, nil))
	if _, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
		Purpose: model.ExternalPaymentPurposeInvoice, TargetID: invoice.ID, BalanceCents: 1000,
	}); err == nil {
		t.Fatal("balance-only intent must not bypass an open order channel")
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Fatalf("rejected alias debited wallet: %d", got)
	}
}

func TestPaymentActiveTargetUniqueIndex(t *testing.T) {
	db := newTestDB(t)
	if !db.Migrator().HasIndex(&model.ExternalPayment{}, "idx_external_payment_active_target") {
		t.Fatal("active targets require a nullable unique index on every supported SQL dialect")
	}
	user := seedUser(t, db, "unique-target", 0)
	for i := 0; i < 2; i++ {
		row := model.ExternalPayment{PluginID: "epay", ExternalID: fmt.Sprintf("unique-%d", i), UserID: user.ID,
			Purpose: model.ExternalPaymentPurposeOrder, TargetID: 1, AmountCents: 1000}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		err := db.Model(&row).UpdateColumn("active_target_key", "order:1").Error
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && err == nil {
			t.Fatal("database accepted a second live canonical target")
		}
	}
	if err := db.Model(&model.ExternalPayment{}).Where("external_id = ?", "unique-0").UpdateColumn("active_target_key", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ExternalPayment{}).Where("external_id = ?", "unique-1").UpdateColumn("active_target_key", nil).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ExternalPayment{}).Where("active_target_key IS NULL").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("NULL terminal/recharge keys must coexist")
	}
}

func countModel(t *testing.T, db *gorm.DB, value any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(value).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPaymentActiveKeyTerminalTransitions(t *testing.T) {
	for _, mode := range []string{"cancel", "create-failed", "paid", "wallet-paid"} {
		t.Run(mode, func(t *testing.T) {
			db := newTestDB(t)
			user := seedUser(t, db, "key-terminal", 5000)
			product := seedProduct(t, db, "key-product", 1000)
			orders := NewOrderService(db, nil, NewWalletService(db), nil)
			order, err := orders.CreateDirect(user.ID, []OrderLine{{ProductID: product.ID, Quantity: 1}})
			if err != nil {
				t.Fatal(err)
			}
			gateway := &contractPaymentGateway{}
			if mode == "create-failed" {
				gateway.create = func(context.Context, *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
					return nil, status.Error(codes.InvalidArgument, "test gateway rejected creation")
				}
			}
			payments, method := newContractPaymentService(t, db, gateway)
			in := PaymentCreateInput{Purpose: model.ExternalPaymentPurposeOrder, TargetID: order.ID, PluginID: fmt.Sprint(method.ID)}
			if mode == "wallet-paid" {
				in.BalanceCents = 1000
			}
			intent, err := payments.Create(context.Background(), user.ID, "127.0.0.1", in)
			if mode != "create-failed" && err != nil {
				t.Fatal(err)
			}
			if mode == "create-failed" && err == nil {
				t.Fatal("gateway rejection must be returned")
			}
			if mode == "cancel" {
				if _, err := payments.Cancel(user.ID, intent.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "paid" {
				if err := payments.FinalizeCallback(context.Background(), method.PluginID, intent.ExternalID, 1000, intent.GatewayRef); err != nil {
					t.Fatal(err)
				}
			}
			var stored model.ExternalPayment
			if err := db.First(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if stored.ActiveTargetKey != nil {
				t.Fatalf("terminal %s must release canonical key, got %q", mode, *stored.ActiveTargetKey)
			}
		})
	}
}
