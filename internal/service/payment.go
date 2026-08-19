package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	PluginID    string `json:"plugin_id"`
	AmountCents int64  `json:"amount_cents"`
}

func (s *PaymentService) Methods() ([]map[string]string, error) {
	if s.plugins == nil {
		return nil, ErrUnavailable("暂无可用的支付插件，请联系管理员配置")
	}
	ids := s.plugins.PaymentPlugins()
	if len(ids) == 0 {
		return nil, ErrUnavailable("暂无可用的支付插件，请联系管理员配置")
	}
	out := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]string{"id": id, "name": id})
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

func (s *PaymentService) Create(ctx context.Context, userID uint, in PaymentCreateInput) (*model.ExternalPayment, error) {
	if s.plugins == nil {
		return nil, ErrUnavailable("暂无可用的支付插件，请联系管理员配置")
	}
	if in.PluginID == "" {
		return nil, ErrBadRequest("请选择支付方式")
	}
	found := false
	for _, id := range s.plugins.PaymentPlugins() {
		if id == in.PluginID {
			found = true
			break
		}
	}
	if !found {
		return nil, ErrUnavailable("所选支付插件当前不可用")
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
	intent := &model.ExternalPayment{PluginID: in.PluginID, ExternalID: externalID, UserID: userID, Purpose: in.Purpose, TargetID: in.TargetID, AmountCents: amount, Currency: "CNY", Subject: subject, Status: model.ExternalPaymentPending}
	if err := s.db.Create(intent).Error; err != nil {
		return nil, err
	}
	reply, err := s.plugins.CreatePayment(ctx, in.PluginID, &pb.CreatePaymentRequest{ExternalId: externalID, AmountCents: amount, Currency: "CNY", Subject: subject, UserId: uint64(userID)})
	if err != nil {
		s.db.Model(intent).Updates(map[string]any{"status": model.ExternalPaymentFailed, "failure_reason": err.Error()})
		return nil, err
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
	reply, err := s.plugins.QueryPayment(ctx, item.PluginID, &pb.QueryPaymentRequest{ExternalId: item.ExternalID, GatewayRef: item.GatewayRef})
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
