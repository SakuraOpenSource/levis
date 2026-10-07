package service

import (
	"context"
	"encoding/json"
	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"testing"
)

func networkPreset(t *testing.T) model.ProvisionSpec {
	t.Helper()
	var cfg model.ProvisionSpec
	if err := json.Unmarshal([]byte(`{"driver":"incus","mode":"fixed","cpu":{"min":1,"max":1},"memory_mb":{"min":512,"max":512},"disk_gb":{"min":10,"max":10},"bandwidth_mbps":{"min":50,"max":50},"traffic_gb":{"min":200,"max":200},"agent_id":3,"max_nat_mappings":4,"network_mode":"dedicated","dedicated_mode":"routed","network_bridge":"eth0","network_dns":["1.1.1.1"],"security_group_ids":[3,9]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProductFixedNetworkOptionsEnforcedInDirectAndCart(t *testing.T) {
	for _, path := range []string{"direct", "cart"} {
		t.Run(path, func(t *testing.T) {
			db := newTestDB(t)
			u := seedUser(t, db, "network-owner", 0)
			p := seedProduct(t, db, "network", 1000)
			p.ProvisionConfig = networkPreset(t)
			if path == "direct" {
				p.InterfaceID = 1
			} else {
				p.UpstreamPluginID = "provider"
				p.UpstreamProductID = "remote"
			}
			if err := db.Save(p).Error; err != nil {
				t.Fatal(err)
			}
			s := NewOrderService(db, NewCartService(db), nil, nil)
			var order *model.Order
			var err error
			if path == "direct" {
				order, err = s.CreateDirect(u.ID, []OrderLine{{ProductID: p.ID, Quantity: 1, Options: map[string]string{"cpu": "1", "memory_mb": "512", "disk_gb": "10", "bandwidth_mbps": "50", "traffic_gb": "200", "image_id": "4", "driver": "qemu", "agent_id": "99", "network_mode": "nat", "dedicated_mode": "bridge", "network_bridge": "evil", "security_group_ids": "99", "network_ipv4": "203.0.113.99", "ip_pool_entry_id": "99", "network_dns": "8.8.8.8", "max_nat_mappings": "0", "path": "/admin"}}})
			} else {
				if err = db.Create(&model.CartItem{UserID: u.ID, ProductID: p.ID, Quantity: 1, BillingCyc: model.CycleMonthly}).Error; err != nil {
					t.Fatal(err)
				}
				order, err = s.CreateFromCart(u.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			order, err = s.Get(u.ID, order.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := order.Items[0].Options
			for key, want := range map[string]string{"driver": "incus", "network_mode": "dedicated", "dedicated_mode": "routed", "network_bridge": "eth0", "network_dns": "1.1.1.1", "security_group_ids": "3,9", "agent_id": "3", "max_nat_mappings": "4"} {
				if got[key] != want {
					t.Errorf("%s=%q want %q", key, got[key], want)
				}
			}
			for _, key := range []string{"path", "ip_pool_entry_id", "network_ipv4"} {
				if _, ok := got[key]; ok {
					t.Errorf("buyer option survived: %s", key)
				}
			}
		})
	}
}

type provisionCapture struct{ req *pb.CreateOrderRequest }

func (f *provisionCapture) CreateOrder(_ context.Context, _ string, r *pb.CreateOrderRequest) (*pb.CreateOrderReply, error) {
	f.req = r
	return &pb.CreateOrderReply{UpstreamOrderId: "7"}, nil
}
func (f *provisionCapture) GetHost(context.Context, string, *pb.GetHostRequest) (*pb.GetHostReply, error) {
	return &pb.GetHostReply{}, nil
}

func TestProvisionBoundaryReassertsFixedDefaultsForHistoricalOrders(t *testing.T) {
	db := newTestDB(t)
	p := &model.Product{UpstreamPluginID: "provider", ProvisionConfig: networkPreset(t)}
	selected := map[string]string{"cpu": "1", "memory_mb": "512", "disk_gb": "10", "bandwidth_mbps": "50", "traffic_gb": "200", "image_id": "4", "network_mode": "nat", "security_group_ids": "99", "max_nat_mappings": "0", "ip_pool_entry_id": "11", "network_ipv4": "203.0.113.99", "agent_id": "99", "driver": "qemu"}
	f := &provisionCapture{}
	if _, _, err := createUpstreamOrderWithHost(f, db, p, model.CycleMonthly, "order", "buyer@example.test", selected); err != nil {
		t.Fatal(err)
	}
	for key, want := range defaultProvisionOptions(p.ProvisionConfig) {
		if f.req.Options[key] != want {
			t.Errorf("%s=%q want %q", key, f.req.Options[key], want)
		}
	}
	for _, key := range []string{"ip_pool_entry_id", "network_ipv4"} {
		if _, ok := f.req.Options[key]; ok {
			t.Errorf("unsafe historical option %s", key)
		}
	}
	if selected["network_mode"] != "nat" {
		t.Fatal("mutated caller snapshot")
	}
}

func TestProvisionBoundaryRejectsQuotaOverride(t *testing.T) {
	db := newTestDB(t)
	p := &model.Product{UpstreamPluginID: "provider", ProvisionConfig: networkPreset(t)}
	for _, key := range []string{"cpu", "traffic_gb", "bandwidth_mbps"} {
		options := defaultProvisionOptions(p.ProvisionConfig)
		options["image_id"] = "4"
		options[key] = "99999"
		f := &provisionCapture{}
		if _, _, err := createUpstreamOrderWithHost(f, db, p, model.CycleMonthly, "order", "buyer@example.test", options); err == nil || f.req != nil {
			t.Errorf("quota override reached provider: %s", key)
		}
	}
}

func TestNormalizeProductNetworkPreset(t *testing.T) {
	for _, patch := range []string{`{"network_mode":"evil"}`, `{"network_mode":"nat","dedicated_mode":"routed"}`, `{"dedicated_mode":"evil"}`, `{"network_bridge":"../eth0"}`, `{"network_dns":["bad"]}`, `{"security_group_ids":[0]}`, `{"security_group_ids":[3,3]}`, `{"security_group_ids":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]}`} {
		cfg := networkPreset(t)
		if err := json.Unmarshal([]byte(patch), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := normalizeProvisionConfig(&cfg); err == nil {
			t.Errorf("accepted %s", patch)
		}
	}
	cfg := networkPreset(t)
	cfg.NetworkMode = ""
	cfg.DedicatedMode = ""
	if err := normalizeProvisionConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkMode != "nat" {
		t.Errorf("default=%q", cfg.NetworkMode)
	}
}
