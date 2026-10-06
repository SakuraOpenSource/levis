package server

import (
	"github.com/SakuraOpenSource/levis/internal/model"
	"testing"
)

func TestServiceAutoRenewToggleOwnership(t *testing.T) {
	rt, h, admin, users := installedWithUsers(t, "renewowner", "renewother")
	id := userIDByName(t, h, admin, "renewowner")
	svc := model.Service{UserID: id, Name: "monthly", Status: model.ServiceActive, BillingCyc: model.CycleMonthly}
	if err := rt.DB().Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/services/" + itoa(svc.ID) + "/auto-renew"
	if r := doAs(t, h, "PATCH", path, map[string]bool{"auto_renew": true}, users["renewother"]); r.Code != 404 {
		t.Fatalf("ownership=%d", r.Code)
	}
	r := doAs(t, h, "PATCH", path, map[string]bool{"auto_renew": true}, users["renewowner"])
	if r.Code != 200 {
		t.Fatalf("toggle=%d %s", r.Code, r.Body.String())
	}
	var out struct {
		AutoRenew bool `json:"auto_renew"`
	}
	decodeJSON(t, r, &out)
	if !out.AutoRenew {
		t.Fatal("not enabled")
	}
	if r := doAs(t, h, "PATCH", path, map[string]string{}, users["renewowner"]); r.Code != 400 {
		t.Fatalf("missing flag=%d", r.Code)
	}
	r = doAs(t, h, "PATCH", path, map[string]bool{"auto_renew": false}, users["renewowner"])
	decodeJSON(t, r, &out)
	if out.AutoRenew {
		t.Fatal("false was ignored")
	}
}
