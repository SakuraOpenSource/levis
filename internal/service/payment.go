package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// PaymentService owns server-created external payment intents.
type PaymentService struct {
	db      *gorm.DB
	plugins *plugin.Manager
	wallet  *WalletService
	orders  *OrderService
	billing *BillingService
}

func NewPaymentService(db *gorm.DB, plugins *plugin.Manager, wallet *WalletService, orders *OrderService, billing *BillingService) *PaymentService {
	return &PaymentService{db: db, plugins: plugins, wallet: wallet, orders: orders, billing: billing}
}

type PaymentCreateInput struct {
	Purpose     string `json:"purpose"`
	TargetID    uint   `json:"target_id"`
	PluginID    string `json:"plugin_id"` // 支付方式 ID（数字字符串），兼容旧调用方也可能传插件 ID
	AmountCents int64  `json:"amount_cents"`
	// BalanceCents 是本次同步抵扣的余额（分），仅 order/invoice 用途有效。
	// 外部渠道只收剩余部分；抵扣部分在创建意图时即扣，取消意图时原路退回。
	BalanceCents int64 `json:"balance_cents"`
}

func (s *PaymentService) Methods() ([]map[string]string, error) {
	var methods []model.PaymentMethod
	if err := s.db.Where("enabled = ?", true).Order("sort_order ASC, id ASC").Find(&methods).Error; err != nil {
		return nil, err
	}
	if len(methods) == 0 {
		return nil, ErrUnavailable("暂无可用的支付方式，请联系管理员配置")
	}
	// 仅返回对应插件当前可用的方式；插件未运行时仍显示但创建时会失败
	out := make([]map[string]string, 0, len(methods))
	for _, m := range methods {
		out = append(out, map[string]string{"id": fmt.Sprint(m.ID), "name": m.Name, "icon": m.Icon})
	}
	return out, nil
}

func paymentExternalID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func parsePaymentMethodConfig(raw string) map[string]string {
	if raw == "" {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]string{}
	}
	if m == nil {
		return map[string]string{}
	}
	return m
}

func paymentMethodNotifyURL(apiBase, pluginID string, methodID uint) string {
	if apiBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/payment-notify/%s/%d", apiBase, pluginID, methodID)
}

func (s *PaymentService) Create(ctx context.Context, userID uint, clientIP string, in PaymentCreateInput) (*model.ExternalPayment, error) {
	if in.Purpose != model.ExternalPaymentPurposeRecharge && in.TargetID == 0 {
		return nil, ErrBadRequest("缺少支付目标")
	}
	if in.BalanceCents < 0 {
		return nil, ErrBadRequest("余额抵扣不能为负")
	}

	// 先按用途算出应付总额与展示标题；余额抵扣只对订单与账单开放。
	var total int64
	subject := "账户充值"
	allowBalance := false
	switch in.Purpose {
	case model.ExternalPaymentPurposeRecharge:
		if in.AmountCents <= 0 || in.AmountCents > 100000000 {
			return nil, ErrBadRequest("充值金额必须大于零且不超过 1000000 元")
		}
		total = in.AmountCents
	case model.ExternalPaymentPurposeOrder:
		var order model.Order
		if err := s.db.First(&order, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "订单")
		}
		if order.Status != model.OrderPending {
			return nil, ErrConflict("订单当前不可支付")
		}
		total, subject, allowBalance = order.TotalCents, "支付订单 "+order.OrderNo, true
	case model.ExternalPaymentPurposeRenewal:
		var svc model.Service
		if err := s.db.First(&svc, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "服务")
		}
		if svc.Status != model.ServiceActive || svc.BillingCyc == model.CycleOneTime {
			return nil, ErrConflict("该服务当前不可续费")
		}
		total, subject = svc.PriceCents, "续费 "+svc.Name
	case model.ExternalPaymentPurposeInvoice:
		var invoice model.Invoice
		if err := s.db.First(&invoice, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "账单")
		}
		if invoice.Status != model.InvoiceUnpaid {
			return nil, ErrConflict("账单当前无需支付")
		}
		total, subject, allowBalance = invoice.TotalCents, "支付账单 "+invoice.InvoiceNo, true
	default:
		return nil, ErrBadRequest("不支持的支付用途")
	}
	if total == 0 {
		return nil, ErrBadRequest("该订单/服务无需在线支付（金额为 0）")
	}
	if total < 0 {
		return nil, ErrBadRequest("金额不能为负")
	}
	balance := in.BalanceCents
	if balance > 0 && !allowBalance {
		return nil, ErrBadRequest("该支付用途不支持余额抵扣")
	}
	if balance > total {
		balance = total
	}
	external := total - balance

	// 余额全覆盖时不需要支付方式；否则必须指定可用方式。
	var method model.PaymentMethod
	var mid *uint
	var cfg map[string]string
	var pluginID string
	if external > 0 {
		if in.PluginID == "" {
			return nil, ErrBadRequest("请选择支付方式")
		}
		methodID, err := strconv.ParseUint(in.PluginID, 10, 64)
		if err != nil {
			return nil, ErrBadRequest("无效的支付方式")
		}
		if err := s.db.First(&method, uint(methodID)).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound("支付方式不存在")
			}
			return nil, err
		}
		if !method.Enabled {
			return nil, ErrUnavailable("该支付方式已停用")
		}
		if s.plugins == nil {
			return nil, ErrUnavailable("支付插件当前不可用")
		}
		inst, err := s.plugins.Get(method.PluginID)
		if err != nil || !inst.Has(pb.Capability_CAPABILITY_CREATE_PAYMENT) {
			return nil, ErrUnavailable("所选支付插件当前不可用")
		}
		if inst.Client() == nil {
			return nil, ErrUnavailable("支付插件未运行")
		}
		cfg = parsePaymentMethodConfig(method.Config)
		pluginID = method.PluginID
		m := method.ID
		mid = &m
	} else if in.PluginID != "" {
		// 余额全覆盖仍传了方式 ID：校验它存在且可用，结果里如实记录。
		if methodID, err := strconv.ParseUint(in.PluginID, 10, 64); err == nil {
			var m model.PaymentMethod
			if err := s.db.First(&m, uint(methodID)).Error; err == nil && m.Enabled {
				method, mid, cfg, pluginID = m, &m.ID, parsePaymentMethodConfig(m.Config), m.PluginID
			}
		}
	}

	externalID, err := paymentExternalID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	intent := &model.ExternalPayment{
		PluginID: pluginID, ExternalID: externalID, UserID: userID,
		Purpose: in.Purpose, TargetID: in.TargetID, AmountCents: external,
		BalanceCents: balance, Currency: "CNY", Subject: subject,
		Status: model.ExternalPaymentPending, PaymentMethodID: mid,
	}

	// 余额抵扣与意图创建同生共死；全余额时顺手把目标结算了。
	var pending *PayResult
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if balance > 0 {
			refType := string(in.Purpose)
			if _, err := s.wallet.adjustBalance(
				tx, userID, -balance, model.TxPayment,
				refType, in.TargetID, fmt.Sprintf("%s（余额抵扣）", subject),
			); err != nil {
				return err
			}
		}
		if err := tx.Create(intent).Error; err != nil {
			return err
		}
		if external == 0 {
			// 余额全覆盖：当场结算，无需再走渠道。
			// 余额已在上面扣足，这里 debit=false；订单会建好 pending 服务。
			res, err := settleTargetTx(tx, s.orders, s.billing, userID, in.Purpose, in.TargetID, now)
			if err != nil {
				return err
			}
			pending = res
			intent.Status = model.ExternalPaymentPaid
			intent.PaidAt = &now
			intent.PaidAmountCents = 0
			if err := tx.Model(intent).Updates(map[string]any{
				"status": model.ExternalPaymentPaid, "paid_at": now, "paid_amount_cents": 0,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if external == 0 {
		// 阶段二：订单开通与上游续费都在提交后进行，失败只记 failed，
		// 不影响已结算的支付。免费续费同样走此路径。
		if pending != nil {
			s.orders.provisionPending(pending)
		}
		s.finishRenewalUpstream(in.Purpose, in.TargetID)
		s.finishTrafficUpstream(in.Purpose, in.TargetID)
		return intent, nil
	}

	notifyURL := ""
	if s.plugins != nil {
		notifyURL = paymentMethodNotifyURL(s.plugins.APIBase(), method.PluginID, method.ID)
	}
	reply, err := s.plugins.CreatePayment(ctx, method.PluginID, &pb.CreatePaymentRequest{ExternalId: externalID, AmountCents: external, Currency: "CNY", Subject: subject, UserId: uint64(userID), ClientIp: clientIP, Config: cfg, NotifyUrl: notifyURL})
	if err != nil {
		// 渠道建单失败：意图作废，已抵扣的余额原路退回。
		_ = s.db.Transaction(func(tx *gorm.DB) error {
			if balance > 0 {
				if _, err := s.wallet.adjustBalance(
					tx, userID, balance, model.TxRefund,
					"external_payment", intent.ID, fmt.Sprintf("%s（建单失败退回抵扣）", subject),
				); err != nil {
					return err
				}
			}
			return tx.Model(&model.ExternalPayment{}).Where("id = ?", intent.ID).Updates(map[string]any{
				"status": model.ExternalPaymentFailed, "failure_reason": err.Error(),
			}).Error
		})
		return nil, ErrUnavailable("创建支付失败: %s", err.Error())
	}
	intent.PayURL, intent.GatewayRef = reply.GetPayUrl(), reply.GetGatewayRef()
	if err := s.db.Model(intent).Updates(map[string]any{"pay_url": intent.PayURL, "gateway_ref": intent.GatewayRef}).Error; err != nil {
		return nil, err
	}
	return intent, nil
}

// settleTargetTx 结算一张待付目标：订单走开通，续费/订单账单按种类处理。
// balance 的扣减由调用方完成（意图创建时已抵扣，或余额全额路径当场扣足）。
// 续费类目标此处只做本地续期与账单落账，上游续费由最外层在提交后补做。
func settleTargetTx(tx *gorm.DB, orders *OrderService, billing *BillingService, userID uint, purpose string, targetID uint, now time.Time) (*PayResult, error) {
	switch purpose {
	case model.ExternalPaymentPurposeOrder:
		return orders.payInTx(tx, userID, targetID, false)
	case model.ExternalPaymentPurposeInvoice:
		return settleInvoiceTx(tx, orders, billing, userID, targetID, now)
	case model.ExternalPaymentPurposeRenewal:
		if _, err := billing.renewInTx(tx, userID, targetID, false); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, ErrBadRequest("不支持的支付用途")
	}
}

// finishRenewalUpstream 是续费类支付提交后的上游对账：钱已落账，上游续费
// 失败只记 failed（见 reconcileUpstreamRenewal），不回滚支付。
func (s *PaymentService) finishRenewalUpstream(purpose string, targetID uint) {
	switch purpose {
	case model.ExternalPaymentPurposeRenewal:
		s.billing.reconcileUpstreamRenewal(targetID)
	case model.ExternalPaymentPurposeInvoice:
		var invoice model.Invoice
		if err := s.db.Preload("Items").First(&invoice, "id = ?", targetID).Error; err != nil {
			return
		}
		// 流量包账单不顺延到期、不走续费对账，由 finishTrafficUpstream 处理。
		if isTrafficInvoice(&invoice) {
			return
		}
		if invoice.ServiceID != nil && *invoice.ServiceID != 0 {
			s.billing.reconcileUpstreamRenewal(*invoice.ServiceID)
		}
	}
}

// finishTrafficUpstream 是流量包支付提交后的上游通知：配额已在本地累加，
// 上游通知失败只记日志（见 ReconcileTrafficUpstream），不回滚支付。
func (s *PaymentService) finishTrafficUpstream(purpose string, targetID uint) {
	if purpose != model.ExternalPaymentPurposeInvoice {
		return
	}
	var invoice model.Invoice
	if err := s.db.Preload("Items").First(&invoice, "id = ?", targetID).Error; err != nil {
		return
	}
	if !isTrafficInvoice(&invoice) {
		return
	}
	extraGB, ok := parseTrafficDescription(invoice.Items[0].Description)
	if !ok {
		return
	}
	s.billing.ReconcileTrafficUpstream(*invoice.ServiceID, extraGB)
}

// settleInvoiceTx 结算一张待付账单：订单账单走订单开通并返回 PayResult，
// 续费账单顺延到期，流量包账单累加配额，无归属的孤账单仅标记已付。
func settleInvoiceTx(tx *gorm.DB, orders *OrderService, billing *BillingService, userID, invoiceID uint, now time.Time) (*PayResult, error) {
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
	if invoice.OrderID != nil && *invoice.OrderID != 0 {
		return orders.payInTx(tx, userID, *invoice.OrderID, false)
	}
	// 流量包账单同样挂 ServiceID，必须先于续费分支识别，否则会被误顺延到期。
	if isTrafficInvoice(&invoice) {
		if _, err := billing.settleTrafficInvoiceTx(tx, userID, invoice.ID, now); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if invoice.ServiceID != nil && *invoice.ServiceID != 0 {
		if _, err := billing.settleRenewalInvoiceTx(tx, userID, invoice.ID, now); err != nil {
			return nil, err
		}
		return nil, nil
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
	return nil, nil
}

// SettleInvoice 用余额全额结清一张待付账单（统一收银台的余额分支）。
// 订单账单走开通，续费账单本地顺延到期；提交后按需跑阶段二开通与上游续费。
func (s *PaymentService) SettleInvoice(userID, invoiceID uint) (*model.Invoice, error) {
	var out *model.Invoice
	var pending *PayResult
	now := time.Now().UTC()
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var invoice model.Invoice
		if err := tx.First(&invoice, "id = ? AND user_id = ?", invoiceID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound("账单不存在")
			}
			return err
		}
		if invoice.Status != model.InvoiceUnpaid {
			return ErrConflict("账单当前无需支付")
		}
		if invoice.TotalCents > 0 {
			if _, err := s.wallet.adjustBalance(
				tx, userID, -invoice.TotalCents, model.TxPayment,
				"invoice", invoice.ID, fmt.Sprintf("支付账单 %s", invoice.InvoiceNo),
			); err != nil {
				return err
			}
		}
		res, err := settleInvoiceTx(tx, s.orders, s.billing, userID, invoice.ID, now)
		if err != nil {
			return err
		}
		pending = res
		return tx.Preload("Items").First(&out, invoice.ID).Error
	})
	if err != nil {
		return nil, err
	}
	if pending != nil {
		s.orders.provisionPending(pending)
	}
	// 流量包账单只加配额、不顺延到期：跳过续费对账，走流量包上游通知。
	if out != nil && isTrafficInvoice(out) {
		if len(out.Items) > 0 {
			if extraGB, ok := parseTrafficDescription(out.Items[0].Description); ok && out.ServiceID != nil {
				s.billing.ReconcileTrafficUpstream(*out.ServiceID, extraGB)
			}
		}
		return out, nil
	}
	// 续费账单提交后再调上游，失败只记 failed，不回滚已付账单。
	if out != nil && out.ServiceID != nil && *out.ServiceID != 0 {
		s.billing.reconcileUpstreamRenewal(*out.ServiceID)
	}
	return out, nil
}

// Cancel 取消一笔待支付意图：已抵扣的余额原路退回，意图置为失败。
func (s *PaymentService) Cancel(userID, id uint) (*model.ExternalPayment, error) {
	var out model.ExternalPayment
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var current model.ExternalPayment
		if err := tx.First(&current, "id = ? AND user_id = ?", id, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound("支付记录不存在")
			}
			return err
		}
		if current.Status != model.ExternalPaymentPending {
			return ErrConflict("只有待支付的订单可以取消")
		}
		if current.BalanceCents > 0 {
			if _, err := s.wallet.adjustBalance(
				tx, userID, current.BalanceCents, model.TxRefund,
				"external_payment", current.ID, fmt.Sprintf("%s（取消支付退回抵扣）", current.Subject),
			); err != nil {
				return err
			}
		}
		reason := "用户取消"
		if current.BalanceCents > 0 {
			reason = "用户取消，已退回余额抵扣"
		}
		if err := tx.Model(&current).Updates(map[string]any{
			"status": model.ExternalPaymentFailed, "failure_reason": reason,
		}).Error; err != nil {
			return err
		}
		out = current
		out.Status = model.ExternalPaymentFailed
		out.FailureReason = reason
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func paymentTargetError(err error, name string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound("%s不存在", name)
	}
	return err
}

func (s *PaymentService) Get(userID, id uint) (*model.ExternalPayment, error) {
	var item model.ExternalPayment
	if err := s.db.First(&item, "id = ? AND user_id = ?", id, userID).Error; err != nil {
		return nil, paymentTargetError(err, "支付记录")
	}
	return &item, nil
}

func (s *PaymentService) Query(ctx context.Context, userID, id uint) (*model.ExternalPayment, error) {
	item, err := s.Get(userID, id)
	if err != nil {
		return nil, err
	}
	if item.Status != model.ExternalPaymentPending {
		return item, nil
	}
	if s.plugins == nil {
		return nil, ErrUnavailable("支付插件当前不可用")
	}
	cfg := map[string]string{}
	if item.PaymentMethodID != nil {
		var method model.PaymentMethod
		if err := s.db.First(&method, *item.PaymentMethodID).Error; err == nil {
			cfg = parsePaymentMethodConfig(method.Config)
		}
	} else {
		// 兼容旧数据：按 plugin 查第一个启用的方式
		var method model.PaymentMethod
		if err := s.db.Where("plugin_id = ? AND enabled = ?", item.PluginID, true).Order("sort_order ASC, id ASC").First(&method).Error; err == nil {
			cfg = parsePaymentMethodConfig(method.Config)
		}
	}
	reply, err := s.plugins.QueryPayment(ctx, item.PluginID, &pb.QueryPaymentRequest{ExternalId: item.ExternalID, GatewayRef: item.GatewayRef, Config: cfg})
	if err != nil {
		return nil, err
	}
	if reply.GetState() != pb.PaymentState_PAYMENT_STATE_PAID {
		return item, nil
	}
	if reply.GetPaidAmountCents() != item.AmountCents {
		return nil, ErrConflict("支付金额与订单金额不一致")
	}
	now := time.Now().UTC()
	if err := s.finalize(item, reply.GetPaidAmountCents(), now); err != nil {
		return nil, err
	}
	return s.Get(userID, id)
}

// FinalizeCallback handles a gateway callback by locating the intent and settling it.
func (s *PaymentService) FinalizeCallback(ctx context.Context, pluginID, externalID string, paidAmount int64, gatewayRef string) error {
	var item model.ExternalPayment
	if err := s.db.First(&item, "plugin_id = ? AND external_id = ?", pluginID, externalID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("支付记录不存在")
		}
		return err
	}
	if item.Status != model.ExternalPaymentPending {
		return nil
	}
	now := time.Now().UTC()
	if err := s.finalize(&item, paidAmount, now); err != nil {
		return err
	}
	if gatewayRef != "" && item.GatewayRef != gatewayRef {
		_ = s.db.Model(&item).Update("gateway_ref", gatewayRef).Error
	}
	return nil
}

func (s *PaymentService) finalize(item *model.ExternalPayment, paidAmount int64, now time.Time) error {
	var pending *PayResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var current model.ExternalPayment
		if err := tx.First(&current, item.ID).Error; err != nil {
			return err
		}
		if current.Status == model.ExternalPaymentPaid {
			if current.PaidAmountCents != 0 && current.PaidAmountCents != paidAmount {
				return ErrConflict("重复支付回报金额不一致")
			}
			return nil
		}
		if current.Status != model.ExternalPaymentPending {
			return ErrConflict("支付状态已变更")
		}
		if paidAmount != current.AmountCents {
			return ErrConflict("支付金额与订单金额不一致")
		}
		claim := tx.Model(&model.ExternalPayment{}).Where("id = ? AND status = ?", current.ID, model.ExternalPaymentPending).Update("status", model.ExternalPaymentProcessing)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return ErrConflict("支付状态已变更")
		}
		switch current.Purpose {
		case model.ExternalPaymentPurposeRecharge:
			if _, err := s.wallet.adjustBalance(tx, current.UserID, current.AmountCents, model.TxRecharge, "external_payment", current.ID, fmt.Sprintf("插件支付充值 %d", current.ID)); err != nil {
				return err
			}
		case model.ExternalPaymentPurposeOrder:
			res, err := s.orders.payInTx(tx, current.UserID, current.TargetID, false)
			if err != nil {
				return err
			}
			pending = res
		case model.ExternalPaymentPurposeRenewal:
			// 事务内只做本地续期与账单落账，上游续费提交后补做。
			if _, err := s.billing.renewInTx(tx, current.UserID, current.TargetID, false); err != nil {
				return err
			}
		case model.ExternalPaymentPurposeInvoice:
			// 余额抵扣部分已在创建意图时扣除，这里只结算剩余渠道款。
			// 订单账单走开通并返回待开通服务，续费账单本地顺延到期，上游提交后补做。
			res, err := settleInvoiceTx(tx, s.orders, s.billing, current.UserID, current.TargetID, now)
			if err != nil {
				return err
			}
			pending = res
		default:
			return ErrBadRequest("不支持的支付用途")
		}
		return tx.Model(&current).Updates(map[string]any{"status": model.ExternalPaymentPaid, "paid_amount_cents": paidAmount, "paid_at": now}).Error
	})
	if err != nil {
		return err
	}
	// 阶段二：事务已提交，再逐个开通/向上游续费，失败只记 failed，不影响已结算的支付。
	if pending != nil {
		s.orders.provisionPending(pending)
	}
	s.finishRenewalUpstream(item.Purpose, item.TargetID)
	s.finishTrafficUpstream(item.Purpose, item.TargetID)
	return nil
}
