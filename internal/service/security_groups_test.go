package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"strings"
	"testing"
)

func TestSecurityGroupFeatureOwnershipAndAdminBinding(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "sg-owner", 0)
	svc := seedService(t, db, u.ID, "vm", 1000)
	if err := db.Model(svc).Updates(map[string]any{"upstream_plugin_id": "provider", "upstream_host_id": "7"}).Error; err != nil {
		t.Fatal(err)
	}
	f := &featureFake{}
	s := NewHostFeatureService(db, f)
	if _, err := s.Operation(context.Background(), u.ID, svc.ID, false, "GET", "security_groups", nil); err != nil {
		t.Fatal(err)
	}
	if f.req.Action != "security_groups_get" || f.req.HostId != "7" {
		t.Fatal("unsafe dispatch")
	}
	if _, err := s.Operation(context.Background(), u.ID+1, svc.ID, true, "GET", "security_groups", nil); err == nil {
		t.Fatal("cross-owner access")
	}
	if _, err := s.Operation(context.Background(), u.ID, svc.ID, false, "POST", "security_groups_set", []byte(`{"security_group_ids":[]}`)); err == nil {
		t.Fatal("buyer detached mandatory groups")
	}
	if _, err := s.Operation(context.Background(), u.ID, svc.ID, true, "POST", "security_groups_set", []byte(`{"security_group_ids":[3,9]}`)); err != nil {
		t.Fatal(err)
	}
	if f.req.Action != "security_groups_set" {
		t.Fatal("wrong admin action")
	}
	calls := f.calls
	for _, payload := range []string{`{}`, `{"security_group_ids":null}`, `{"security_group_ids":[0]}`, `{"security_group_ids":[3,3]}`, `{"security_group_ids":["3"]}`, `{"security_group_ids":[3],"security_group_ids":[9]}`, `{"security_group_ids":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]}`, `{"security_group_ids":[3],"path":"/admin"}`} {
		if _, err := s.Operation(context.Background(), u.ID, svc.ID, true, "POST", "security_groups_set", []byte(payload)); err == nil {
			t.Errorf("unsafe binding: %s", payload)
		}
	}
	if f.calls != calls {
		t.Fatal("unsafe binding reached plugin")
	}
}

type catalogFeatureFake struct {
	featureFake
	data string
}

func (f *catalogFeatureFake) HostOperation(_ context.Context, plugin string, r *pb.HostOperationRequest) (*pb.HostOperationReply, error) {
	f.req = r
	return &pb.HostOperationReply{DataJson: f.data}, nil
}
func TestInterfaceSecurityGroupChoicesAreSanitizedAndScoped(t *testing.T) {
	db := newTestDB(t)
	iface := model.UpstreamInterface{Name: "provider", PluginID: "virtualis", Config: model.OptionMap{"api_key": "catalog-key", "api_url": "https://provider.invalid"}}
	if err := db.Create(&iface).Error; err != nil {
		t.Fatal(err)
	}
	f := &catalogFeatureFake{data: `{"items":[{"id":3,"name":"web","description":"catalog-key","ingress_policy":"drop","egress_policy":"accept","shell":"secret","rules":[{"token":"secret"}]}],"api_key":"secret"}`}
	out, err := NewHostFeatureService(db, f).InterfaceSecurityGroups(context.Background(), iface.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "catalog-key") {
		t.Fatalf("leaked data: %s", out)
	}
	if f.req.HostId != "" || f.req.Action != "security_groups_list" || f.req.PayloadJson != "{}" || f.req.InterfaceConfig["api_key"] != "catalog-key" {
		t.Fatal("unsafe catalog dispatch")
	}
	f.data = `{"items":[{"id":0,"name":"web"}]}`
	if _, err := NewHostFeatureService(db, f).InterfaceSecurityGroups(context.Background(), iface.ID); err == nil {
		t.Fatal("malformed choice accepted")
	}
}

func TestServiceChangeQuoteDropsProductFixedNetworkOptions(t *testing.T) {
	cfg := networkPreset(t)
	o := defaultProvisionOptions(cfg)
	for _, k := range []string{"network_mode", "dedicated_mode", "network_bridge", "network_dns", "security_group_ids", "agent_id", "max_nat_mappings"} {
		if _, ok := o[k]; !ok || o[k] == "" {
			t.Fatalf("preset missing fixed key %s", k)
		}
	}
}

func TestSecurityGroupSetPreservesProductRequiredGroups(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "sg-required", 0)
	p := seedProduct(t, db, "sg-product", 1000)
	order := model.Order{UserID: u.ID, OrderNo: "SG-1", Status: model.OrderPaid}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	item := model.OrderItem{OrderID: order.ID, ProductID: p.ID, ProductName: p.Name, PriceCents: 1000, Quantity: 1, BillingCyc: model.CycleMonthly, Options: model.OptionMap{"security_group_ids": "3,9"}}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	svc := seedService(t, db, u.ID, "vm", 1000)
	if err := db.Model(svc).Updates(map[string]any{"upstream_plugin_id": "provider", "upstream_host_id": "7", "order_id": order.ID, "product_id": p.ID}).Error; err != nil {
		t.Fatal(err)
	}
	f := &featureFake{}
	s := NewHostFeatureService(db, f)
	if _, err := s.Operation(context.Background(), u.ID, svc.ID, true, "POST", "security_groups_set", []byte(`{"security_group_ids":[3,12]}`)); err == nil {
		t.Fatal("detached product-required group 9")
	}
	if _, err := s.Operation(context.Background(), u.ID, svc.ID, true, "POST", "security_groups_set", []byte(`{"security_group_ids":[3,9,12]}`)); err != nil {
		t.Fatal(err)
	}
}
