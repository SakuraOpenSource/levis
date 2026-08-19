package service

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// BillingService 提供已购服务与账单的读取，以及服务的续费。
type BillingService struct {
	db     *gorm.DB
	wallet *WalletService
}

// NewBillingService 构造 BillingService。
func NewBillingService(db *gorm.DB, wallet *WalletService) *BillingService {
	return &BillingService{db: db, wallet: wallet}
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
	return out, nil
}

// renewInTx settles a renewal using the caller's transaction.
func (s *BillingService) renewInTx(tx *gorm.DB, userID, serviceID uint, debit bool) (*RenewResult, error) {
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

	if debit {
		// 扣款放在最前面：余额不足会在此直接失败，后续写入都不会发生。
		if _, err := s.wallet.adjustBalance(
			tx, userID, -svc.PriceCents, model.TxPayment,
			"service", svc.ID, fmt.Sprintf("续费 %s", svc.Name),
		); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	// 从「现在」与「原到期时间」中取较晚者起算，剩余时长不缩水。
	base := now
	if svc.ExpiresAt != nil && svc.ExpiresAt.After(now) {
		base = *svc.ExpiresAt
	}
	next := model.AdvanceCycle(base, svc.BillingCyc)
	if err := tx.Model(&model.Service{}).Where("id = ?", svc.ID).
		Updates(map[string]any{
			"next_due_at": next,
			"expires_at":  next,
			"status":      model.ServiceActive,
		}).Error; err != nil {
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

	svc.NextDueAt = &next
	svc.ExpiresAt = &next
	return &RenewResult{Service: &svc, Invoice: &invoice}, nil
}
