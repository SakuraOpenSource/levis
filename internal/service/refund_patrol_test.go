package service

import (
	"context"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

func TestRefundPatrolSuspendsRevokedUpstreamEntitlement(t *testing.T) {
	fx := newRefundFixture(t)
	service := model.Service{
		UserID: fx.userID, ProductID: 1, OrderID: fx.orderID, Name: "refunded upstream",
		Status: model.ServiceActive, BillingCyc: model.CycleMonthly,
		UpstreamPluginID: "provider", UpstreamHostID: "refund-host", AutoRenew: true,
	}
	if err := fx.db.Create(&service).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.SavePolicy(RefundPolicyInput{AutoApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Create(context.Background(), fx.userID, RefundCreateInput{PaymentID: fx.payID, Reason: "full refund"}); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	lc := newLifecycleServiceForTest(fx.db, host, func() bool { return false })
	lc.Run(context.Background())
	if !host.hasAction(service.UpstreamHostID, "HOST_ACTION_SUSPEND") {
		t.Fatal("patrol must suspend the refunded upstream entitlement, not only its local row")
	}
	if host.hasAction(service.UpstreamHostID, "HOST_ACTION_UNSUSPEND") {
		t.Fatal("refund revocation must never be treated as a pending resume")
	}
	before := len(host.actions)
	lc.Run(context.Background())
	if len(host.actions) != before {
		t.Fatal("a reconciled refund suspension must not dispatch again")
	}
}
