package service

import (
 "context"
 "errors"
 "fmt"
 "math"
 "strconv"
 "time"
 "github.com/SakuraOpenSource/levis/internal/model"
 pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
 "gorm.io/gorm"
 "google.golang.org/grpc/codes"
 "google.golang.org/grpc/status"
)

type changeHost interface {
 GetHost(context.Context,string,*pb.GetHostRequest)(*pb.GetHostReply,error)
 ManageHost(context.Context,string,*pb.ManageHostRequest)(*pb.ManageHostReply,error)
}
type ServiceChangeService struct {db *gorm.DB;host changeHost}
func NewServiceChangeService(db *gorm.DB,host changeHost)*ServiceChangeService{return &ServiceChangeService{db:db,host:host}}
type ChangeInput struct {ProductID uint `json:"product_id"`;Options map[string]string `json:"options"`;IdempotencyKey string `json:"idempotency_key"`}
type ChangeQuote struct {
 ProductID uint `json:"product_id"`
 ChargeCents int64 `json:"charge_cents"`
 CreditCents int64 `json:"credit_cents"`
 RemainingSeconds int64 `json:"remaining_seconds"`
 TotalSeconds int64 `json:"total_seconds"`
 PriceCents int64 `json:"price_cents"`
 Options model.OptionMap `json:"options"`
}
type ChangeResult struct {Change *model.ServiceChange `json:"change"`;Service *model.Service `json:"service"`}
func(s *ServiceChangeService) owned(userID,id uint)(*model.Service,error){return NewBillingService(s.db,nil,nil).Service(userID,id)}
func(s *ServiceChangeService) remote(ctx context.Context,svc *model.Service)(*pb.UpstreamHost,error){
 if s.host==nil || svc.UpstreamHostID=="" || svc.UpstreamPluginID=="" {return nil,ErrBadRequest("该服务不支持改配")}
 cfg,e:=interfaceConfigForService(s.db,svc);if e!=nil{return nil,e}
 reply,e:=s.host.GetHost(ctx,svc.UpstreamPluginID,&pb.GetHostRequest{HostId:svc.UpstreamHostID,InterfaceConfig:cfg});if e!=nil{return nil,e}
 if reply==nil || reply.Host==nil || reply.Host.Resources==nil {return nil,ErrConflict("上游未提供可验证规格")}
 if reply.Host.Id!=svc.UpstreamHostID {return nil,ErrConflict("上游主机标识不匹配")}
 return reply.Host,nil
}
func resizeSupported(h *pb.UpstreamHost)bool{for _,a:=range h.Actions {if a=="resize"{return true}};return false}
func targetEligible(db *gorm.DB,svc *model.Service,current,target *model.Product,h *pb.UpstreamHost)error{
 if target.Status!=model.ProductActive || target.BillingCyc!=svc.BillingCyc || current.InterfaceID!=target.InterfaceID || current.ProvisionConfig.Driver=="" || current.ProvisionConfig.Driver!=target.ProvisionConfig.Driver {return ErrBadRequest("商品接口、驱动或计费周期不兼容")}
 id,_,e:=resolvePluginForProduct(db,target);if e!=nil{return e};if id!=svc.UpstreamPluginID {return ErrBadRequest("不能切换上游供应商")}
 if target.ProvisionConfig.AgentID!=0 && target.ProvisionConfig.AgentID!=current.ProvisionConfig.AgentID {return ErrBadRequest("不能改配到其他节点")}
 if !resizeSupported(h){return ErrBadRequest("上游不支持规格更新")};return nil
}
func periodStart(expiry time.Time,cycle string)(time.Time,error){
 months:=0;switch cycle {case model.CycleMonthly:months=1;case model.CycleQuarterly:months=3;case model.CycleSemiannually:months=6;case model.CycleAnnually:months=12;default:return time.Time{},ErrBadRequest("该服务不可按周期改配")}
 return expiry.AddDate(0,-months,0),nil
}
func resourceOptions(r *pb.HostResources)model.OptionMap{
 cpu:=float64(r.Cpu);if r.CpuMilli>0 {cpu=float64(r.CpuMilli)/1000}
 return model.OptionMap{"cpu":formatSpecNumber(cpu),"memory_mb":strconv.FormatInt(r.MemoryMb,10),"disk_gb":strconv.FormatInt(r.DiskGb,10),"bandwidth_mbps":strconv.FormatInt(r.BandwidthMbps,10),"traffic_gb":strconv.FormatInt(r.TrafficGb,10)}
}
func optionResources(o map[string]string)*pb.HostResources{
 cpu,_:=strconv.ParseFloat(o["cpu"],64);get:=func(k string)int64{v,_:=strconv.ParseInt(o[k],10,64);return v}
 r:=&pb.HostResources{Cpu:int32(math.Ceil(cpu)),MemoryMb:get("memory_mb"),DiskGb:get("disk_gb"),BandwidthMbps:get("bandwidth_mbps"),TrafficGb:get("traffic_gb")}
 if cpu!=math.Trunc(cpu){r.CpuMilli=int32(math.Round(cpu*1000))};return r
}
func(s *ServiceChangeService) quote(svc *model.Service,target *model.Product,h *pb.UpstreamHost,in ChangeInput,now time.Time)(*ChangeQuote,error){
 if svc.Status!=model.ServiceActive || svc.ExpiresAt==nil || !svc.ExpiresAt.After(now) {return nil,ErrConflict("仅未到期的在用服务可改配")}
 start,e:=periodStart(*svc.ExpiresAt,svc.BillingCyc);if e!=nil{return nil,e}
 cfg:=target.ProvisionConfig;o:=defaultProvisionOptions(cfg);o["bandwidth_mbps"]=formatSpecNumber(specValue(cfg.BandwidthMbps));o["traffic_gb"]=formatSpecNumber(specValue(cfg.TrafficGB));o["image_id"]="preserve"
 for k,v:=range in.Options {switch k {case "cpu","memory_mb","disk_gb","bandwidth_mbps","traffic_gb":o[k]=v;default:return nil,ErrBadRequest("改配不能修改系统、驱动、节点或网络身份")}}
 for _,k:=range []string{"cpu","memory_mb","disk_gb","bandwidth_mbps","traffic_gb"}{v,e:=strconv.ParseFloat(o[k],64);if e!=nil || math.IsNaN(v) || math.IsInf(v,0){return nil,ErrBadRequest("规格格式无效")}}
 if e=validateProvisionOptions(cfg,o);e!=nil{return nil,e}
 r:=optionResources(o);if r.Cpu<=0 || r.MemoryMb<=0 || r.DiskGb<h.Resources.DiskGb {return nil,ErrBadRequest("资源无效或磁盘不可缩小")}
 if target.PriceCents<0 || svc.PriceCents<0 {return nil,ErrBadRequest("价格无效")}
 extra:=provisionOptionPrice(cfg,o);if extra<0 || target.PriceCents>math.MaxInt64-extra{return nil,ErrBadRequest("选配价格溢出")}
 price:=target.PriceCents+extra;total:=int64(svc.ExpiresAt.Sub(start)/time.Second);remaining:=int64(svc.ExpiresAt.Sub(now)/time.Second);if remaining>total {remaining=total};if total<=0{return nil,ErrBadRequest("周期无效")}
 diff:=price-svc.PriceCents;charge,credit:=int64(0),int64(0);if diff>=0{charge,e=mulDivCents(diff,remaining,total)}else{credit,e=mulDivCents(-diff,remaining,total)};if e!=nil{return nil,e}
 delete(o,"image_id");delete(o,"agent_id");delete(o,"max_nat_mappings")
 return &ChangeQuote{ProductID:target.ID,ChargeCents:charge,CreditCents:credit,RemainingSeconds:remaining,TotalSeconds:total,PriceCents:price,Options:model.OptionMap(o)},nil
}
func(s *ServiceChangeService) Preview(ctx context.Context,userID,id uint,in ChangeInput)(*ChangeQuote,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e};h,e:=s.remote(ctx,svc);if e!=nil{return nil,e}
 var current,target model.Product;if e=s.db.First(&current,svc.ProductID).Error;e!=nil{return nil,e};if e=s.db.First(&target,in.ProductID).Error;e!=nil{return nil,ErrNotFound("商品不存在")}
 if e=targetEligible(s.db,svc,&current,&target,h);e!=nil{return nil,e};return s.quote(svc,&target,h,in,time.Now().UTC())
}
func(s *ServiceChangeService) Options(ctx context.Context,userID,id uint)([]model.Product,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e};h,e:=s.remote(ctx,svc);if e!=nil{return nil,e};var current model.Product;if e=s.db.First(&current,svc.ProductID).Error;e!=nil{return nil,e}
 var all []model.Product;if e=s.db.Where("status = ? AND billing_cycle = ?",model.ProductActive,svc.BillingCyc).Find(&all).Error;e!=nil{return nil,e};out:=[]model.Product{}
 for _,p:=range all {if targetEligible(s.db,svc,&current,&p,h)==nil {if _,e=s.quote(svc,&p,h,ChangeInput{},time.Now().UTC());e==nil {out=append(out,p)}}};return out,nil
}
func(s *ServiceChangeService) result(userID,id,changeID uint)(*ChangeResult,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e};var row model.ServiceChange;if e=s.db.First(&row,"id = ? AND service_id = ? AND user_id = ?",changeID,id,userID).Error;e!=nil{return nil,ErrNotFound("改配记录不存在")};return &ChangeResult{&row,svc},nil
}
func(s *ServiceChangeService) Change(ctx context.Context,userID,id uint,in ChangeInput)(*ChangeResult,error){
 svc,e:=s.owned(userID,id);if e!=nil{return nil,e}
 if len(in.IdempotencyKey)>128{return nil,ErrBadRequest("幂等键过长")}
 if in.IdempotencyKey!="" {var old model.ServiceChange;e=s.db.First(&old,"service_id = ? AND idempotency_key = ?",id,in.IdempotencyKey).Error;if e==nil {if old.ProductID!=in.ProductID{return nil,ErrConflict("幂等键已用于其他商品")};return s.result(userID,id,old.ID)};if !errors.Is(e,gorm.ErrRecordNotFound){return nil,e}}
 h,e:=s.remote(ctx,svc);if e!=nil{return nil,e};if h.Status!="stopped" && h.Status!="off" {return nil,ErrConflict("请先关闭上游实例")}
 q,e:=s.Preview(ctx,userID,id,in);if e!=nil{return nil,e};operation,e:=serialNo("CHG");if e!=nil{return nil,e};if in.IdempotencyKey==""{in.IdempotencyKey=operation}
 row:=model.ServiceChange{UserID:userID,ServiceID:id,ProductID:in.ProductID,PreviousProductID:svc.ProductID,IdempotencyKey:in.IdempotencyKey,OperationID:operation,Status:"reserved",ChargeCents:q.ChargeCents,CreditCents:q.CreditCents,PriceCents:q.PriceCents,Options:q.Options,PreviousResources:resourceOptions(h.Resources)}
 e=s.db.Transaction(func(tx *gorm.DB)error{
  if e:=tx.Create(&row).Error;e!=nil{return e}
  claim:=tx.Model(&model.Service{}).Where("id = ? AND user_id = ? AND change_pending_id IS NULL AND product_id = ? AND status = ? AND expires_at = ?",id,userID,svc.ProductID,svc.Status,*svc.ExpiresAt).Update("change_pending_id",row.ID)
  if claim.Error!=nil{return claim.Error};if claim.RowsAffected!=1{return ErrConflict("服务已有改配或状态已变更")}
  if row.ChargeCents>0{_,e:=NewWalletService(tx).adjustBalance(tx,userID,-row.ChargeCents,model.TxPayment,"service_change",row.ID,"规格更新预留");return e};return nil
 });if e!=nil{return nil,e}
 return s.Retry(ctx,userID,id,row.ID)
}
func(s *ServiceChangeService) Retry(ctx context.Context,userID,id,changeID uint)(*ChangeResult,error){
 out,e:=s.result(userID,id,changeID);if e!=nil{return nil,e};row,svc:=out.Change,out.Service
 if row.Status=="applied" || row.Status=="failed" {return out,nil}
 if svc.ChangePendingID==nil || *svc.ChangePendingID!=row.ID {return nil,ErrConflict("服务预留已变化")}
 if row.Status=="applying" && time.Since(row.UpdatedAt)<2*time.Hour+time.Minute {return nil,ErrConflict("改配正在处理")}
 oldStatus:=row.Status
 claim:=s.db.Model(&model.ServiceChange{}).Where("id = ? AND status = ? AND updated_at = ?",row.ID,row.Status,row.UpdatedAt).Update("status","applying");if claim.Error!=nil{return nil,claim.Error};if claim.RowsAffected!=1{return nil,ErrConflict("改配正在处理")}
 uncertain:=func(cause error)(*ChangeResult,error){
  result:=s.db.Model(&model.ServiceChange{}).Where("id = ? AND status = ?",row.ID,"applying").Updates(map[string]any{"status":"uncertain","error":truncateProvisionError(cause.Error())})
  if result.Error!=nil{return out,fmt.Errorf("%w; 持久化恢复状态失败: %v",cause,result.Error)}
  return s.result(userID,id,row.ID)
 }
 // Always observe before retrying. A remote commit + lost reply/DB rollback
 // is recovered without sending another resize or releasing the debit.
 h,e:=s.remote(ctx,svc);if e!=nil{return uncertain(e)}
 desired:=optionResources(row.Options)
 if resourcesEqual(h.Resources,desired) {if e=s.finishChange(row,true,"");e!=nil{_,_=uncertain(e);return out,e};return s.result(userID,id,row.ID)}
 if !resourcesEqual(h.Resources,optionResources(row.PreviousResources)) {return uncertain(fmt.Errorf("上游规格部分变更或漂移，需人工核对"))}
 if h.Status!="stopped" && h.Status!="off" {if oldStatus=="reserved" {e=s.finishChange(row,false,"请先关闭实例");if e!=nil{return uncertain(e)};return s.result(userID,id,row.ID)};return uncertain(fmt.Errorf("上游实例未停止，无法安全重试"))}
 cfg,e:=interfaceConfigForService(s.db,svc);if e!=nil{return uncertain(e)}
 rctx,cancel:=context.WithTimeout(ctx,2*time.Hour);defer cancel()
 reply,e:=s.host.ManageHost(rctx,svc.UpstreamPluginID,&pb.ManageHostRequest{HostId:svc.UpstreamHostID,Action:pb.HostAction_HOST_ACTION_RESIZE,Resources:desired,OperationId:row.OperationID,InterfaceConfig:cfg})
 // Only an explicit pre-mutation protocol rejection is definitive. A plugin
 // string error can also represent a timeout or upstream DB failure.
 if e!=nil && (status.Code(e)==codes.InvalidArgument || status.Code(e)==codes.Unimplemented) {
  if e=s.finishChange(row,false,e.Error());e!=nil{return uncertain(e)};return s.result(userID,id,row.ID)
 }
 if e!=nil{return uncertain(e)}
 if reply==nil || !reply.Success {msg:="上游结果不明确";if reply!=nil && reply.Error!=""{msg=reply.Error};return uncertain(fmt.Errorf("%s",msg))}
 h,e=s.remote(rctx,svc);if e!=nil{return uncertain(e)};if !resourcesEqual(h.Resources,desired){return uncertain(fmt.Errorf("上游成功回复与实际规格不一致"))}
 if e=s.finishChange(row,true,"");e!=nil{_,_=uncertain(e);return out,e};return s.result(userID,id,row.ID)
}

func resourcesEqual(a,b *pb.HostResources)bool{
 if a==nil || b==nil{return false};cpu:=func(r *pb.HostResources)int32{if r.CpuMilli>0{return r.CpuMilli};return r.Cpu*1000}
 return cpu(a)==cpu(b) && a.MemoryMb==b.MemoryMb && a.DiskGb==b.DiskGb && a.BandwidthMbps==b.BandwidthMbps && a.TrafficGb==b.TrafficGb
}
func(s *ServiceChangeService) finishChange(row *model.ServiceChange,applied bool,msg string)error{
 return s.db.Transaction(func(tx *gorm.DB)error{
  state:="failed";if applied{state="applied"}
  c:=tx.Model(&model.ServiceChange{}).Where("id = ? AND status = ?",row.ID,"applying").Updates(map[string]any{"status":state,"error":truncateProvisionError(msg)});if c.Error!=nil{return c.Error};if c.RowsAffected!=1{return ErrConflict("改配状态已变化")}
  updates:=map[string]any{"change_pending_id":nil};if applied {updates["product_id"]=row.ProductID;updates["price_cents"]=row.PriceCents;updates["provision_options"]=row.Options}
  c=tx.Model(&model.Service{}).Where("id = ? AND change_pending_id = ?",row.ServiceID,row.ID).Updates(updates);if c.Error!=nil{return c.Error};if c.RowsAffected!=1{return ErrConflict("服务预留已变化")}
  credit:=row.ChargeCents;note:="规格更新拒绝退回预留";if applied{credit=row.CreditCents;note="规格降级差价"}
  if credit>0 {_,e:=NewWalletService(tx).adjustBalance(tx,row.UserID,credit,model.TxRefund,"service_change",row.ID,note);return e};return nil
 })
}
func(s *ServiceChangeService) List(userID,id uint)([]model.ServiceChange,error){if _,e:=s.owned(userID,id);e!=nil{return nil,e};rows:=[]model.ServiceChange{};e:=s.db.Where("service_id = ? AND user_id = ?",id,userID).Order("id DESC").Limit(100).Find(&rows).Error;return rows,e}
