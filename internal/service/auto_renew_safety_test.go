package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"sync"
	"testing"
	"time"
)

func TestAutoRenewDryRunNoWrites(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "dryrenew", 3000)
	svc := seedService(t, db, u.ID, "renew", 1000)
	expiry := time.Now().UTC().Add(time.Hour)
	db.Model(svc).Updates(map[string]any{"auto_renew": true, "expires_at": expiry})
	db.Create(&model.Setting{Key: model.SettingLifecycleDryRun, Value: "1"})
	newLifecycleServiceForTest(db, nil, func() bool { return false }).Run(context.Background())
	if balanceOf(t, db, u.ID) != 3000 {
		t.Fatal("dry run debited")
	}
	var n int64
	db.Model(&model.RenewalEvent{}).Count(&n)
	if n != 0 {
		t.Fatal("dry run wrote ledger")
	}
}
func TestAutoRenewConcurrentAndUnpaidExcluded(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "concurrentrenew", 5000)
	svc := seedService(t, db, u.ID, "renew", 1000)
	expiry := time.Now().UTC().Add(time.Hour)
	db.Model(svc).Updates(map[string]any{"auto_renew": true, "expires_at": expiry})
	invoice := model.Invoice{InvoiceNo: "existing", UserID: u.ID, ServiceID: &svc.ID, Status: model.InvoiceUnpaid, TotalCents: 1000}
	db.Create(&invoice)
	lc := newLifecycleServiceForTest(db, nil, func() bool { return false })
	lc.autoRenew(context.Background())
	if balanceOf(t, db, u.ID) != 5000 {
		t.Fatal("unpaid invoice ignored")
	}
	db.Model(&invoice).Update("status", model.InvoiceCancelled)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); lc.autoRenew(context.Background()) }()
	}
	wg.Wait()
	if balanceOf(t, db, u.ID) != 4000 {
		t.Fatal("concurrent renewal duplicated")
	}
}
