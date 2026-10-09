package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
	"testing"
	"time"
)

type resizeFake struct {
	host   *pb.UpstreamHost
	calls  int
	req    *pb.ManageHostRequest
	reject bool
	err    error
}

func (f *resizeFake) GetHost(context.Context, string, *pb.GetHostRequest) (*pb.GetHostReply, error) {
	return &pb.GetHostReply{Host: f.host}, nil
}
func (f *resizeFake) ManageHost(_ context.Context, _ string, r *pb.ManageHostRequest) (*pb.ManageHostReply, error) {
	f.calls++
	f.req = r
	if f.err != nil {
		return nil, f.err
	}
	if f.reject {
		return nil, status.Error(codes.InvalidArgument, "rejected before mutation")
	}
	f.host.Resources = r.Resources
	return &pb.ManageHostReply{Success: true}, nil
}
func changeFixture(t *testing.T) (*gorm.DB, *model.User, *model.Service, *model.Product, *resizeFake) {
	db := newTestDB(t)
	u := seedUser(t, db, "resizebuyer", 10000)
	cfg := model.ProvisionSpec{Driver: "qemu", Mode: "fixed", CPU: model.SpecRange{Min: 2, Max: 2}, MemoryMB: model.SpecRange{Min: 1024, Max: 1024}, DiskGB: model.SpecRange{Min: 20, Max: 20}}
	p := model.Product{Name: "old", PriceCents: 1000, BillingCyc: model.CycleMonthly, UpstreamPluginID: "provider", ProvisionConfig: cfg}
	db.Create(&p)
	target := p
	target.Base = model.Base{}
	target.Name = "new"
	target.PriceCents = 2000
	target.ProvisionConfig.CPU = model.SpecRange{Min: 4, Max: 4}
	db.Create(&target)
	expiry := time.Now().UTC().Add(15 * 24 * time.Hour)
	svc := model.Service{UserID: u.ID, ProductID: p.ID, OrderID: 1, Name: "vm", Status: model.ServiceActive, BillingCyc: model.CycleMonthly, PriceCents: 1000, ExpiresAt: &expiry, UpstreamPluginID: "provider", UpstreamHostID: "vm-1"}
	db.Create(&svc)
	f := &resizeFake{host: &pb.UpstreamHost{Id: "vm-1", Status: "stopped", Actions: []string{"resize"}, Resources: &pb.HostResources{Cpu: 2, MemoryMb: 1024, DiskGb: 20}}}
	return db, u, &svc, &target, f
}
func TestChangeProductResizesAndIdempotent(t *testing.T) {
	db, u, svc, target, f := changeFixture(t)
	s := NewServiceChangeService(db, f)
	q, e := s.Preview(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID})
	if e != nil {
		t.Fatal(e)
	}
	if q.ChargeCents <= 0 || q.ChargeCents > 1000 {
		t.Fatalf("invalid proration %+v", q)
	}
	in := ChangeInput{ProductID: target.ID, IdempotencyKey: "repeat"}
	out, e := s.Change(context.Background(), u.ID, svc.ID, in)
	if e != nil {
		t.Fatal(e)
	}
	if out.Change.Status != "applied" || out.Service.ProductID != target.ID || f.req.Action != pb.HostAction_HOST_ACTION_RESIZE || f.req.OperationId == "" || f.req.Resources.Cpu != 4 {
		t.Fatalf("not a real resize %+v", out)
	}
	balance := balanceOf(t, db, u.ID)
	out, e = s.Change(context.Background(), u.ID, svc.ID, in)
	if e != nil {
		t.Fatal(e)
	}
	if f.calls != 1 || balanceOf(t, db, u.ID) != balance {
		t.Fatal("duplicate resize/payment")
	}
}
