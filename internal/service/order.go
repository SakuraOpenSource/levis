package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// OrderService 处理下单与支付。
type OrderService struct {
	db      *gorm.DB
	cart    *CartService
	wallet  *WalletService
	plugins *plugin.Manager
}

// NewOrderService 构造 OrderService。plugins 可为 nil（测试或无插件场景），
// 此时上游商品仅本地开通，不会向上游下单。
func NewOrderService(db *gorm.DB, cart *CartService, wallet *WalletService, plugins *plugin.Manager) *OrderService {
	return &OrderService{db: db, cart: cart, wallet: wallet, plugins: plugins}
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
	return s.pay(userID, orderID, true)
}

// PayExternal settles an already verified external payment without debiting balance.
func (s *OrderService) PayExternal(userID, orderID uint) (*PayResult, error) {
	return s.pay(userID, orderID, false)
}

func (s *OrderService) pay(userID, orderID uint, debit bool) (*PayResult, error) {
	var out *PayResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.payInTx(tx, userID, orderID, debit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// payInTx settles an order using the caller's transaction. External payment
// finalization uses this helper so intent status and provisioning commit together.
func (s *OrderService) payInTx(tx *gorm.DB, userID, orderID uint, debit bool) (*PayResult, error) {
	var out PayResult
	var order model.Order
	err := tx.Preload("Items").
		First(&order, "id = ? AND user_id = ?", orderID, userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("订单不存在")
		}
		return nil, err
	}
	switch order.Status {
	case model.OrderPaid:
		return nil, ErrConflict("订单已支付")
	case model.OrderCancelled:
		return nil, ErrConflict("订单已取消")
	}
	if len(order.Items) == 0 {
		return nil, ErrBadRequest("订单没有明细")
	}

	if debit && order.TotalCents > 0 {
		// 扣款放在最前面：余额不足会在此直接失败，后续写入都不会发生。
		// 免费订单（总额为 0）无款可扣，直接跳过，否则会触发「金额不能为零」。
		if _, err := s.wallet.adjustBalance(
			tx, userID, -order.TotalCents, model.TxPayment,
			"order", order.ID, fmt.Sprintf("支付订单 %s", order.OrderNo),
		); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	// 用 RowsAffected 兜住并发重复支付：状态已变则本事务回滚。
	result := tx.Model(&model.Order{}).
		Where("id = ? AND status = ?", order.ID, model.OrderPending).
		Updates(map[string]any{"status": model.OrderPaid, "paid_at": now})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrConflict("订单状态已变更，请刷新后重试")
	}
	order.Status = model.OrderPaid
	order.PaidAt = &now

	invoiceNo, err := serialNo("INV")
	if err != nil {
		return nil, err
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
		return nil, err
	}

	services := make([]model.Service, 0, len(order.Items))
	invoiceItems := make([]model.InvoiceItem, 0, len(order.Items))
	for _, item := range order.Items {
		// 读取商品以判断是否为上游对接商品。
		var product model.Product
		if err := tx.First(&product, item.ProductID).Error; err != nil {
			return nil, err
		}

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

			// 上游对接商品：调用插件向上游下单并开通，把上游 host_id 存入服务。
			// 上游下单失败则整体回滚，绝不允许「扣了钱却没在上游开通」。
			if product.UpstreamPluginID != "" && product.UpstreamProductID != "" {
				hostID, upstreamExpiry, err := s.provisionUpstream(tx, userID, &product, item.BillingCyc, order.OrderNo)
				if err != nil {
					return nil, err
				}
				service.UpstreamPluginID = product.UpstreamPluginID
				service.UpstreamHostID = hostID
				// 上游返回了到期时间则以上游为准，保持两边一致。
				if upstreamExpiry != nil {
					service.NextDueAt = upstreamExpiry
					service.ExpiresAt = upstreamExpiry
				}
			}

			if err := tx.Create(&service).Error; err != nil {
				return nil, err
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
				return nil, res.Error
			}
		}
	}
	if err := tx.Create(&invoiceItems).Error; err != nil {
		return nil, err
	}
	invoice.Items = invoiceItems

	out = PayResult{Order: &order, Invoice: &invoice, Services: services}
	return &out, nil
}

// provisionUpstream 调用上游插件下单开通，返回上游服务实例 ID 与到期时间。
//
// 插件内部会完成上游的下单、结算与余额支付；返回的 upstream_order_id
// 即上游服务实例 ID（如魔方财务 host_id），后续管理操作都凭它定位。
// 开通成功后再调 GetHost 拉取到期时间，拉取失败不影响开通结果。
func (s *OrderService) provisionUpstream(tx *gorm.DB, userID uint, product *model.Product, cycle, orderNo string) (string, *time.Time, error) {
	if s.plugins == nil {
		return "", nil, ErrBadRequest("上游插件不可用，无法开通该商品")
	}

	// 上游可能用客户邮箱建账号，取下单用户的邮箱。
	var user model.User
	if err := tx.First(&user, userID).Error; err != nil {
		return "", nil, err
	}

	reply, err := s.plugins.CreateOrder(context.Background(), product.UpstreamPluginID, &pb.CreateOrderRequest{
		ProductId:    product.UpstreamProductID,
		BillingCycle: cycle,
		Quantity:     1,
		ClientEmail:  user.Email,
		Remark:       orderNo,
	})
	if err != nil {
		log.Printf("上游开通失败 plugin=%s product=%s order=%s: %v",
			product.UpstreamPluginID, product.UpstreamProductID, orderNo, err)
		return "", nil, ErrBadRequest("上游开通失败: %v", err)
	}

	hostID := reply.GetUpstreamOrderId()
	if hostID == "" {
		return "", nil, ErrBadRequest("上游开通失败: 未返回服务实例 ID")
	}

	// 拉取上游服务信息，同步到期时间；失败不阻断开通。
	var expiry *time.Time
	if host, err := s.plugins.GetHost(context.Background(), product.UpstreamPluginID, &pb.GetHostRequest{HostId: hostID}); err == nil {
		if e := host.GetHost().GetExpiry(); e != "" {
			if t, err := time.Parse(time.RFC3339, e); err == nil {
				expiry = &t
			}
		}
	}

	return hostID, expiry, nil
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
