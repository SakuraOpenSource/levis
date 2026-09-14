package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// BillingService 提供已购服务与账单的读取，以及服务的续费。
type BillingService struct {
	db      *gorm.DB
	wallet  *WalletService
	plugins *plugin.Manager
}

// NewBillingService 构造 BillingService。plugins 可为 nil（测试或无插件场景），
// 此时上游服务续费仅本地顺延，不会向上游发起续费。
func NewBillingService(db *gorm.DB, wallet *WalletService, plugins *plugin.Manager) *BillingService {
	return &BillingService{db: db, wallet: wallet, plugins: plugins}
}

// Services 分页返回用户的已购服务。
func (s *BillingService) Services(userID uint, offset, limit int) ([]model.Service, int64, error) {
	var (
		items []model.Service
		total int64
	)
	if err := s.db.Model(&model.Service{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := s.db.Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Service 读取用户的单个服务。
func (s *BillingService) Service(userID, serviceID uint) (*model.Service, error) {
	var item model.Service
	err := s.db.First(&item, "id = ? AND user_id = ?", serviceID, userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	return &item, nil
}

// Invoices 分页返回用户账单。
func (s *BillingService) Invoices(userID uint, offset, limit int) ([]model.Invoice, int64, error) {
	var (
		items []model.Invoice
		total int64
	)
	if err := s.db.Model(&model.Invoice{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := s.db.Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Invoice 读取用户的单个账单（含明细）。
func (s *BillingService) Invoice(userID, invoiceID uint) (*model.Invoice, error) {
	var item model.Invoice
	err := s.db.Preload("Items").
		First(&item, "id = ? AND user_id = ?", invoiceID, userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("账单不存在")
		}
		return nil, err
	}
	return &item, nil
}

// RenewResult 是续费结果。
type RenewResult struct {
	Service *model.Service `json:"service"`
	Invoice *model.Invoice `json:"invoice"`
}

// Renew 为已购服务续费一个周期。
//
// 事务内依次完成：锁定服务 → 扣减余额并记流水 → 生成已付账单 → 顺延到期时间。
// 只允许续费在用中的服务；一次性付费没有周期，谈不上续费。到期时间已过则从
// 当前时间起算，尚未过期则从原到期日起顺延，避免「提前续费吃掉剩余时长」。
func (s *BillingService) Renew(userID, serviceID uint) (*RenewResult, error) {
	return s.renew(userID, serviceID, true)
}

// RenewExternal settles an already verified external payment without debiting balance.
func (s *BillingService) RenewExternal(userID, serviceID uint) (*RenewResult, error) {
	return s.renew(userID, serviceID, false)
}

func (s *BillingService) renew(userID, serviceID uint, debit bool) (*RenewResult, error) {
	var out *RenewResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.renewInTx(tx, userID, serviceID, debit)
		return err
	})
	if err != nil {
		return nil, err
	}
	// 事务提交后再调上游：上游失败只记 failed，不回滚已付账单
	// （与订单先付后开通的可重试模式一致）。免费服务同样走此路径。
	if out != nil && out.Service != nil && needsUpstreamRenew(out.Service) {
		s.reconcileUpstreamRenewal(out.Service.ID)
		var reloaded model.Service
		if err := s.db.First(&reloaded, out.Service.ID).Error; err == nil {
			out.Service = &reloaded
		}
	}
	return out, nil
}

// needsUpstreamRenew 报告服务是否绑定了上游，需要提交后向上游续费。
func needsUpstreamRenew(svc *model.Service) bool {
	return svc != nil && svc.UpstreamPluginID != "" && svc.UpstreamHostID != ""
}

// reconcileUpstreamRenewal 在 DB 事务提交后向上游发起续费并对账。
// 成功且上游返回新到期时间则以上游为准；失败则把服务置为 failed 并记录
// provision_error，账单保持已付，调用方不应将其视为支付失败。
func (s *BillingService) reconcileUpstreamRenewal(serviceID uint) {
	var svc model.Service
	if err := s.db.First(&svc, serviceID).Error; err != nil {
		log.Printf("续费对账读取服务 %d 失败: %v", serviceID, err)
		return
	}
	if !needsUpstreamRenew(&svc) {
		return
	}
	expiry, err := s.renewUpstream(&svc)
	if err != nil {
		msg := truncateProvisionError(err.Error())
		log.Printf("上游续费失败 service=%d host=%s: %v", svc.ID, svc.UpstreamHostID, err)
		_ = s.db.Model(&model.Service{}).Where("id = ?", svc.ID).
			Updates(map[string]any{"status": model.ServiceFailed, "provision_error": msg}).Error
		return
	}
	updates := map[string]any{"provision_error": ""}
	if expiry != nil {
		updates["next_due_at"] = *expiry
		updates["expires_at"] = *expiry
	}
	_ = s.db.Model(&model.Service{}).Where("id = ?", svc.ID).Updates(updates).Error
}

// loadRenewableService 读取并校验可续费的服务：必须是在用中且非一次性付费。
func (s *BillingService) loadRenewableService(tx *gorm.DB, userID, serviceID uint) (*model.Service, error) {
	var svc model.Service
	if err := tx.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.Status != model.ServiceActive {
		return nil, ErrConflict("只有使用中的服务才能续费")
	}
	if svc.BillingCyc == model.CycleOneTime {
		return nil, ErrBadRequest("一次性付费服务无需续费")
	}
	return &svc, nil
}

// applyRenewalTx 执行续费的本地部分：只顺延到期时间，不碰钱、不建账单、不调上游。
// 上游续费必须在事务提交后由 reconcileUpstreamRenewal 完成，避免长耗时 RPC
// 占住 DB 事务（锁等待与连接耗尽）以及失败时误回滚已付账单。
func (s *BillingService) applyRenewalTx(tx *gorm.DB, svc *model.Service, userID uint, now time.Time) (time.Time, error) {
	// 从「现在」与「原到期时间」中取较晚者起算，剩余时长不缩水。
	base := now
	if svc.ExpiresAt != nil && svc.ExpiresAt.After(now) {
		base = *svc.ExpiresAt
	}
	next := model.AdvanceCycle(base, svc.BillingCyc)
	if err := tx.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{
			"next_due_at":     next,
			"expires_at":      next,
			"status":          model.ServiceActive,
			"provision_error": "",
		}).Error; err != nil {
		return time.Time{}, err
	}
	svc.NextDueAt = &next
	svc.ExpiresAt = &next
	svc.Status = model.ServiceActive
	svc.ProvisionError = ""
	return next, nil
}

// renewInTx settles a renewal using the caller's transaction.
func (s *BillingService) renewInTx(tx *gorm.DB, userID, serviceID uint, debit bool) (*RenewResult, error) {
	svc, err := s.loadRenewableService(tx, userID, serviceID)
	if err != nil {
		return nil, err
	}

	if debit && svc.PriceCents > 0 {
		// 扣款放在最前面：余额不足会在此直接失败，后续写入都不会发生。
		// 免费服务（0 元）无需扣款，直接跳过避免“金额不能为零”错误。
		if _, err := s.wallet.adjustBalance(
			tx, userID, -svc.PriceCents, model.TxPayment,
			"service", svc.ID, fmt.Sprintf("续费 %s", svc.Name),
		); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	if _, err := s.applyRenewalTx(tx, svc, userID, now); err != nil {
		return nil, err
	}

	invoiceNo, err := serialNo("INV")
	if err != nil {
		return nil, err
	}
	invoice := model.Invoice{
		InvoiceNo:  invoiceNo,
		UserID:     userID,
		Status:     model.InvoicePaid,
		TotalCents: svc.PriceCents,
		DueAt:      &now,
		PaidAt:     &now,
	}
	if err := tx.Create(&invoice).Error; err != nil {
		return nil, err
	}
	item := model.InvoiceItem{
		InvoiceID:   invoice.ID,
		ServiceID:   &svc.ID,
		Description: fmt.Sprintf("续费 %s（%s）", svc.Name, svc.BillingCyc),
		AmountCents: svc.PriceCents,
	}
	if err := tx.Create(&item).Error; err != nil {
		return nil, err
	}
	invoice.Items = []model.InvoiceItem{item}

	return &RenewResult{Service: svc, Invoice: &invoice}, nil
}

// CreateRenewalInvoice 为服务创建一个待付续费账单，支付环节复用统一收银台
// （余额全额 / 余额抵扣 + 外部支付），不再直接扣款。
// 同一服务最多保留一张待付续费账单：重复创建直接 409，附带已有账单号，
// 前端据此提示用户先去支付，避免刷出多张悬空账单。
func (s *BillingService) CreateRenewalInvoice(userID, serviceID uint) (*model.Invoice, error) {
	svc, err := s.loadRenewableService(s.db, userID, serviceID)
	if err != nil {
		return nil, err
	}
	var existing model.Invoice
	if err := s.db.Where("service_id = ? AND status = ?", svc.ID, model.InvoiceUnpaid).First(&existing).Error; err == nil {
		return nil, ErrConflict("该服务已有待支付的续费账单（%s），请先完成支付", existing.InvoiceNo)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	invoiceNo, err := serialNo("INV")
	if err != nil {
		return nil, err
	}
	invoice := model.Invoice{
		InvoiceNo:  invoiceNo,
		UserID:     userID,
		ServiceID:  &svc.ID,
		Status:     model.InvoiceUnpaid,
		TotalCents: svc.PriceCents,
		DueAt:      &now,
	}
	if err := s.db.Create(&invoice).Error; err != nil {
		return nil, err
	}
	item := model.InvoiceItem{
		InvoiceID:   invoice.ID,
		ServiceID:   &svc.ID,
		Description: fmt.Sprintf("续费 %s（%s）", svc.Name, svc.BillingCyc),
		AmountCents: svc.PriceCents,
	}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, err
	}
	invoice.Items = []model.InvoiceItem{item}
	return &invoice, nil
}

// settleRenewalInvoiceTx 结算一张续费账单：标记已付并顺延服务到期。
// 调用方负责先扣款（余额全额或意图创建时已抵扣），本方法只做记录与本地续期。
// 上游续费由最外层调用在事务提交后补做（见 reconcileUpstreamRenewal），此处不调上游。
func (s *BillingService) settleRenewalInvoiceTx(tx *gorm.DB, userID, invoiceID uint, now time.Time) (*model.Invoice, error) {
	var invoice model.Invoice
	if err := tx.Preload("Items").First(&invoice, "id = ? AND user_id = ?", invoiceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("账单不存在")
		}
		return nil, err
	}
	if invoice.Status != model.InvoiceUnpaid {
		return nil, ErrConflict("账单当前无需支付")
	}
	if invoice.ServiceID == nil || *invoice.ServiceID == 0 {
		return nil, ErrBadRequest("该账单不是续费账单")
	}
	svc, err := s.loadRenewableService(tx, userID, *invoice.ServiceID)
	if err != nil {
		return nil, err
	}
	if _, err := s.applyRenewalTx(tx, svc, userID, now); err != nil {
		return nil, err
	}
	res := tx.Model(&model.Invoice{}).
		Where("id = ? AND user_id = ? AND status = ?", invoice.ID, userID, model.InvoiceUnpaid).
		Updates(map[string]any{"status": model.InvoicePaid, "paid_at": now})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrConflict("账单状态已变更，请刷新后重试")
	}
	invoice.Status = model.InvoicePaid
	invoice.PaidAt = &now
	return &invoice, nil
}

// renewUpstream 向上游插件发起续费，返回上游给出的新到期时间（可能为 nil）。
// 插件内部会完成上游续费单的创建与余额支付；接口商品透传接口配置。
// 仅在事务提交后由 reconcileUpstreamRenewal 调用，超时由插件层的 120s 兜底。
func (s *BillingService) renewUpstream(svc *model.Service) (*time.Time, error) {
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用，无法续费该服务")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, svc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reply, err := s.plugins.ManageHost(ctx, svc.UpstreamPluginID, &pb.ManageHostRequest{
		HostId:          svc.UpstreamHostID,
		Action:          pb.HostAction_HOST_ACTION_RENEW,
		BillingCycle:    svc.BillingCyc,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, ErrBadRequest("上游续费失败: %v", err)
	}
	if !reply.GetSuccess() {
		return nil, ErrBadRequest("上游续费失败")
	}
	if e := reply.GetNewExpiry(); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			return &t, nil
		}
	}
	return nil, nil
}

// 电源操作动作名，与前端约定一致。
const (
	PowerBoot        = "boot"
	PowerShutdown    = "shutdown"
	PowerReboot      = "reboot"
	PowerHardBoot    = "hard_boot"
	PowerHardStop    = "hard_stop"
	PowerHardRestart = "hard_restart"
	PowerReinstall   = "reinstall"
)

// Power 对上游服务执行电源操作（开机/关机/重启/重装系统）。
// 仅上游对接的服务支持；本地服务没有电源概念。os 仅在 reinstall 时使用。
func (s *BillingService) Power(userID, serviceID uint, action string, os string) error {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("服务不存在")
		}
		return err
	}
	if svc.UpstreamPluginID == "" || svc.UpstreamHostID == "" {
		return ErrBadRequest("该服务不支持电源操作")
	}
	if svc.Status != model.ServiceActive {
		return ErrConflict("只有使用中的服务才能执行电源操作")
	}

	var pbAction pb.HostAction
	switch action {
	case PowerBoot:
		pbAction = pb.HostAction_HOST_ACTION_BOOT
	case PowerShutdown:
		pbAction = pb.HostAction_HOST_ACTION_SHUTDOWN
	case PowerReboot:
		pbAction = pb.HostAction_HOST_ACTION_REBOOT
	case PowerHardBoot:
		pbAction = pb.HostAction_HOST_ACTION_HARD_BOOT
	case PowerHardStop:
		pbAction = pb.HostAction_HOST_ACTION_HARD_STOP
	case PowerHardRestart:
		pbAction = pb.HostAction_HOST_ACTION_HARD_RESTART
	case PowerReinstall:
		pbAction = pb.HostAction_HOST_ACTION_REINSTALL
	default:
		return ErrBadRequest("无效的电源操作")
	}

	if s.plugins == nil {
		return ErrBadRequest("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, &svc)
	if err != nil {
		return err
	}
	reply, err := s.plugins.ManageHost(context.Background(), svc.UpstreamPluginID, &pb.ManageHostRequest{
		HostId:          svc.UpstreamHostID,
		Action:          pbAction,
		Os:              os,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return ErrBadRequest("上游操作失败: %v", err)
	}
	if !reply.GetSuccess() {
		return ErrBadRequest("上游操作失败")
	}
	return nil
}

// UpstreamInfo 返回上游主机详情（包含支持的操作列表）。
func (s *BillingService) UpstreamInfo(userID, serviceID uint) (*pb.UpstreamHost, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.UpstreamPluginID == "" || svc.UpstreamHostID == "" {
		return nil, ErrBadRequest("该服务未绑定上游")
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, &svc)
	if err != nil {
		return nil, err
	}
	reply, err := s.plugins.GetHost(context.Background(), svc.UpstreamPluginID, &pb.GetHostRequest{
		HostId:          svc.UpstreamHostID,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, ErrBadRequest("获取上游信息失败: %v", err)
	}
	if reply.GetHost() == nil {
		return nil, ErrBadRequest("上游未返回主机信息")
	}
	return reply.GetHost(), nil
}

// ServiceMetrics 返回上游主机的实时监控数据（CPU、内存、带宽）。
func (s *BillingService) ServiceMetrics(userID, serviceID uint) (*pb.HostMetrics, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.UpstreamPluginID == "" || svc.UpstreamHostID == "" {
		return nil, ErrBadRequest("该服务未绑定上游")
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, &svc)
	if err != nil {
		return nil, err
	}
	reply, err := s.plugins.GetHostMetrics(context.Background(), svc.UpstreamPluginID, &pb.GetHostMetricsRequest{
		HostId:          svc.UpstreamHostID,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, ErrBadRequest("获取实时监控失败: %v", err)
	}
	if reply.GetMetrics() == nil {
		return nil, ErrBadRequest("上游未返回监控数据")
	}
	return reply.GetMetrics(), nil
}

// ServiceVNC 返回上游主机的 VNC 接入信息（短票由主控签发，一次性、120 秒有效）。
func (s *BillingService) ServiceVNC(userID, serviceID uint) (*pb.HostVNC, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.UpstreamPluginID == "" || svc.UpstreamHostID == "" {
		return nil, ErrBadRequest("该服务未绑定上游")
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, &svc)
	if err != nil {
		return nil, err
	}
	reply, err := s.plugins.GetHostVNC(context.Background(), svc.UpstreamPluginID, &pb.GetHostVNCRequest{
		HostId:          svc.UpstreamHostID,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, ErrBadRequest("获取 VNC 信息失败: %v", err)
	}
	if reply.GetVnc() == nil || !reply.GetVnc().GetAvailable() {
		msg := reply.GetVnc().GetMessage()
		if msg == "" {
			msg = "上游暂未提供 VNC 控制台"
		}
		return nil, ErrBadRequest("%s", msg)
	}
	return reply.GetVnc(), nil
}

// ListOS 返回上游主机可用的重装系统列表。
func (s *BillingService) ListOS(userID, serviceID uint) ([]*pb.OSImage, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.UpstreamPluginID == "" || svc.UpstreamHostID == "" {
		return nil, ErrBadRequest("该服务未绑定上游")
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用")
	}
	ifaceConfig, err := interfaceConfigForService(s.db, &svc)
	if err != nil {
		return nil, err
	}
	reply, err := s.plugins.ListHostOS(context.Background(), svc.UpstreamPluginID, &pb.ListHostOSRequest{
		HostId:          svc.UpstreamHostID,
		InterfaceConfig: ifaceConfig,
	})
	if err != nil {
		return nil, ErrBadRequest("获取系统列表失败: %v", err)
	}
	return reply.GetOs(), nil
}
