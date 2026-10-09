package server

import (
	"github.com/SakuraOpenSource/levis/internal/model"
	"testing"
)

func TestFeatureRoutesOwnershipAndWhitelist(t *testing.T) {
	rt, h, admin, users := installedWithUsers(t, "featureowner", "featureother")
	u := userIDByName(t, h, admin, "featureowner")
	svc := model.Service{UserID: u, Name: "vm", Status: model.ServiceActive, BillingCyc: model.CycleMonthly}
	rt.DB().Create(&svc)
	base := "/api/services/" + itoa(svc.ID)
	for _, path := range []string{"/change-options", "/changes", "/features/snapshots", "/backups/1/download"} {
		if r := doAs(t, h, "GET", base+path, nil, users["featureother"]); r.Code != 404 {
			t.Fatalf("ownership %s: %d", path, r.Code)
		}
	}
	if r := doAs(t, h, "GET", base+"/changes", nil, users["featureowner"]); r.Code != 200 {
		t.Fatalf("changes missing %d", r.Code)
	}
	if r := doAs(t, h, "POST", base+"/features/arbitrary_url", map[string]string{"url": "https://evil"}, users["featureowner"]); r.Code != 400 {
		t.Fatalf("whitelist %d", r.Code)
	}
	if r := doAs(t, h, "POST", base+"/features/purge", map[string]string{}, users["featureowner"]); r.Code != 403 {
		t.Fatalf("customer purge %d", r.Code)
	}
}
