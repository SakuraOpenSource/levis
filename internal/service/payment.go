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
		out = append(out, map[string]string{"id": fmt.Sprint(m.ID), "name": m.Name})
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
	if in.PluginID == "" {
		return nil, ErrBadRequest("请选择支付方式")
	}
	// in.PluginID 预期为支付方式 ID（数字字符串）
	methodID, err := strconv.ParseUint(in.PluginID, 10, 64)
	if err != nil {
		return nil, ErrBadRequest("无效的支付方式")
	}
	var method model.PaymentMethod
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
	if in.Purpose != model.ExternalPaymentPurposeRecharge && in.TargetID == 0 {
		return nil, ErrBadRequest("缺少支付目标")
	}
	amount := in.AmountCents
	subject := "账户充值"
	switch in.Purpose {
	case model.ExternalPaymentPurposeRecharge:
		if amount <= 0 || amount > 100000000 {
			return nil, ErrBadRequest("充值金额必须大于零且不超过 1000000 元")
		}
	case model.ExternalPaymentPurposeOrder:
		var order model.Order
		if err := s.db.First(&order, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "订单")
		}
		if order.Status != model.OrderPending {
			return nil, ErrConflict("订单当前不可支付")
		}
		amount, subject = order.TotalCents, "支付订单 "+order.OrderNo
	case model.ExternalPaymentPurposeRenewal:
		var svc model.Service
		if err := s.db.First(&svc, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "服务")
		}
		if svc.Status != model.ServiceActive || svc.BillingCyc == model.CycleOneTime {
			return nil, ErrConflict("该服务当前不可续费")
		}
		amount, subject = svc.PriceCents, "续费 "+svc.Name
	case model.ExternalPaymentPurposeInvoice:
		var invoice model.Invoice
		if err := s.db.First(&invoice, "id = ? AND user_id = ?", in.TargetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "账单")
		}
		if invoice.Status != model.InvoiceUnpaid {
			return nil, ErrConflict("账单当前无需支付")
		}
		amount, subject = invoice.TotalCents, "支付账单 "+invoice.InvoiceNo
	default:
		return nil, ErrBadRequest("不支持的支付用途")
	}
	externalID, err := paymentExternalID()
	if err != nil {
		return nil, err
	}
	cfg := parsePaymentMethodConfig(method.Config)
	notifyURL := ""
	if s.plugins != nil {
		notifyURL = paymentMethodNotifyURL(s.plugins.APIBase(), method.PluginID, method.ID)
	}
	mid := method.ID
	intent := &model.ExternalPayment{PluginID: method.PluginID, ExternalID: externalID, UserID: userID, Purpose: in.Purpose, TargetID: in.TargetID, AmountCents: amount, Currency: "CNY", Subject: subject, Status: model.ExternalPaymentPending, PaymentMethodID: &mid}
	if err := s.db.Create(intent).Error; err != nil {
		return nil, err
	}
	reply, err := s.plugins.CreatePayment(ctx, method.PluginID, &pb.CreatePaymentRequest{ExternalId: externalID, AmountCents: amount, Currency: "CNY", Subject: subject, UserId: uint64(userID), ClientIp: clientIP, Config: cfg, NotifyUrl: notifyURL})
	if err != nil {
		s.db.Model(intent).Updates(map[string]any{"status": model.ExternalPaymentFailed, "failure_reason": err.Error()})
		return nil, ErrUnavailable("创建支付失败: %s", err.Error())
	}
	intent.PayURL, intent.GatewayRef = reply.GetPayUrl(), reply.GetGatewayRef()
	if err := s.db.Model(intent).Updates(map[string]any{"pay_url": intent.PayURL, "gateway_ref": intent.GatewayRef}).Error; err != nil {
		return nil, err
	}
	return intent, nil
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
	return s.db.Transaction(func(tx *gorm.DB) error {
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
		claim := tx.Model(&model.ExternalPayment{}).
			Where("id = ? AND status = ?", current.ID, model.ExternalPaymentPending).
			Update("status", model.ExternalPaymentProcessing)
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
			if _, err := s.orders.payInTx(tx, current.UserID, current.TargetID, false); err != nil {
				return err
			}
		case model.ExternalPaymentPurposeRenewal:
			if _, err := s.billing.renewInTx(tx, current.UserID, current.TargetID, false); err != nil {
				return err
			}
		case model.ExternalPaymentPurposeInvoice:
			var invoice model.Invoice
			if err := tx.First(&invoice, "id = ? AND user_id = ?", current.TargetID, current.UserID).Error; err != nil {
				return paymentTargetError(err, "账单")
			}
			if invoice.Status != model.InvoiceUnpaid {
				return ErrConflict("账单当前无需支付")
			}
			result := tx.Model(&model.Invoice{}).
				Where("id = ? AND user_id = ? AND status = ?", invoice.ID, current.UserID, model.InvoiceUnpaid).
				Updates(map[string]any{"status": model.InvoicePaid, "paid_at": now})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return ErrConflict("账单状态已变更")
			}
		default:
			return ErrBadRequest("不支持的支付用途")
		}
		return tx.Model(&current).Updates(map[string]any{"status": model.ExternalPaymentPaid, "paid_amount_cents": paidAmount, "paid_at": now}).Error
	})
}
