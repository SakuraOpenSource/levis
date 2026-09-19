package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// fakeHost 是注入 lifecycle 的上游操作面，记录所有动作便于断言。
type fakeHost struct {
	metrics    map[string]*pb.HostMetrics // hostID -> metrics
	actions    []string                   // 记录 "hostID:action" 序列
	metricsN   int
	metricsErr error
	manageErr  error
	// UNSUSPEND/TERMINATE 等动作的失败表：hostID -> error
	manageFail map[string]error
}

func (f *fakeHost) GetHostMetrics(ctx context.Context, pluginID string, req *pb.GetHostMetricsRequest) (*pb.GetHostMetricsReply, error) {
	f.metricsN++
	if f.metricsErr != nil {
		return nil, f.metricsErr
	}
	if m, ok := f.metrics[req.GetHostId()]; ok {
		return &pb.GetHostMetricsReply{Metrics: m}, nil
	}
	return nil, fmt.Errorf("host %s 不存在", req.GetHostId())
}

func (f *fakeHost) ManageHost(ctx context.Context, pluginID string, req *pb.ManageHostRequest) (*pb.ManageHostReply, error) {
	action := pb.HostAction_name[int32(req.GetAction())]
	f.actions = append(f.actions, req.GetHostId()+":"+action)
	if f.manageFail != nil {
		if err, ok := f.manageFail[req.GetHostId()]; ok {
			return nil, err
		}
	}
	return &pb.ManageHostReply{Success: true}, nil
}

func (f *fakeHost) hasAction(hostID, action string) bool {
	want := hostID + ":" + action
	for _, a := range f.actions {
		if a == want {
			return true
		}
	}
	return false
}

// seedLifecycleService 建一个带上游绑定的服务；quotaGB<=0 表示不限流量。
func seedLifecycleService(t *testing.T, db *gorm.DB, status string, expiresAt *time.Time, trafficExtraGB int64, hostID string) *model.Service {
	t.Helper()
	user := seedUser(t, db, "u"+fmt.Sprint(time.Now().UnixNano()), 0)
	svc := model.Service{
		UserID:           user.ID,
		ProductID:        1,
		OrderID:          1,
		Name:             "lifecycle-test",
		Status:           status,
		BillingCyc:       model.CycleMonthly,
		PriceCents:       100,
		ExpiresAt:        expiresAt,
		TrafficExtraGB:   trafficExtraGB,
		UpstreamPluginID: "virtualis",
		UpstreamHostID:   hostID,
	}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatalf("创建测试服务失败: %v", err)
	}
	return &svc
}

// markAutoSuspended 模拟 lifecycle 已写入的自动停机状态并同步内存对象。
func markAutoSuspended(t *testing.T, db *gorm.DB, svc *model.Service, reason string) {
	markAutoSuspendedAt(t, db, svc, reason, time.Now())
}

// markAutoSuspendedAt 以指定停机时刻写入自动停机状态（宽限期基准回归用）。
func markAutoSuspendedAt(t *testing.T, db *gorm.DB, svc *model.Service, reason string, at time.Time) {
	t.Helper()
	if err := db.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{"status": model.ServiceSuspended, "suspend_reason": reason, "suspended_at": at}).Error; err != nil {
		t.Fatalf("写入停机状态失败: %v", err)
	}
	svc.Status, svc.SuspendReason, svc.SuspendedAt = model.ServiceSuspended, reason, &at
}

func giB(n int64) int64 { return n * bytesPerGiB }

// --- 场景 1：流量未超限，只累计不停机 ---
func TestLifecycleTrafficUnderQuotaAccumulatesOnly(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceActive, nil, 10, "host-1")
	host := &fakeHost{metrics: map[string]*pb.HostMetrics{
		"host-1": {NetworkRxBytes: 100, NetworkTxBytes: 200},
	}}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("未超限不应停机，status=%s", got.Status)
	}
	if got.TrafficUsedBytes != 300 {
		t.Fatalf("应累计 300 字节，实际 %d", got.TrafficUsedBytes)
	}
	if host.hasAction("host-1", "HOST_ACTION_SUSPEND") {
		t.Fatal("未超限绝不应 SUSPEND")
	}
}

// --- 场景 2：流量超限停机，写 reason=traffic ---
func TestLifecycleTrafficExceededSuspends(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceActive, nil, 1, "host-1")
	host := &fakeHost{metrics: map[string]*pb.HostMetrics{
		"host-1": {NetworkRxBytes: uint64(giB(2)), NetworkTxBytes: 0},
	}}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceSuspended || got.SuspendReason != "traffic" {
		t.Fatalf("应 suspended/traffic，实际 %s/%s", got.Status, got.SuspendReason)
	}
	if !host.hasAction("host-1", "HOST_ACTION_SUSPEND") {
		t.Fatal("超限应调用 SUSPEND")
	}
}

// --- 场景 3：metrics 失败不改状态，下轮重试 ---
func TestLifecycleMetricsFailureKeepsState(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceActive, nil, 1, "host-1")
	host := &fakeHost{metricsErr: errors.New("upstream down")}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceActive {
		t.Fatalf("metrics 失败应保持 active 待重试，实际 %s", got.Status)
	}
	// 第二轮恢复
	host.metricsErr = nil
	host.metrics = map[string]*pb.HostMetrics{"host-1": {NetworkRxBytes: uint64(giB(2))}}
	lc.Run(context.Background())
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceSuspended {
		t.Fatalf("恢复后超限应停机，实际 %s", got.Status)
	}
}

// --- 场景 4：计数器回退不清历史 ---
func TestLifecycleCounterResetKeepsHistory(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceActive, nil, 100, "host-1")
	host := &fakeHost{metrics: map[string]*pb.HostMetrics{
		"host-1": {NetworkRxBytes: 5000, NetworkTxBytes: 6000},
	}}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())
	// 模拟重装：计数器回退到小值
	host.metrics["host-1"] = &pb.HostMetrics{NetworkRxBytes: 3, NetworkTxBytes: 4}
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.TrafficUsedBytes != 11007 {
		t.Fatalf("计数器回退应保留历史 11007，实际 %d", got.TrafficUsedBytes)
	}
}

// --- 场景 5：到期停机，写 reason=expired ---
func TestLifecycleExpirySuspends(t *testing.T) {
	db := newTestDB(t)
	expired := time.Now().Add(-1 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceActive, &expired, 0, "host-1")
	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	if err := db.First(&got, svc.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ServiceSuspended || got.SuspendReason != "expired" {
		t.Fatalf("应 suspended/expired，实际 %s/%s", got.Status, got.SuspendReason)
	}
}

// --- 场景 6+7：72 小时内不删；超过 72 小时才删 ---
func TestLifecycleExpiryGraceWindow(t *testing.T) {
	db := newTestDB(t)
	// 宽限期从实际停机时刻（suspended_at）起算，与 expires_at 各自独立：
	// 两个服务 expires_at 均已过期 71/73 小时，停机时刻决定是否删。
	inGrace := time.Now().Add(-71 * time.Hour)
	svc1 := seedLifecycleService(t, db, model.ServiceSuspended, &inGrace, 0, "host-1")
	markAutoSuspendedAt(t, db, svc1, "expired", time.Now().Add(-71*time.Hour))

	pastGrace := time.Now().Add(-73 * time.Hour)
	svc2 := seedLifecycleService(t, db, model.ServiceSuspended, &pastGrace, 0, "host-2")
	markAutoSuspendedAt(t, db, svc2, "expired", time.Now().Add(-73*time.Hour))

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got1, got2 model.Service
	db.First(&got1, svc1.ID)
	db.First(&got2, svc2.ID)
	if got1.Status != model.ServiceSuspended || got1.SuspendReason != "expired" {
		t.Fatalf("宽限期内不应删机：%s/%s", got1.Status, got1.SuspendReason)
	}
	if got2.Status != model.ServiceTerminated {
		t.Fatalf("宽限期满应 terminated，实际 %s", got2.Status)
	}
	if !host.hasAction("host-2", "HOST_ACTION_TERMINATE") {
		t.Fatal("宽限期满应调用 TERMINATE")
	}
}

// --- 场景 8：TERMINATE 失败保持 suspended，下轮重试 ---
func TestLifecycleTerminateFailureRetries(t *testing.T) {
	db := newTestDB(t)
	pastGrace := time.Now().Add(-73 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &pastGrace, 0, "host-2")
	markAutoSuspendedAt(t, db, svc, "expired", time.Now().Add(-73*time.Hour))

	host := &fakeHost{manageFail: map[string]error{"host-2": errors.New("delete failed")}}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended {
		t.Fatalf("TERMINATE 失败应保持 suspended，实际 %s", got.Status)
	}
	// 恢复后第二轮删除
	host.manageFail = nil
	lc.Run(context.Background())
	db.First(&got, svc.ID)
	if got.Status != model.ServiceTerminated {
		t.Fatalf("恢复后应删除，实际 %s", got.Status)
	}
}

// --- 场景 9：手动 suspended（reason=""）绝不删除 ---
func TestLifecycleManualSuspendNeverDeleted(t *testing.T) {
	db := newTestDB(t)
	pastGrace := time.Now().Add(-100 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &pastGrace, 0, "host-2")
	// 手动停机：不写 suspend_reason（默认空）

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended {
		t.Fatalf("手动停机不应被删除，实际 %s", got.Status)
	}
	if len(host.actions) != 0 {
		t.Fatalf("手动停机不应触发任何上游动作，实际 %v", host.actions)
	}
}

// --- 场景 9b：干跑开关关闭时，宽限期满也不删 ---
func TestLifecycleTerminateDryRun(t *testing.T) {
	db := newTestDB(t)
	pastGrace := time.Now().Add(-73 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &pastGrace, 0, "host-2")
	markAutoSuspendedAt(t, db, svc, "expired", time.Now().Add(-73*time.Hour))

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return false }) // 开关关
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended {
		t.Fatalf("干跑模式不应删除，实际 %s", got.Status)
	}
	if len(host.actions) != 0 {
		t.Fatal("干跑模式不应有任何上游动作")
	}
	// 开启后删除
	lc2 := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc2.Run(context.Background())
	db.First(&got, svc.ID)
	if got.Status != model.ServiceTerminated {
		t.Fatalf("开关开启后应删除，实际 %s", got.Status)
	}
}

// --- 场景 10：traffic 停机 + 流量包结清 → UNSUSPEND + active ---
func TestResumeTrafficSuspended(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, nil, 10, "host-1")
	markAutoSuspended(t, db, svc, "traffic")

	host := &fakeHost{}
	resumeSuspended(db, host, svc, suspendReasonTraffic)

	if !host.hasAction("host-1", "HOST_ACTION_UNSUSPEND") {
		t.Fatal("应调用 UNSUSPEND")
	}
	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceActive || got.SuspendReason != "" {
		t.Fatalf("应恢复 active，实际 %s/%s", got.Status, got.SuspendReason)
	}
}

// --- 场景 10b：UNSUSPEND 失败保持 suspended ---
func TestResumeUpstreamFailureKeepsSuspended(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, nil, 10, "host-1")
	markAutoSuspended(t, db, svc, "traffic")

	host := &fakeHost{manageFail: map[string]error{"host-1": errors.New("start failed")}}
	resumeSuspended(db, host, svc, suspendReasonTraffic)

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended {
		t.Fatalf("UNSUSPEND 失败应保持 suspended，实际 %s", got.Status)
	}
}

// --- 场景 11+12：expired 停机 + 续费成功 → UNSUSPEND + active；RENEW 失败不恢复 ---
func TestResumeAfterRenew(t *testing.T) {
	db := newTestDB(t)
	future := time.Now().Add(24 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &future, 0, "host-1")
	markAutoSuspended(t, db, svc, "expired")

	// 续费路径（billing.Renew）会在上游对账后调用恢复；这里直接验证恢复函数
	// 对 expired 原因的处理，与 renew() 内的调用一致。
	host := &fakeHost{}
	resumeSuspended(db, host, svc, suspendReasonTraffic, suspendReasonExpired)
	if !host.hasAction("host-1", "HOST_ACTION_UNSUSPEND") {
		t.Fatal("续费成功后应 UNSUSPEND")
	}
	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceActive {
		t.Fatalf("续费后应 active，实际 %s", got.Status)
	}
}

// --- 场景 13：本地无上游的服务到期只改本地状态 ---
func TestLifecycleExpiryLocalOnlyService(t *testing.T) {
	db := newTestDB(t)
	expired := time.Now().Add(-1 * time.Hour)
	user := seedUser(t, db, "local-only", 0)
	svc := model.Service{
		UserID: user.ID, ProductID: 1, OrderID: 1, Name: "local", Status: model.ServiceActive,
		BillingCyc: model.CycleMonthly, PriceCents: 100, ExpiresAt: &expired,
	}
	db.Create(&svc)

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended || got.SuspendReason != "expired" {
		t.Fatalf("本地服务到期应本地停机：%s/%s", got.Status, got.SuspendReason)
	}
}

// --- 场景 13b：本地无上游服务宽限期满 → 本地 terminated（不调上游）---
func TestLifecycleTerminateLocalOnlyService(t *testing.T) {
	db := newTestDB(t)
	pastGrace := time.Now().Add(-100 * time.Hour)
	stoppedAt := time.Now().Add(-80 * time.Hour)
	user := seedUser(t, db, "local-only2", 0)
	svc := model.Service{
		UserID: user.ID, ProductID: 1, OrderID: 1, Name: "local", Status: model.ServiceSuspended,
		BillingCyc: model.CycleMonthly, PriceCents: 100, ExpiresAt: &pastGrace,
		SuspendReason: "expired", SuspendedAt: &stoppedAt,
	}
	db.Create(&svc)

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceTerminated {
		t.Fatalf("本地服务宽限期满应 terminated，实际 %s", got.Status)
	}
	if len(host.actions) != 0 {
		t.Fatal("无上游服务不应有任何上游动作")
	}
}

// --- 回归（review High 2）：停机远晚于到期时，宽限期从停机时刻起算 ---
func TestLifecycleGraceFromActualSuspendTime(t *testing.T) {
	db := newTestDB(t)
	// 到期已 100h，但 10h 前才停机成功：宽限期未满，绝不删。
	longExpired := time.Now().Add(-100 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, &longExpired, 0, "host-1")
	markAutoSuspendedAt(t, db, svc, "expired", time.Now().Add(-10*time.Hour))

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended {
		t.Fatalf("宽限期应从停机时刻起算（还剩 62h），实际 %s", got.Status)
	}
	if len(host.actions) != 0 {
		t.Fatal("宽限期未满不应有任何上游动作")
	}
}

// --- 回归（review High 1）：续费与巡检并发交错，CAS 防止覆盖已付款状态 ---
func TestLifecycleSuspendCASSkipsRenewedService(t *testing.T) {
	db := newTestDB(t)
	expired := time.Now().Add(-1 * time.Hour)
	svc := seedLifecycleService(t, db, model.ServiceActive, &expired, 0, "host-1")
	// 模拟：巡检读到 active+expired 列表后、调上游 SUSPEND 前，用户完成续费。
	// renewInTx 已把 status 写回 active 且 expires_at 顺延到未来。
	future := time.Now().Add(30 * 24 * time.Hour)
	db.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{"status": model.ServiceActive, "expires_at": future})

	host := &fakeHost{}
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceActive {
		t.Fatalf("CAS 应跳过已续费服务，实际 %s", got.Status)
	}
	if !future.Equal(*got.ExpiresAt) {
		t.Fatal("续费后的到期时间不应被巡检改写")
	}
	// 上游 SUSPEND 可能已发出（动作先于 CAS）：必须补发 UNSUSPEND 回滚。
	if host.hasAction("host-1", "HOST_ACTION_SUSPEND") && !host.hasAction("host-1", "HOST_ACTION_UNSUSPEND") {
		t.Fatal("上游已 SUSPEND 但未补发 UNSUSPEND 回滚")
	}
}

// --- 回归（review Medium 3）：resume 失败写 resume_pending，巡检自动重试 ---
func TestResumeRetryAfterUpstreamFailure(t *testing.T) {
	db := newTestDB(t)
	svc := seedLifecycleService(t, db, model.ServiceSuspended, nil, 10, "host-1")
	markAutoSuspended(t, db, svc, "traffic")

	host := &fakeHost{manageFail: map[string]error{"host-1": errors.New("start failed")}}
	resumeSuspended(db, host, svc, suspendReasonTraffic)

	var got model.Service
	db.First(&got, svc.ID)
	if got.Status != model.ServiceSuspended || !got.ResumePending {
		t.Fatalf("resume 失败应保持 suspended 且置 resume_pending：%s/%v", got.Status, got.ResumePending)
	}

	// 上游恢复后，巡检重试成功。
	host.manageFail = nil
	lc := newLifecycleServiceForTest(db, host, func() bool { return true })
	lc.Run(context.Background())
	db.First(&got, svc.ID)
	if got.Status != model.ServiceActive || got.ResumePending {
		t.Fatalf("巡检重试应恢复 active 并清 resume_pending：%s/%v", got.Status, got.ResumePending)
	}
}

// --- 配额汇总：首购选配 + 售后加购 ---
func TestTrafficQuotaTotalGB(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "quota-user", 0)
	order := model.Order{UserID: user.ID, Status: "paid"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	item := model.OrderItem{OrderID: order.ID, ProductID: 1, ProductName: "p", PriceCents: 100, Quantity: 1, BillingCyc: model.CycleMonthly, Options: model.OptionMap{"traffic_gb": "50"}}
	if err := db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	svc := model.Service{UserID: user.ID, ProductID: 1, OrderID: order.ID, Name: "q", Status: model.ServiceActive, BillingCyc: model.CycleMonthly, TrafficExtraGB: 25}
	if err := db.Create(&svc).Error; err != nil {
		t.Fatal(err)
	}
	if got := TrafficQuotaTotalGB(db, &svc); got != 75 {
		t.Fatalf("总配额应为 75（50 首购 + 25 加购），实际 %d", got)
	}
}

func TestAccumulateTrafficPreservesUsageAcrossCounterReset(t *testing.T) {
	used, rx, tx := accumulateTraffic(0, 0, 0, 100, 200)
	if used != 300 || rx != 100 || tx != 200 {
		t.Fatalf("首次累计 = (%d,%d,%d)，期望 (300,100,200)", used, rx, tx)
	}
	used, rx, tx = accumulateTraffic(used, rx, tx, 150, 260)
	if used != 410 || rx != 150 || tx != 260 {
		t.Fatalf("正常增量累计 = (%d,%d,%d)，期望 (410,150,260)", used, rx, tx)
	}
	// 重装/重启后 hypervisor 计数器回退：当前值作为新基线，旧 used 不清零。
	used, rx, tx = accumulateTraffic(used, rx, tx, 7, 11)
	if used != 428 || rx != 7 || tx != 11 {
		t.Fatalf("计数器回退累计 = (%d,%d,%d)，期望 (428,7,11)", used, rx, tx)
	}
}
