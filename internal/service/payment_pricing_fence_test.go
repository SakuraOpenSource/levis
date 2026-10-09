package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"gorm.io/gorm"
)

func TestPaymentCreateRenewalPricingFence(t *testing.T) {
	for _, purpose := range []string{model.ExternalPaymentPurposeRenewal, model.ExternalPaymentPurposeInvoice} {
		for _, mutation := range []string{"pending-change", "price", "expiry", "manual-suspend"} {
			t.Run(purpose+"/"+mutation, func(t *testing.T) {
				db := newTestDB(t)
				user := seedUser(t, db, "create-fence", 5000)
				svc := seedService(t, db, user.ID, "fenced-service", 1000)
				expiry := time.Date(2030, 4, 15, 0, 0, 0, 0, time.UTC)
				if err := db.Model(svc).Update("expires_at", expiry).Error; err != nil {
					t.Fatal(err)
				}
				billing := NewBillingService(db, NewWalletService(db), nil)
				targetID := svc.ID
				if purpose == model.ExternalPaymentPurposeInvoice {
					invoice, err := billing.CreateRenewalInvoice(user.ID, svc.ID)
					if err != nil {
						t.Fatal(err)
					}
					targetID = invoice.ID
				}
				gateway := &contractPaymentGateway{}
				payments, method := newContractPaymentService(t, db, gateway)
				var once sync.Once
				// Change the DB after preflight but before intent reservation. The
				// transactional recheck must reject the stale quote before RPC/money.
				if err := db.Callback().Query().After("gorm:query").Register("pricing_change", func(tx *gorm.DB) {
					if tx.Statement.Table != "payment_methods" {
						return
					}
					once.Do(func() {
						updates := map[string]any{}
						switch mutation {
						case "pending-change":
							updates["change_pending_id"] = 42
						case "price":
							updates["price_cents"] = 2000
						case "expiry":
							updates["expires_at"] = expiry.AddDate(0, 1, 0)
						case "manual-suspend":
							updates["status"], updates["suspend_reason"] = model.ServiceSuspended, "manual"
						}
						if err := db.Model(svc).Updates(updates).Error; err != nil {
							tx.AddError(err)
						}
					})
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Callback().Query().Remove("pricing_change") })
				_, err := payments.Create(context.Background(), user.ID, "127.0.0.1", PaymentCreateInput{
					Purpose: purpose, TargetID: targetID, PluginID: fmt.Sprint(method.ID),
				})
				if err == nil {
					t.Fatal("creation must recheck current billing fence, price and expiry inside reservation transaction")
				}
				if gateway.creates.Load() != 0 || countModel(t, db, &model.ExternalPayment{}) != 0 || balanceOf(t, db, user.ID) != 5000 {
					t.Fatal("a stale pricing window must not create channel orders, intents or debits")
				}
			})
		}
	}
}

func TestCreateRenewalInvoiceRollsBackItemFailure(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "invoice-atomic", 5000)
	svc := seedService(t, db, user.ID, "atomic-service", 1000)
	if err := db.Exec("CREATE TRIGGER reject_renewal_item BEFORE INSERT ON invoice_items BEGIN SELECT RAISE(FAIL, 'injected invoice item failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewBillingService(db, NewWalletService(db), nil).CreateRenewalInvoice(user.ID, svc.ID); err == nil {
		t.Fatal("injected SQL failure must be returned")
	}
	if n := countModel(t, db, &model.Invoice{}); n != 0 {
		t.Fatalf("item failure left %d orphan invoices", n)
	}
}

type invoiceBeforeChangeHost struct {
	*resizeFake
	before func()
	once   sync.Once
}

func (h *invoiceBeforeChangeHost) GetHost(ctx context.Context, id string, r *pb.GetHostRequest) (*pb.GetHostReply, error) {
	h.once.Do(h.before)
	return h.resizeFake.GetHost(ctx, id, r)
}

func TestChangeReservationRechecksConcurrentRenewalInvoice(t *testing.T) {
	fx := newChangeFenceFixture(t, "invoice-before-reserve", time.Now().UTC().AddDate(0, 1, 0), 1000, 2000)
	var injected atomic.Bool
	host := &invoiceBeforeChangeHost{resizeFake: fx.fake, before: func() {
		if _, err := NewBillingService(fx.db, NewWalletService(fx.db), nil).CreateRenewalInvoice(fx.user.ID, fx.svc.ID); err != nil {
			t.Fatal(err)
		}
		injected.Store(true)
	}}
	_, err := NewServiceChangeService(fx.db, host).Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID})
	if !injected.Load() {
		t.Fatal("test did not exercise the precheck/reservation gap")
	}
	if err == nil {
		t.Fatal("reservation must recheck the billing fence after taking the service lease")
	}
	if countModel(t, fx.db, &model.ServiceChange{}) != 0 || balanceOf(t, fx.db, fx.user.ID) != 100000 || fx.fake.calls != 0 {
		t.Fatal("lost fence must roll back reservation and wallet and never resize")
	}
	var svc model.Service
	if err := fx.db.First(&svc, fx.svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if svc.ChangePendingID != nil {
		t.Fatal("failed reservation leaked the service lease")
	}
}
