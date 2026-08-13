package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// MaxCartQuantity 是单条购物车记录的数量上限。
const MaxCartQuantity = 99

// CartService 处理购物车。
type CartService struct {
	db *gorm.DB
}

// NewCartService 构造 CartService。
func NewCartService(db *gorm.DB) *CartService {
	return &CartService{db: db}
}

// CartView 是购物车视图，含小计。
type CartView struct {
	Items      []model.CartItem `json:"items"`
	TotalCents int64            `json:"total_cents"`
}

// List 返回用户购物车及总额。
func (s *CartService) List(userID uint) (*CartView, error) {
	var items []model.CartItem
	err := s.db.Preload("Product").Where("user_id = ?", userID).
		Order("id ASC").Find(&items).Error
	if err != nil {
		return nil, err
	}

	view := CartView{Items: make([]model.CartItem, 0, len(items))}
	for _, item := range items {
		// 商品被删除或下架时跳过，不计入总额，避免结账时金额与展示不一致。
		if item.Product == nil || item.Product.Status != model.ProductActive {
			continue
		}
		view.Items = append(view.Items, item)
		view.TotalCents += item.Product.PriceCents * int64(item.Quantity)
	}
	return &view, nil
}

// AddRequest 是加入购物车的入参。
type AddRequest struct {
	ProductID  uint   `json:"product_id"`
	Quantity   int    `json:"quantity"`
	BillingCyc string `json:"billing_cycle"`
}

// Add 把商品加入购物车。同一商品、同一周期已存在时累加数量。
func (s *CartService) Add(userID uint, req AddRequest) error {
	if req.Quantity <= 0 {
		req.Quantity = 1
	}
	if req.Quantity > MaxCartQuantity {
		return ErrBadRequest("单个商品数量不能超过 %d", MaxCartQuantity)
	}

	var product model.Product
	err := s.db.First(&product, "id = ? AND status = ?", req.ProductID, model.ProductActive).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("商品不存在或已下架")
		}
		return err
	}

	// 未指定周期时沿用商品默认周期。
	cycle := req.BillingCyc
	if cycle == "" {
		cycle = product.BillingCyc
	}
	if !model.ValidCycle(cycle) {
		return ErrBadRequest("无效的计费周期")
	}

	var existing model.CartItem
	err = s.db.First(&existing, "user_id = ? AND product_id = ? AND billing_cycle = ?",
		userID, req.ProductID, cycle).Error
	switch {
	case err == nil:
		quantity := existing.Quantity + req.Quantity
		if quantity > MaxCartQuantity {
			quantity = MaxCartQuantity
		}
		return s.db.Model(&existing).Update("quantity", quantity).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.db.Create(&model.CartItem{
			UserID:     userID,
			ProductID:  req.ProductID,
			BillingCyc: cycle,
			Quantity:   req.Quantity,
		}).Error
	default:
		return err
	}
}

// UpdateQuantity 修改购物车条目数量。
func (s *CartService) UpdateQuantity(userID, itemID uint, quantity int) error {
	if quantity <= 0 {
		return ErrBadRequest("数量必须大于零")
	}
	if quantity > MaxCartQuantity {
		return ErrBadRequest("单个商品数量不能超过 %d", MaxCartQuantity)
	}
	// WHERE 带 user_id，防止越权改他人购物车。
	result := s.db.Model(&model.CartItem{}).
		Where("id = ? AND user_id = ?", itemID, userID).
		Update("quantity", quantity)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("购物车条目不存在")
	}
	return nil
}

// Remove 删除购物车条目。
func (s *CartService) Remove(userID, itemID uint) error {
	result := s.db.Where("id = ? AND user_id = ?", itemID, userID).Delete(&model.CartItem{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("购物车条目不存在")
	}
	return nil
}

// Clear 清空用户购物车。
func (s *CartService) Clear(tx *gorm.DB, userID uint) error {
	return tx.Where("user_id = ?", userID).Delete(&model.CartItem{}).Error
}
