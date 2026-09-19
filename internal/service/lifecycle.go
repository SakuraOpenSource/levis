package service

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// 到期即刻停机，停机满宽限期后删除。手动停机不写 suspend_reason，因而不会
// 被自动删除；续费/加购结清后只恢复对应原因的自动停机。
const (
	expiryGrace       = 72 * time.Hour
	lifecycleInterval = 5 * time.Minute
	lifecycleTimeout  = 60 * time.Second
	bytesPerGiB       = int64(1024) * 1024 * 1024
)

// hostManager 是生命周期执法器依赖的上游操作面：拉取主机 metrics 与执行
// 电源/生命周期动作。生产实现是 *plugin.Manager；测试注入 fake，用来证明
// 每个「何时停机/删除/恢复」分支，而不必启动真实插件进程。
type hostManager interface {
	GetHostMetrics(ctx context.Context, pluginID string, req *pb.GetHostMetricsRequest) (*pb.GetHostMetricsReply, error)
	ManageHost(ctx context.Context, pluginID string, req *pb.ManageHostRequest) (*pb.ManageHostReply, error)
}

// LifecycleService 是服务生命周期后台执法器。
//
// 流量超限：采样上游 metrics，按差分累计写回 Levis；达到总配额后调用
// HOST_ACTION_SUSPEND（停机但不删除）。流量包结清后调用 UNSUSPEND。
// 产品到期：调用 SUSPEND；72 小时内未续费再调用 TERMINATE 并标记 terminated。
// TERMINATE 受站点设置 lifecycle_terminate_enabled 控制：默认关闭（干跑，
// 只记日志不删除），管理员确认停机清单后再开启。
// 每个动作幂等，单个服务失败不会阻塞同轮其它服务。
type LifecycleService struct {
	db   *gorm.DB
	host hostManager
	// terminateEnabled 每轮执法时读取一次开关；nil 视为关闭（干跑）。
	terminateEnabled func() bool
}

// NewLifecycleService 构造生命周期执法器。
func NewLifecycleService(db *gorm.DB, plugins *plugin.Manager) *LifecycleService {
	var host hostManager
	if plugins != nil {
		host = plugins
	}
	return &LifecycleService{db: db, host: host, terminateEnabled: terminateEnabledSetting(db)}
}

// newLifecycleServiceForTest 用注入的上游操作面构造执法器（测试专用）。
func newLifecycleServiceForTest(db *gorm.DB, host hostManager, terminateEnabled func() bool) *LifecycleService {
	return &LifecycleService{db: db, host: host, terminateEnabled: terminateEnabled}
}

// terminateEnabledSetting 从站点设置读取删机开关；缺省关闭。
// 设置表被误删时回落到关闭而不是打开：自动删除是破坏性动作，宁可漏删
// 也不能误删，漏删的服务停在 suspended 仍可人工处理。
func terminateEnabledSetting(db *gorm.DB) func() bool {
	return func() bool {
		var row model.Setting
		if err := db.First(&row, "key = ?", model.SettingLifecycleTerminate).Error; err != nil {
			return false
		}
		return row.Value == "1"
	}
}

// Start 启动周期巡检；调用方应在独立 goroutine 中运行。
func (s *LifecycleService) Start(ctx context.Context) {
	// 避免进程刚启动、插件尚未完成握手时立刻大量 RPC。
	select {
	case <-time.After(lifecycleInterval / 2):
	case <-ctx.Done():
		return
	}
	s.Run(ctx)
	ticker := time.NewTicker(lifecycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Run(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Run 执行一轮流量与到期执法。
func (s *LifecycleService) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("生命周期巡检 panic（已恢复，下轮继续）: %v", r)
		}
	}()
	s.enforceTraffic(ctx)
	s.enforceExpiry(ctx)
	s.retryPendingResumes(ctx)
}

func (s *LifecycleService) enforceTraffic(ctx context.Context) {
	if s.host == nil {
		return
	}
	var services []model.Service
	if err := s.db.Where("status = ? AND upstream_plugin_id <> '' AND upstream_host_id <> ''", model.ServiceActive).
		Find(&services).Error; err != nil {
		log.Printf("流量执法读取服务列表失败: %v", err)
		return
	}
	for i := range services {
		svc := &services[i]
		quotaGB := TrafficQuotaTotalGB(s.db, svc)
		// 不限流量的服务照常差分累计（供 /traffic 展示用量），只是不参与执法。
		used, ok := s.sampleTraffic(ctx, svc)
		if quotaGB <= 0 || !ok || used < int64(quotaGB)*bytesPerGiB {
			continue
		}
		if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_SUSPEND); err != nil {
			log.Printf("流量超限停机失败 service=%d host=%s（下轮重试）: %v", svc.ID, svc.UpstreamHostID, err)
			continue
		}
		if !s.markSuspended(svc, suspendReasonTraffic, time.Now()) {
			// 上游已停机但本地状态被并发变更（典型：刚结清流量包/续费）：
			// 补发 UNSUSPEND 回滚上游动作，以本地账务为准。
			if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_UNSUSPEND); err != nil {
				log.Printf("流量停机回滚失败 service=%d host=%s（需人工核对上游状态）: %v", svc.ID, svc.UpstreamHostID, err)
			}
			continue
		}
		log.Printf("服务 %d 流量超限已停机不断机（used=%d bytes quota=%d GB），加购后自动恢复", svc.ID, used, quotaGB)
	}
}

func (s *LifecycleService) sampleTraffic(ctx context.Context, svc *model.Service) (int64, bool) {
	metrics, err := s.fetchMetrics(ctx, svc)
	if err != nil || metrics == nil {
		if err != nil {
			log.Printf("流量采样失败 service=%d host=%s: %v", svc.ID, svc.UpstreamHostID, err)
		}
		return svc.TrafficUsedBytes, false
	}
	curRx := int64(metrics.GetNetworkRxBytes())
	curTx := int64(metrics.GetNetworkTxBytes())
	used, lastRx, lastTx := accumulateTraffic(svc.TrafficUsedBytes, svc.TrafficLastRxBytes, svc.TrafficLastTxBytes, curRx, curTx)
	if err := s.db.Model(&model.Service{}).Where("id = ?", svc.ID).Updates(map[string]any{
		"traffic_used_bytes":    used,
		"traffic_last_rx_bytes": lastRx,
		"traffic_last_tx_bytes": lastTx,
	}).Error; err != nil {
		log.Printf("流量累计写回失败 service=%d: %v", svc.ID, err)
		return svc.TrafficUsedBytes, false
	}
	svc.TrafficUsedBytes, svc.TrafficLastRxBytes, svc.TrafficLastTxBytes = used, lastRx, lastTx
	return used, true
}

func (s *LifecycleService) enforceExpiry(ctx context.Context) {
	now := time.Now()
	var active []model.Service
	if err := s.db.Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?", model.ServiceActive, now).
		Find(&active).Error; err != nil {
		log.Printf("到期执法读取服务列表失败: %v", err)
		return
	}
	// 本轮刚自动停机的服务：停机时刻就是 now，即使 expires_at 早已超过宽限期
	// 也必须完整享受 72 小时，绝不能在同一轮的删机段被立即 TERMINATE。
	suspendedThisRound := make(map[uint]bool, len(active))
	for i := range active {
		svc := &active[i]
		if svc.UpstreamPluginID != "" && svc.UpstreamHostID != "" {
			if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_SUSPEND); err != nil {
				log.Printf("到期停机失败 service=%d host=%s（下轮重试）: %v", svc.ID, svc.UpstreamHostID, err)
				continue
			}
		}
		if !s.markSuspended(svc, suspendReasonExpired, now) {
			// 上游已停但本地刚被续费并发改写：回滚上游停机，尊重已付款结果。
			if svc.UpstreamPluginID != "" && svc.UpstreamHostID != "" {
				if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_UNSUSPEND); err != nil {
					log.Printf("到期停机回滚失败 service=%d host=%s（需人工核对上游状态）: %v", svc.ID, svc.UpstreamHostID, err)
				}
			}
			continue
		}
		suspendedThisRound[svc.ID] = true
		log.Printf("服务 %d 已到期停机，宽限期至 %s", svc.ID, now.Add(expiryGrace).Format(time.RFC3339))
	}

	// 宽限期从实际停机时刻（suspended_at）起算；存量行 suspended_at 为 NULL
	// 时不命中（宁可不删也不能按可能失真的 expires_at 立删）。开关关闭时
	// 只记干跑日志：正式实例的删除必须先经管理员确认清单再开启。
	deadline := now.Add(-expiryGrace)
	var expired []model.Service
	if err := s.db.Where("status = ? AND suspend_reason = ? AND suspended_at IS NOT NULL AND suspended_at < ?",
		model.ServiceSuspended, "expired", deadline).Find(&expired).Error; err != nil {
		log.Printf("到期删机读取服务列表失败: %v", err)
		return
	}
	for i := range expired {
		svc := &expired[i]
		if suspendedThisRound[svc.ID] {
			continue // 刚停机，本轮不删。
		}
		if s.terminateEnabled != nil && !s.terminateEnabled() {
			log.Printf("服务 %d 宽限期已满（干跑：删除开关未开启，仅记录，未删机）", svc.ID)
			continue
		}
		if svc.UpstreamPluginID != "" && svc.UpstreamHostID != "" {
			if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_TERMINATE); err != nil {
				log.Printf("到期删机失败 service=%d host=%s（下轮重试）: %v", svc.ID, svc.UpstreamHostID, err)
				continue
			}
		}
		s.markTerminated(svc)
		log.Printf("服务 %d 到期宽限期结束，实例已删除", svc.ID)
	}
}

// markSuspended 以 CAS 写入自动停机状态（仅 active→suspended），返回是否生效。
// 条件更新是竞态防线：读取列表后用户可能已完成续费（status 已变回 active 且
// expires_at 顺延），无条件覆盖会把已付款的服务打回停机且没有任何自动恢复路径。
// CAS 失败（RowsAffected==0）说明服务已不在 active，放弃本次写入。
func (s *LifecycleService) markSuspended(svc *model.Service, reason string, now time.Time) bool {
	res := s.db.Model(&model.Service{}).Where("id = ? AND status = ?", svc.ID, model.ServiceActive).
		Updates(map[string]any{"status": model.ServiceSuspended, "suspend_reason": reason, "suspended_at": now})
	if res.Error != nil {
		log.Printf("写入自动停机状态失败 service=%d: %v", svc.ID, res.Error)
		return false
	}
	if res.RowsAffected == 0 {
		log.Printf("跳过停机写入 service=%d：状态已并发变更（可能刚续费）", svc.ID)
		return false
	}
	svc.Status, svc.SuspendReason, svc.SuspendedAt = model.ServiceSuspended, reason, &now
	return true
}

// markTerminated 以 CAS 写入终止状态（仅 suspended+expired→terminated）。
// 条件防止把并发恢复（续费/加购已写回 active/suspended 之外的态）误覆盖。
func (s *LifecycleService) markTerminated(svc *model.Service) {
	res := s.db.Model(&model.Service{}).Where("id = ? AND status = ? AND suspend_reason = ?",
		svc.ID, model.ServiceSuspended, suspendReasonExpired).
		Updates(map[string]any{"status": model.ServiceTerminated, "suspend_reason": "", "suspended_at": nil})
	if res.Error != nil {
		log.Printf("写入终止状态失败 service=%d: %v", svc.ID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("跳过终止写入 service=%d：状态已并发变更", svc.ID)
		return
	}
	svc.Status, svc.SuspendReason, svc.SuspendedAt = model.ServiceTerminated, "", nil
}

// suspendReasonTraffic / suspendReasonExpired 是 suspend_reason 的合法取值，
// 续费、流量包结清与恢复流程按它判断「这台机器为什么停着」。
const (
	suspendReasonTraffic = "traffic"
	suspendReasonExpired = "expired"
)

// ResumeSuspendedService 在续费或流量包结清后恢复指定原因的自动停机服务。
// 调用方应在本地账务提交后调用；上游开机失败会留下 provision_error，下一次
// 人工/自动重试仍可恢复，且不会回滚已支付账单。
func ResumeSuspendedService(db *gorm.DB, plugins *plugin.Manager, svc *model.Service, reasons ...string) {
	var host hostManager
	if plugins != nil {
		host = plugins
	}
	resumeSuspended(db, host, svc, reasons...)
}

// resumeSuspended 是 ResumeSuspendedService 的可注入实现（测试用 fake 上游）。
//
// 前置条件已满足（钱已收、配额/到期已落账）后把自动停机的服务恢复开机。
// 上游开机失败时把原因写入 provision_error 并保持 suspended：下一轮巡检的
// resume 重试（见 retryPendingResumes）与管理员人工重试都能再触发，已收款
// 不受影响。本地无上游的服务直接写回 active。
func resumeSuspended(db *gorm.DB, host hostManager, svc *model.Service, reasons ...string) {
	if svc == nil || svc.Status != model.ServiceSuspended || svc.SuspendReason == "" {
		return
	}
	matched := false
	for _, reason := range reasons {
		if svc.SuspendReason == reason {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	if host != nil && svc.UpstreamPluginID != "" && svc.UpstreamHostID != "" {
		ifaceConfig, err := interfaceConfigForService(db, svc)
		if err != nil {
			log.Printf("恢复服务读取接口配置失败 service=%d: %v", svc.ID, err)
			saveResumeError(db, svc, err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		reply, err := host.ManageHost(ctx, svc.UpstreamPluginID, &pb.ManageHostRequest{
			HostId: svc.UpstreamHostID, Action: pb.HostAction_HOST_ACTION_UNSUSPEND, InterfaceConfig: ifaceConfig,
		})
		cancel()
		if err != nil || reply == nil || !reply.GetSuccess() {
			if err == nil && reply != nil {
				err = fmt.Errorf("%s", reply.GetError())
			}
			log.Printf("恢复服务上游开机失败 service=%d host=%s（已记录，稍后自动重试）: %v", svc.ID, svc.UpstreamHostID, err)
			saveResumeError(db, svc, err)
			return
		}
	}
	// CAS：只有仍处于同原因的 suspended 才写回 active，防止与巡检的并发
	// 停机/删机交错后覆盖出新状态（例如刚被 markTerminated 的服务复活）。
	res := db.Model(&model.Service{}).
		Where("id = ? AND status = ? AND suspend_reason = ?", svc.ID, model.ServiceSuspended, svc.SuspendReason).
		Updates(map[string]any{"status": model.ServiceActive, "suspend_reason": "", "suspended_at": nil, "provision_error": "", "resume_pending": false})
	if res.Error != nil {
		log.Printf("恢复服务写状态失败 service=%d: %v", svc.ID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		log.Printf("跳过恢复写入 service=%d：状态已并发变更", svc.ID)
		return
	}
	svc.Status, svc.SuspendReason, svc.SuspendedAt = model.ServiceActive, "", nil
	log.Printf("服务 %d 已恢复运行", svc.ID)
}

// saveResumeError 把恢复失败原因持久化到 provision_error，供巡检重试与管理员排查。
func saveResumeError(db *gorm.DB, svc *model.Service, err error) {
	msg := truncateProvisionError(fmt.Sprintf("自动恢复开机失败: %v", err))
	_ = db.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{"provision_error": msg, "resume_pending": true}).Error
}

// retryPendingResumes 每轮巡检时重试「前置条件已满足但开机失败」的恢复：
// traffic 停机 + 配额未超，或 expired 停机 + 到期已顺延到未来。
// 没有它，一次上游抖动就会让已付款用户永久卡在 suspended。
func (s *LifecycleService) retryPendingResumes(ctx context.Context) {
	if s.host == nil {
		return
	}
	var pending []model.Service
	if err := s.db.Where("status = ? AND suspend_reason <> '' AND resume_pending = ? AND provision_error <> ''",
		model.ServiceSuspended, true).Find(&pending).Error; err != nil {
		return
	}
	for i := range pending {
		svc := &pending[i]
		// 只重试前置条件确实已满足的原因，避免把流量仍超限/仍过期的机器开机。
		switch svc.SuspendReason {
		case suspendReasonTraffic:
			if TrafficQuotaTotalGB(s.db, svc) > 0 && svc.TrafficUsedBytes >= int64(TrafficQuotaTotalGB(s.db, svc))*bytesPerGiB {
				continue
			}
		case suspendReasonExpired:
			if svc.ExpiresAt == nil || !svc.ExpiresAt.After(time.Now()) {
				continue
			}
		default:
			continue
		}
		resumeSuspended(s.db, s.host, svc, svc.SuspendReason)
	}
}

func (s *LifecycleService) manageHost(ctx context.Context, svc *model.Service, action pb.HostAction) error {
	if s.host == nil {
		return fmt.Errorf("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, svc)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	reply, err := s.host.ManageHost(rctx, svc.UpstreamPluginID, &pb.ManageHostRequest{
		HostId: svc.UpstreamHostID, Action: action, InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return err
	}
	if reply == nil {
		return fmt.Errorf("上游未返回操作结果")
	}
	if !reply.GetSuccess() {
		return fmt.Errorf("%s", reply.GetError())
	}
	return nil
}

func (s *LifecycleService) fetchMetrics(ctx context.Context, svc *model.Service) (*pb.HostMetrics, error) {
	if s.host == nil {
		return nil, fmt.Errorf("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, svc)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, lifecycleTimeout)
	defer cancel()
	reply, err := s.host.GetHostMetrics(rctx, svc.UpstreamPluginID, &pb.GetHostMetricsRequest{
		HostId: svc.UpstreamHostID, InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, err
	}
	if reply == nil || reply.GetMetrics() == nil {
		return nil, fmt.Errorf("上游未返回监控数据")
	}
	return reply.GetMetrics(), nil
}

// TrafficQuotaTotalGB 汇总首购选配与售后加购的总配额；0 表示不限流量。
// 首购选配按「同订单中属于本服务商品」的明细过滤：一单多商品时各服务
// 只认自己商品的 traffic_gb，否则每个服务都会把整单配额算进去（超卖）。
func TrafficQuotaTotalGB(db *gorm.DB, svc *model.Service) int {
	total := int(svc.TrafficExtraGB)
	if db == nil {
		return total
	}
	var items []model.OrderItem
	if err := db.Where("order_id = ? AND product_id = ?", svc.OrderID, svc.ProductID).Find(&items).Error; err == nil {
		for _, item := range items {
			if value, err := strconv.Atoi(item.Options["traffic_gb"]); err == nil && value > 0 {
				total += value
			}
		}
	}
	return total
}

// accumulateTraffic 处理计数器差分与重启回退，重装不会清掉 prevUsed。
func accumulateTraffic(prevUsed, prevRx, prevTx, curRx, curTx int64) (int64, int64, int64) {
	deltaRx, deltaTx := curRx, curTx
	if curRx >= prevRx {
		deltaRx = curRx - prevRx
	}
	if curTx >= prevTx {
		deltaTx = curTx - prevTx
	}
	return prevUsed + deltaRx + deltaTx, curRx, curTx
}
