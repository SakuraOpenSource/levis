package service

import (
	"context"
	"testing"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"

	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
)

// This file guards the change-product financial fence and proration math from
// the security audit: MONEY-07 (a pending resize must fence unpaid renewal
// invoices, old-price invoices, and renewal payment intents) and MONEY-08
// (proration must cover every prepaid remaining period, not clamp to the
// last cycle).

// changeFenceFixture seeds a user + active monthly service bound to a resize
// capable host, plus old/target products on the same provider/driver.
type changeFenceFixture struct {
	db      *gorm.DB
	user    *model.User
	svc     *model.Service
	old     *model.Product
	target  *model.Product
	fake    *resizeFake
	service *ServiceChangeService
}

func newChangeFenceFixture(t *testing.T, name string, expires time.Time, oldPrice, targetPrice int64) *changeFenceFixture {
	t.Helper()
	db := newTestDB(t)
	user := seedUser(t, db, name, 100000)
	cfg := model.ProvisionSpec{Driver: "qemu", Mode: "fixed", CPU: model.SpecRange{Min: 2, Max: 2}, MemoryMB: model.SpecRange{Min: 1024, Max: 1024}, DiskGB: model.SpecRange{Min: 20, Max: 20}}
	old := &model.Product{Name: name + "-old", PriceCents: oldPrice, BillingCyc: model.CycleMonthly, UpstreamPluginID: "provider", ProvisionConfig: cfg}
	if err := db.Create(old).Error; err != nil {
		t.Fatal(err)
	}
	target := &model.Product{Name: name + "-new", PriceCents: targetPrice, BillingCyc: model.CycleMonthly, UpstreamPluginID: "provider", ProvisionConfig: cfg, InterfaceID: old.InterfaceID}
	target.ProvisionConfig.CPU = model.SpecRange{Min: 4, Max: 4}
	if err := db.Create(target).Error; err != nil {
		t.Fatal(err)
	}
	svc := model.Service{
		UserID: user.ID, ProductID: old.ID, OrderID: 1, Name: "vm",
		Status: model.ServiceActive, BillingCyc: model.CycleMonthly,
		PriceCents: oldPrice, ExpiresAt: &expires,
		UpstreamPluginID: "provider", UpstreamHostID: "vm-1",
	}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	fake := &resizeFake{host: &pb.UpstreamHost{
		Id: "vm-1", Status: "stopped", Actions: []string{"resize"},
		Resources: &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 20},
	}}
	return &changeFenceFixture{
		db: db, user: user, svc: &svc, old: old, target: target,
		fake: fake, service: NewServiceChangeService(db, fake),
	}
}

// MONEY-08: with three prepaid monthly periods left and a +1000/month price
// difference, the prorated charge must cover all remaining periods (~3000),
// not the single-cycle clamp (~1000).
func TestChangeQuoteProratesAllPrepaidPeriods(t *testing.T) {
	fx := newChangeFenceFixture(t, "prorate-multi", time.Now().UTC().AddDate(0, 3, 0), 1000, 2000)
	q, err := fx.service.Preview(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Full remaining time at the full monthly difference: ratio remaining/total
	// is ~1, so the charge must be the full ~3000. The old clamp computed
	// remaining/total with total = ONE cycle, yielding ~1000.
	if q.ChargeCents < 2900 || q.ChargeCents > 3000 {
		t.Fatalf("three prepaid periods must charge ~3000, got %d (remaining=%d total=%d)", q.ChargeCents, q.RemainingSeconds, q.TotalSeconds)
	}
}

// MONEY-08: downgrade credits must likewise scale with the full remaining
// prepaid time, not one cycle.
func TestChangeQuoteProratesAllPrepaidPeriodsDowngrade(t *testing.T) {
	fx := newChangeFenceFixture(t, "prorate-down", time.Now().UTC().AddDate(0, 3, 0), 2000, 1000)
	q, err := fx.service.Preview(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if q.CreditCents < 2900 || q.CreditCents > 3000 {
		t.Fatalf("downgrade credit must cover all remaining periods (~3000), got %d", q.CreditCents)
	}
}

// MONEY-07: creating a change reservation must reject a service that has an
// unpaid, non-traffic renewal invoice — otherwise the old price could buy the
// upgraded spec's period after the change applied.
func TestChangeFencedByUnpaidRenewalInvoice(t *testing.T) {
	fx := newChangeFenceFixture(t, "fence-invoice", time.Now().UTC().Add(20*24*time.Hour), 1000, 2000)
	inv := model.Invoice{InvoiceNo: "FENCE1", UserID: fx.user.ID, ServiceID: &fx.svc.ID, Status: model.InvoiceUnpaid, TotalCents: 1000}
	if err := fx.db.Create(&inv).Error; err != nil {
		t.Fatal(err)
	}
	item := model.InvoiceItem{InvoiceID: inv.ID, ServiceID: &fx.svc.ID, Description: "续费 vm（monthly）", AmountCents: 1000}
	if err := fx.db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := fx.service.Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID}); err == nil {
		t.Fatal("a pending unpaid renewal invoice must fence the change reservation")
	}
	var changes []model.ServiceChange
	fx.db.Where("service_id = ?", fx.svc.ID).Find(&changes)
	if len(changes) != 0 {
		t.Fatalf("no ServiceChange row may be created behind the fence, got %d", len(changes))
	}
	if got := balanceOf(t, fx.db, fx.user.ID); got != 100000 {
		t.Fatalf("no wallet movement behind the fence, got %d", got)
	}
}

// MONEY-07: a live renewal payment intent must fence the change as well —
// it settles an old-price renewal outside the invoice table.
func TestChangeFencedByPendingRenewalPaymentIntent(t *testing.T) {
	fx := newChangeFenceFixture(t, "fence-intent", time.Now().UTC().Add(20*24*time.Hour), 1000, 2000)
	intent := model.ExternalPayment{
		PluginID: "epay", ExternalID: "fence-intent-1", UserID: fx.user.ID,
		Purpose: model.ExternalPaymentPurposeRenewal, TargetID: fx.svc.ID,
		AmountCents: 1000, Currency: "CNY", Subject: "renew",
		Status: model.ExternalPaymentPending,
	}
	if err := fx.db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := fx.service.Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID}); err == nil {
		t.Fatal("a live renewal payment intent must fence the change reservation")
	}
	var changes []model.ServiceChange
	fx.db.Where("service_id = ?", fx.svc.ID).Find(&changes)
	if len(changes) != 0 {
		t.Fatalf("no ServiceChange row may be created behind the fence, got %d", len(changes))
	}
}

// MONEY-07 (reverse fence): once a change reservation exists, renewing the
// service (wallet path) must be rejected — the reservation owns the billing
// period until it settles.
func TestRenewalFencedByPendingChange(t *testing.T) {
	fx := newChangeFenceFixture(t, "fence-renew", time.Now().UTC().Add(20*24*time.Hour), 1000, 2000)
	// Create the reservation (target applies via the fake host).
	if _, err := fx.service.Change(context.Background(), fx.user.ID, fx.svc.ID, ChangeInput{ProductID: fx.target.ID}); err != nil {
		t.Fatalf("change should succeed without invoices: %v", err)
	}
	// Re-hold the service with an in-flight reservation: a successful change
	// clears change_pending_id (renewal is then legitimately allowed), so
	// simulate the uncertain/applying state the fence must guard.
	row := model.ServiceChange{}
	if err := fx.db.Where("service_id = ?", fx.svc.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := fx.db.Model(&model.Service{}).Where("id = ?", fx.svc.ID).
		Update("change_pending_id", row.ID).Error; err != nil {
		t.Fatal(err)
	}

	billing := NewBillingService(fx.db, NewWalletService(fx.db), nil)
	before := balanceOf(t, fx.db, fx.user.ID)
	if _, err := billing.Renew(fx.user.ID, fx.svc.ID); err == nil {
		t.Fatal("renewal must be rejected while a change reservation holds the service")
	}
	if got := balanceOf(t, fx.db, fx.user.ID); got != before {
		t.Fatalf("renewal behind the fence must not debit: %d -> %d", before, got)
	}
	// Invoice creation for renewal must also be fenced.
	if _, err := billing.CreateRenewalInvoice(fx.user.ID, fx.svc.ID); err == nil {
		t.Fatal("creating a renewal invoice must be rejected while a change reservation holds the service")
	}
}
