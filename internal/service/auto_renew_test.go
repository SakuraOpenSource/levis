package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"testing"
	"time"
)

func TestLifecycleAutoRenewAtomicAndRepeated(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "autorenewer", 3000)
	svc := seedService(t, db, user.ID, "autorenew", 1000)
	expiry := time.Now().UTC().Add(time.Hour)
	if err := db.Model(svc).Updates(map[string]any{"auto_renew": true, "expires_at": expiry, "next_due_at": expiry}).Error; err != nil {
		t.Fatal(err)
	}
	lc := newLifecycleServiceForTest(db, nil, func() bool { return false })
	lc.Run(context.Background())
	lc.Run(context.Background())
	var got model.Service
	db.First(&got, svc.ID)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(model.AdvanceCycle(expiry, model.CycleMonthly)) {
		t.Fatalf("expiry not extended atomically: %+v", got)
	}
	if balanceOf(t, db, user.ID) != 2000 {
		t.Fatal("duplicate or missing debit")
	}
	var invoices []model.Invoice
	db.Preload("Items").Find(&invoices)
	if len(invoices) != 1 || invoices[0].Status != model.InvoicePaid || invoices[0].ServiceID == nil || len(invoices[0].Items) != 1 {
		t.Fatalf("paid renewal invoices=%+v", invoices)
	}
	var n int64
	db.Model(&model.Transaction{}).Count(&n)
	if n != 1 {
		t.Fatalf("transactions=%d", n)
	}
}
