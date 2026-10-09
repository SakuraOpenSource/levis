package service

import (
	"context"
	"errors"
	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"testing"
)

func TestChangeDefinitiveRejectionRefundsOnce(t *testing.T) {
	db, u, svc, target, f := changeFixture(t)
	f.reject = true
	s := NewServiceChangeService(db, f)
	out, e := s.Change(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID})
	if e != nil {
		t.Fatal(e)
	}
	if out.Change.Status != "failed" || balanceOf(t, db, u.ID) != 10000 || out.Service.ChangePendingID != nil {
		t.Fatal("definitive rejection did not compensate", out.Change.Status)
	}
	s.Retry(context.Background(), u.ID, svc.ID, out.Change.ID)
	if balanceOf(t, db, u.ID) != 10000 {
		t.Fatal("refund duplicated")
	}
}
func TestChangeAmbiguousRemoteSuccessReconciles(t *testing.T) {
	db, u, svc, target, f := changeFixture(t)
	f.err = errors.New("timeout after apply")
	s := NewServiceChangeService(db, f)
	out, e := s.Change(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID})
	if e != nil {
		t.Fatal(e)
	}
	if out.Change.Status != "uncertain" {
		t.Fatal("ambiguous state lost")
	}
	reserved := balanceOf(t, db, u.ID)
	f.err = nil
	f.host.Resources = f.req.Resources
	out, e = s.Retry(context.Background(), u.ID, svc.ID, out.Change.ID)
	if e != nil {
		t.Fatal(e)
	}
	if f.calls != 1 || out.Change.Status != "applied" || balanceOf(t, db, u.ID) != reserved {
		t.Fatal("reconcile resent or charged twice")
	}
}
func TestChangeDBFailureAfterRemoteSuccessRecoverable(t *testing.T) {
	db, u, svc, target, f := changeFixture(t)
	s := NewServiceChangeService(db, f)
	db.Exec("CREATE TRIGGER reject_change BEFORE UPDATE OF product_id ON services BEGIN SELECT RAISE(FAIL, 'injected write failure'); END")
	out, e := s.Change(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID})
	if e == nil {
		t.Fatal("DB failure hidden")
	}
	var row model.ServiceChange
	db.First(&row)
	if row.Status != "uncertain" {
		t.Fatal("DB failure stranded applying", row.Status)
	}
	db.Exec("DROP TRIGGER reject_change")
	out, e = s.Retry(context.Background(), u.ID, svc.ID, row.ID)
	if e != nil {
		t.Fatal(e)
	}
	if out.Change.Status != "applied" || f.calls != 1 {
		t.Fatal("DB recovery resent remote")
	}
}
func TestChangeNoDiskShrinkDriverChangeOrCrossOwner(t *testing.T) {
	db, u, svc, target, f := changeFixture(t)
	s := NewServiceChangeService(db, f)
	if _, e := s.Preview(context.Background(), u.ID+1, svc.ID, ChangeInput{ProductID: target.ID}); e == nil {
		t.Fatal("cross owner allowed")
	}
	db.Model(target).Update("provision_config", model.ProvisionSpec{Driver: "incus"})
	if _, e := s.Preview(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID}); e == nil {
		t.Fatal("driver change allowed")
	}
	db.Model(target).Update("provision_config", model.ProvisionSpec{Driver: "qemu", Mode: "fixed", CPU: model.SpecRange{Min: 4, Max: 4}, MemoryMB: model.SpecRange{Min: 1024, Max: 1024}, DiskGB: model.SpecRange{Min: 10, Max: 10}})
	if _, e := s.Preview(context.Background(), u.ID, svc.ID, ChangeInput{ProductID: target.ID}); e == nil {
		t.Fatal("disk shrink allowed")
	}
	_ = &pb.HostResources{}
}
