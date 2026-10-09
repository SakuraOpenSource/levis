package service

import (
	"context"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"testing"
)

type featureFake struct {
	calls int
	req   *pb.HostOperationRequest
}

func (f *featureFake) HostOperation(_ context.Context, _ string, r *pb.HostOperationRequest) (*pb.HostOperationReply, error) {
	f.calls++
	f.req = r
	return &pb.HostOperationReply{DataJson: `{"items":[]}`}, nil
}
func (f *featureFake) DownloadHostBackup(context.Context, string, *pb.HostBackupRequest) (plugin.BackupStream, error) {
	return nil, nil
}
func TestHostOperationOwnershipWhitelistAndPayload(t *testing.T) {
	db := newTestDB(t)
	u := seedUser(t, db, "featureowner", 0)
	svc := seedService(t, db, u.ID, "vm", 1000)
	db.Model(svc).Updates(map[string]any{"upstream_plugin_id": "provider", "upstream_host_id": "7"})
	f := &featureFake{}
	s := NewHostFeatureService(db, f)
	if _, e := s.Operation(context.Background(), u.ID+1, svc.ID, false, "GET", "snapshots", nil); e == nil {
		t.Fatal("cross owner allowed")
	}
	if _, e := s.Operation(context.Background(), u.ID, svc.ID, false, "POST", "purge", nil); e == nil {
		t.Fatal("customer purge allowed")
	}
	if _, e := s.Operation(context.Background(), u.ID, svc.ID, false, "POST", "snapshot_create", []byte(`{"name":"n","host_id":"8"}`)); e == nil {
		t.Fatal("payload host injection")
	}
	if _, e := s.Operation(context.Background(), u.ID, svc.ID, false, "POST", "snapshots", nil); e == nil {
		t.Fatal("wrong method")
	}
	if _, e := s.Operation(context.Background(), u.ID, svc.ID, false, "GET", "snapshots", nil); e != nil {
		t.Fatal(e)
	}
	if f.calls != 1 || f.req.HostId != "7" || f.req.Action != "snapshot_list" {
		t.Fatal("unsafe dispatch")
	}
	_ = model.ServiceActive
}
