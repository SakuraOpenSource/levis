package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"testing"
	"time"
)

func TestLifecycleExpiryMixedTimezones(t *testing.T) {
	db := newTestDB(t)
	future := time.Now().UTC().Add(time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceActive, &future, 0, "")
	lc := newLifecycleServiceForTest(db, nil, func() bool { return true })
	lc.Run(context.Background())
	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceActive {
		t.Fatal("future UTC expiry suspended", got.Status)
	}
}
