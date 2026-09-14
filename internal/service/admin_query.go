package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// AdminOrders 分页返回全部订单，支持按用户与状态过滤。
func (s *AdminService) AdminOrders(userID uint, status string, offset, limit int) ([]model.Order, int64, error) {
	query := s.db.Model(&model.Order{})
	if userID != 0 {
		query = query.Where("user_id = ?", userID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Order
	if err := query.Preload("Items").Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminOrder 读取单个订单（含明细）。
func (s *AdminService) AdminOrder(id uint) (*model.Order, error) {
	var order model.Order
	if err := s.db.Preload("Items").First(&order, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("订单不存在")
		}
		return nil, err
	}
	return &order, nil
}

// AdminInvoices 分页返回全部账单，支持按用户与状态过滤。
func (s *AdminService) AdminInvoices(userID uint, status string, offset, limit int) ([]model.Invoice, int64, error) {
	query := s.db.Model(&model.Invoice{})
	if userID != 0 {
		query = query.Where("user_id = ?", userID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Invoice
	if err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminInvoiceDetail 是管理端账单详情：账单字段平铺，附带关联的外部支付。
type AdminInvoiceDetail struct {
	model.Invoice
	ExternalPayments []model.ExternalPayment `json:"external_payments"`
}

// AdminInvoice 读取单个账单（含明细与关联的外部支付）。
func (s *AdminService) AdminInvoice(id uint) (*AdminInvoiceDetail, error) {
	var invoice model.Invoice
	if err := s.db.Preload("Items").First(&invoice, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("账单不存在")
		}
		return nil, err
	}
	out := &AdminInvoiceDetail{Invoice: invoice}
	var payments []model.ExternalPayment
	// 与该账单相关的支付：直接以账单为目标的，以及以其所属订单为目标的。
	if err := s.db.Where("purpose = ? AND target_id = ?", model.ExternalPaymentPurposeInvoice, invoice.ID).Find(&payments).Error; err != nil {
		return nil, err
	}
	out.ExternalPayments = payments
	if invoice.OrderID != nil {
		var orderPayments []model.ExternalPayment
		if err := s.db.Where("purpose = ? AND target_id = ?", model.ExternalPaymentPurposeOrder, *invoice.OrderID).Find(&orderPayments).Error; err != nil {
			return nil, err
		}
		out.ExternalPayments = append(out.ExternalPayments, orderPayments...)
	}
	if out.ExternalPayments == nil {
		out.ExternalPayments = []model.ExternalPayment{}
	}
	return out, nil
}

// AdminServices 跨用户分页返回服务，支持按用户、商品与状态过滤。
func (s *AdminService) AdminServices(userID, productID uint, status string, offset, limit int) ([]model.Service, int64, error) {
	query := s.db.Model(&model.Service{})
	if userID != 0 {
		query = query.Where("user_id = ?", userID)
	}
	if productID != 0 {
		query = query.Where("product_id = ?", productID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Service
	if err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminRetryProvision 以管理员身份重试任意用户的服务开通。
func (s *AdminService) AdminRetryProvision(serviceID uint) (*model.Service, error) {
	orders := NewOrderService(s.db, NewCartService(s.db), s.wallet, s.plugins)
	return orders.AdminRetryProvision(serviceID)
}
