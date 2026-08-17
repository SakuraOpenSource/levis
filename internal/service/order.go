package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// OrderService 处理下单与支付。
type OrderService struct {
	db     *gorm.DB
	cart   *CartService
	wallet *WalletService
}

// NewOrderService 构造 OrderService。
func NewOrderService(db *gorm.DB, cart *CartService, wallet *WalletService) *OrderService {
	return &OrderService{db: db, cart: cart, wallet: wallet}
}

// serialNo 生成带前缀的业务单号，形如 ORD20260813T1A2B3C4D。
func serialNo(prefix string) (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成单号失败: %w", err)
	}
	return fmt.Sprintf("%s%sT%s",
		prefix,
		time.Now().UTC().Format("20060102"),
		hex.EncodeToString(buf),
	), nil
}

// OrderLine 是一条待下单明细：买哪个商品、几份、按哪个周期计费。
//
// 购物车下单与开放接口直接下单都归约成一组 OrderLine，再交给
// buildOrderItems 统一校验与定价 —— 定价逻辑只有一份。
type OrderLine struct {
	ProductID  uint   `json:"product_id"`
	Quantity   int    `json:"quantity"`
	BillingCyc string `json:"billing_cycle"`
}

// MaxOrderLines 是单笔订单的明细条数上限。
const MaxOrderLines = 20

// buildOrderItems 校验明细并生成订单条目与总额。
//
// 价格与商品名一律从数据库实时读取后快照进条目，绝不采用调用方传入的金额 ——
// 这是全系统唯一的定价入口，购物车与开放接口共用。
func buildOrderItems(tx *gorm.DB, lines []OrderLine) ([]model.OrderItem, int64, error) {
	items := make([]model.OrderItem, 0, len(lines))
	var total int64
	for _, line := range lines {
		if line.Quantity <= 0 {
			return nil, 0, ErrBadRequest("商品数量必须大于零")
		}
		if line.Quantity > MaxCartQuantity {
			return nil, 0, ErrBadRequest("单个商品数量不能超过 %d", MaxCartQuantity)
		}

		var product model.Product
		err := tx.First(&product, "id = ? AND status = ?", line.ProductID, model.ProductActive).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, 0, ErrBadRequest("商品不存在或已下架")
			}
			return nil, 0, err
		}
		if product.Stock >= 0 && product.Stock < line.Quantity {
			return nil, 0, ErrBadRequest("商品「%s」库存不足", product.Name)
		}

		cycle := line.BillingCyc
		if cycle == "" {
			cycle = product.BillingCyc
		}
		if !model.ValidCycle(cycle) {
			return nil, 0, ErrBadRequest("无效的计费周期")
		}

		total += product.PriceCents * int64(line.Quantity)
		items = append(items, model.OrderItem{
			// 冗余快照：商品日后改价或改名，历史订单仍显示成交时的值。
			ProductID:   product.ID,
			ProductName: product.Name,
			PriceCents:  product.PriceCents,
			Quantity:    line.Quantity,
			BillingCyc:  cycle,
		})
	}
	return items, total, nil
}

// CreateFromCart 用当前购物车创建待支付订单。
//
// 全程在一个事务内完成：读购物车 → 建订单与明细 → 清空购物车。
func (s *OrderService) CreateFromCart(userID uint) (*model.Order, error) {
	var order model.Order
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var items []model.CartItem
		if err := tx.Where("user_id = ?", userID).Order("id ASC").Find(&items).Error; err != nil {
			return err
		}
		if len(items) == 0 {
			return ErrBadRequest("购物车为空")
		}

		lines := make([]OrderLine, 0, len(items))
		for _, item := range items {
			lines = append(lines, OrderLine{
				ProductID:  item.ProductID,
				Quantity:   item.Quantity,
				BillingCyc: item.BillingCyc,
			})
		}
		if err := s.create(tx, userID, lines, &order); err != nil {
			return err
		}
		return s.cart.Clear(tx, userID)
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// CreateDirect 按给定明细直接创建订单，不经过购物车。
//
// 开放接口用它下单：机器调用与用户浏览器里的购物车是两回事，共用一个购物车
// 会让 API 下单把用户正在挑的东西一并结掉。
func (s *OrderService) CreateDirect(userID uint, lines []OrderLine) (*model.Order, error) {
	if len(lines) == 0 {
		return nil, ErrBadRequest("请至少提供一条商品明细")
	}
	if len(lines) > MaxOrderLines {
		return nil, ErrBadRequest("单笔订单最多 %d 条明细", MaxOrderLines)
	}

	var order model.Order
	err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.create(tx, userID, lines, &order)
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// create 在事务内写入订单与明细。
func (s *OrderService) create(tx *gorm.DB, userID uint, lines []OrderLine, out *model.Order) error {
	orderItems, total, err := buildOrderItems(tx, lines)
	if err != nil {
		return err
	}

	no, err := serialNo("ORD")
	if err != nil {
		return err
	}
	order := model.Order{
		OrderNo:    no,
		UserID:     userID,
		Status:     model.OrderPending,
		TotalCents: total,
	}
	if err := tx.Create(&order).Error; err != nil {
		return err
	}
	for i := range orderItems {
		orderItems[i].OrderID = order.ID
	}
	if err := tx.Create(&orderItems).Error; err != nil {
		return err
	}
	order.Items = orderItems
	*out = order
	return nil
}

// PayResult 是支付结果。
type PayResult struct {
	Order    *model.Order    `json:"order"`
	Invoice  *model.Invoice  `json:"invoice"`
	Services []model.Service `json:"services"`
}

// Pay 用余额支付订单（当前为假支付，等待接入真实支付渠道）。
//
// 事务内依次完成：锁定订单 → 扣减余额并记流水 → 标记订单已付 → 开通服务 →
// 生成已付账单 → 扣减库存。任一步失败整体回滚，绝不会出现「扣了钱没开服务」
// 或「开了服务没扣钱」的中间态。
func (s *OrderService) Pay(userID, orderID uint) (*PayResult, error) {
	var out PayResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var order model.Order
		err := tx.Preload("Items").
			First(&order, "id = ? AND user_id = ?", orderID, userID).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound("订单不存在")
			}
			return err
		}
		switch order.Status {
		case model.OrderPaid:
			return ErrConflict("订单已支付")
		case model.OrderCancelled:
			return ErrConflict("订单已取消")
		}
		if len(order.Items) == 0 {
			return ErrBadRequest("订单没有明细")
		}

		// 扣款放在最前面：余额不足会在此直接失败，后续写入都不会发生。
		if _, err := s.wallet.adjustBalance(
			tx, userID, -order.TotalCents, model.TxPayment,
			"order", order.ID, fmt.Sprintf("支付订单 %s", order.OrderNo),
		); err != nil {
			return err
		}

		now := time.Now().UTC()
		// 用 RowsAffected 兜住并发重复支付：状态已变则本事务回滚。
		result := tx.Model(&model.Order{}).
			Where("id = ? AND status = ?", order.ID, model.OrderPending).
			Updates(map[string]any{"status": model.OrderPaid, "paid_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrConflict("订单状态已变更，请刷新后重试")
		}
		order.Status = model.OrderPaid
		order.PaidAt = &now

		invoiceNo, err := serialNo("INV")
		if err != nil {
			return err
		}
		invoice := model.Invoice{
			InvoiceNo:  invoiceNo,
			UserID:     userID,
			OrderID:    &order.ID,
			Status:     model.InvoicePaid,
			TotalCents: order.TotalCents,
			DueAt:      &now,
			PaidAt:     &now,
		}
		if err := tx.Create(&invoice).Error; err != nil {
			return err
		}

		services := make([]model.Service, 0, len(order.Items))
		invoiceItems := make([]model.InvoiceItem, 0, len(order.Items))
		for _, item := range order.Items {
			// 数量为 N 时开通 N 个独立服务实例，与魔方财务的行为一致。
			for i := 0; i < item.Quantity; i++ {
				service := model.Service{
					UserID:     userID,
					ProductID:  item.ProductID,
					OrderID:    order.ID,
					Name:       item.ProductName,
					Status:     model.ServiceActive,
					BillingCyc: item.BillingCyc,
					PriceCents: item.PriceCents,
				}
				// 一次性付费无续费与到期概念，两个时间字段均留空。
				if next := model.AdvanceCycle(now, item.BillingCyc); !next.IsZero() {
					service.NextDueAt = &next
					service.ExpiresAt = &next
				}
				if err := tx.Create(&service).Error; err != nil {
					return err
				}
				services = append(services, service)

				serviceID := service.ID
				invoiceItems = append(invoiceItems, model.InvoiceItem{
					InvoiceID:   invoice.ID,
					ServiceID:   &serviceID,
					Description: fmt.Sprintf("%s（%s）", item.ProductName, item.BillingCyc),
					AmountCents: item.PriceCents,
				})
			}

			// 库存为负表示不限量，跳过扣减。
			if item.Quantity > 0 {
				res := tx.Model(&model.Product{}).
					Where("id = ? AND stock >= 0", item.ProductID).
					UpdateColumn("stock", gorm.Expr("stock - ?", item.Quantity))
				if res.Error != nil {
					return res.Error
				}
			}
		}
		if err := tx.Create(&invoiceItems).Error; err != nil {
			return err
		}
		invoice.Items = invoiceItems

		out = PayResult{Order: &order, Invoice: &invoice, Services: services}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel 取消待支付订单。
func (s *OrderService) Cancel(userID, orderID uint) error {
	result := s.db.Model(&model.Order{}).
		Where("id = ? AND user_id = ? AND status = ?", orderID, userID, model.OrderPending).
		Update("status", model.OrderCancelled)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrConflict("订单不存在或状态不允许取消")
	}
	return nil
}

// List 分页返回用户订单。
func (s *OrderService) List(userID uint, offset, limit int) ([]model.Order, int64, error) {
	var (
		items []model.Order
		total int64
	)
	if err := s.db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := s.db.Preload("Items").Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Get 读取用户的单个订单。
func (s *OrderService) Get(userID, orderID uint) (*model.Order, error) {
	var order model.Order
	err := s.db.Preload("Items").First(&order, "id = ? AND user_id = ?", orderID, userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("订单不存在")
		}
		return nil, err
	}
	return &order, nil
}
