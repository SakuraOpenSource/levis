package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// BillingService 提供已购服务与账单的读取。
type BillingService struct {
	db *gorm.DB
}

// NewBillingService 构造 BillingService。
func NewBillingService(db *gorm.DB) *BillingService {
	return &BillingService{db: db}
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
