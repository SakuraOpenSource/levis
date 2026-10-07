package server

import (
	"github.com/SakuraOpenSource/levis/internal/model"
	"net/http"
	"testing"
)

func TestAdminInterfaceSecurityGroupsRouteAndAuthorization(t *testing.T) {
	rt, h, admin, users := installedWithUsers(t, "sg-buyer")
	iface := model.UpstreamInterface{Name: "Virtualis", PluginID: "virtualis"}
	if err := rt.DB().Create(&iface).Error; err != nil {
		t.Fatal(err)
	}
	const path = "/api/admin/interfaces/1/security-groups"
	if rec := doAs(t, h, http.MethodGet, path, nil, users["sg-buyer"]); rec.Code != http.StatusForbidden {
		t.Fatalf("buyer=%d %s", rec.Code, rec.Body.String())
	}
	// A configured interface but unavailable plugin is a business error, not SPA/404.
	if rec := doAs(t, h, http.MethodGet, path, nil, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("admin=%d %s", rec.Code, rec.Body.String())
	}
	if rec := doAs(t, h, http.MethodGet, "/api/admin/interfaces/999/security-groups", nil, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("missing=%d", rec.Code)
	}
}
