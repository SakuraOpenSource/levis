package plugin

import (
 "context"
 "fmt"
 "time"
 pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)
// BackupStream exposes bounded chunks without retaining the entire backup.
type BackupStream interface { Recv() (*pb.HostBackupChunk,error) }
func(m *Manager) HostOperation(ctx context.Context,id string,req *pb.HostOperationRequest)(*pb.HostOperationReply,error){
 inst,e:=m.get(id);if e!=nil{return nil,e};if !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT){return nil,ErrUnavailable}
 client,c:=inst.client();if client==nil{return nil,ErrUnavailable}
 var reply *pb.HostOperationReply
 e=c.call(ctx,2*time.Hour,func(ctx context.Context)error{var err error;reply,err=client.HostOperation(ctx,req);return err});if e!=nil{return nil,e}
 if reply==nil{return nil,fmt.Errorf("插件未返回结果")};return reply,nil
}
// The caller owns the lifetime context. Do not wrap stream creation in c.call:
// its deferred cancel would cancel the stream as soon as this method returns.
func(m *Manager) DownloadHostBackup(ctx context.Context,id string,req *pb.HostBackupRequest)(BackupStream,error){
 inst,e:=m.get(id);if e!=nil{return nil,e};if !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT){return nil,ErrUnavailable}
 client,_:=inst.client();if client==nil{return nil,ErrUnavailable};return client.DownloadHostBackup(ctx,req)
}
