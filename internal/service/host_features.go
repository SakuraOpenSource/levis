package service

import (
 "context"
 "encoding/json"
 "fmt"
 "strings"
 "github.com/SakuraOpenSource/levis/internal/model"
 "github.com/SakuraOpenSource/levis/internal/plugin"
 pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
 "gorm.io/gorm"
)
type featureHost interface {
 HostOperation(context.Context,string,*pb.HostOperationRequest)(*pb.HostOperationReply,error)
 DownloadHostBackup(context.Context,string,*pb.HostBackupRequest)(plugin.BackupStream,error)
}
type HostFeatureService struct {db *gorm.DB;host featureHost}
func NewHostFeatureService(db *gorm.DB,host featureHost)*HostFeatureService{return &HostFeatureService{db,host}}
type featureRule struct {rpc,method,keys string;admin bool}
var featureRules=map[string]featureRule{
 "snapshots":{"snapshot_list","GET","",false},"backups":{"backup_list","GET","",false},"firewall":{"firewall_list","GET","",false},
 "snapshot_create":{"snapshot_create","POST","name remark",false},"backup_create":{"backup_create","POST","name remark",false},
 "snapshot_restore":{"snapshot_restore","POST","snapshot_id",false},"snapshot_delete":{"snapshot_delete","POST","snapshot_id",false},
 "backup_restore":{"backup_restore","POST","backup_id",false},"backup_delete":{"backup_delete","POST","backup_id",false},
 "firewall_create":{"firewall_create","POST","direction action protocol port_start port_end cidr priority enabled remark",false},
 "firewall_update":{"firewall_update","POST","rule_id direction action protocol port_start port_end cidr priority enabled remark",false},
 "firewall_delete":{"firewall_delete","POST","rule_id",false},
 "vpcs":{"vpc_list","GET","agent_id",true},"ip_pool":{"ip_pool_free","GET","agent_id",true},
 "migrate":{"migrate","POST","target_agent_id vpc_id ip_pool_entry_id network",true},"trash_restore":{"trash_restore","POST","",true},"purge":{"trash_purge","POST","",true},
}
func(s *HostFeatureService) owned(userID,id uint)(*model.Service,error){return NewBillingService(s.db,nil,nil).Service(userID,id)}
func(s *HostFeatureService) Operation(ctx context.Context,userID,id uint,admin bool,method,action string,payload []byte)(json.RawMessage,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e}
 rule,ok:=featureRules[action];if !ok{return nil,ErrBadRequest("不支持的 provider action")};if rule.admin && !admin{return nil,ErrForbidden("需要管理员权限")};if method!=rule.method{return nil,ErrBadRequest("操作请求方法不正确")}
 var fields map[string]json.RawMessage;if len(payload)==0 {payload=[]byte("{}")};if len(payload)>64*1024 || json.Unmarshal(payload,&fields)!=nil || fields==nil{return nil,ErrBadRequest("操作参数必须为 JSON 对象")}
 keys:=map[string]bool{};for _,key:=range strings.Fields(rule.keys){keys[key]=true};for key:=range fields {if !keys[key]{return nil,ErrBadRequest("不允许的操作参数: %s",key)}}
 if rule.method=="POST" && svc.ChangePendingID!=nil{return nil,ErrConflict("规格更新进行中，请先完成对账")}
 if svc.Status!=model.ServiceActive && !rule.admin{return nil,ErrConflict("仅在用服务可以执行上游操作")}
 if s.host==nil || svc.UpstreamPluginID=="" || svc.UpstreamHostID==""{return nil,ErrBadRequest("该服务不支持上游功能")}
 config,e:=interfaceConfigForService(s.db,svc);if e!=nil{return nil,e}
 reply,e:=s.host.HostOperation(ctx,svc.UpstreamPluginID,&pb.HostOperationRequest{HostId:svc.UpstreamHostID,Action:rule.rpc,PayloadJson:string(payload),InterfaceConfig:config});if e!=nil{return nil,e};if reply==nil{return nil,fmt.Errorf("上游未返回操作结果")};if reply.Error!=""{return nil,ErrConflict("上游操作失败: %s",truncateProvisionError(reply.Error))}
 raw:=json.RawMessage(reply.DataJson);if len(raw)==0{raw=json.RawMessage("{}")};if len(raw)>4*1024*1024 || !json.Valid(raw){return nil,fmt.Errorf("上游返回无效 JSON")};return raw,nil
}
func(s *HostFeatureService) Download(ctx context.Context,userID,id uint,backupID uint64)(plugin.BackupStream,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e};if backupID==0{return nil,ErrBadRequest("备份 ID 无效")}
 if s.host==nil || svc.UpstreamPluginID=="" || svc.UpstreamHostID==""{return nil,ErrBadRequest("该服务不支持备份下载")}
 config,e:=interfaceConfigForService(s.db,svc);if e!=nil{return nil,e}
 return s.host.DownloadHostBackup(ctx,svc.UpstreamPluginID,&pb.HostBackupRequest{HostId:svc.UpstreamHostID,BackupId:backupID,InterfaceConfig:config})
}
