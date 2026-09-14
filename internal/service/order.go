package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
)

// OrderService 处理下单与支付。
type OrderService struct {
	db      *gorm.DB
	cart    *CartService
	wallet  *WalletService
	plugins *plugin.Manager
}

// NewOrderService 构造 OrderService。plugins 可为 nil（测试或无插件场景），
// 此时上游商品先建待开通服务，实际开通时再报插件不可用。
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
	// Options 是购买时的选配（弹性规格与系统镜像）。接口商品必填，
	// 其余商品忽略。键约定：cpu / memory_mb / disk_gb / bandwidth_mbps /
	// traffic_gb / image_id，值一律为字符串。
	Options map[string]string `json:"options"`
}

// MaxOrderLines 是单笔订单的明细条数上限。
const MaxOrderLines = 20

// buildOrderItems 校验明细并生成订单条目与总额。
//
// 价格与商品名一律从数据库实时读取后快照进条目，绝不采用调用方传入的金额 ——
// 这是全系统唯一的定价入口，购物车与开放接口共用。
// userID 非零且代理加盟开启时，按用户等级的分组折扣折算（小分组覆盖大分组）。
func buildOrderItems(tx *gorm.DB, userID uint, lines []OrderLine) ([]model.OrderItem, int64, error) {
	items := make([]model.OrderItem, 0, len(lines))
	var total int64
	// 代理折扣一次性判定：等级由余额实时推导，折扣按分组沿父链解析。
	var agents *AgentProgramService
	var tier *model.AgentTier
	if userID != 0 {
		agents = NewAgentProgramService(tx)
		if agents.Enabled() {
			var user model.User
			if err := tx.First(&user, userID).Error; err == nil {
				tier = agents.TierForBalance(user.BalanceCents)
			}
		}
	}
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

		unitPrice := product.PriceCents
		if product.InterfaceID != 0 {
			// 接口商品的选配价格以商品基础价为底，再叠加每项资源的弹性加价。
			// 先校验选配，避免对越界或未对齐的值计价。
			if err := validateProvisionOptions(product.ProvisionConfig, line.Options); err != nil {
				return nil, 0, err
			}
			unitPrice += provisionOptionPrice(product.ProvisionConfig, line.Options)
		}
		discountPermille := 0
		if agents != nil && tier != nil {
			if permille, ok := agents.DiscountFor(tier.ID, product.CategoryID); ok && permille > 0 && permille < 1000 {
				discountPermille = permille
				unitPrice = unitPrice * int64(permille) / 1000
			}
		}
		total += unitPrice * int64(line.Quantity)
		options := model.OptionMap(nil)
		if product.InterfaceID != 0 {
			options = model.OptionMap(line.Options)
		}
		items = append(items, model.OrderItem{
			// 冗余快照：商品日后改价或改名，历史订单仍显示成交时的值。
			ProductID:   product.ID,
			ProductName: product.Name,
			PriceCents:  unitPrice,
			Quantity:    line.Quantity,
			BillingCyc:  cycle,

			DiscountPermille: discountPermille,
			Options:          options,
		})
	}
	return items, total, nil
}

// validateProvisionOptions 校验接口商品的购买选配。
//
// 五项规格（CPU 核数、内存 MB、硬盘 GB、带宽 Mbps、流量 GB）都必须给出，
// 且落在商品配置的区间内 —— 固定规格商品的区间退化为单点，等于必须一致；
// 系统镜像必填。流量为 0 表示不限，是否允许 0 由区间决定。
func validateProvisionOptions(cfg model.ProvisionSpec, options map[string]string) error {
	if cfg.Driver == "" {
		return nil
	}
	numeric := []struct {
		key   string
		rng   model.SpecRange
		label string
	}{
		{"cpu", cfg.CPU, "CPU 核数"},
		{"memory_mb", cfg.MemoryMB, "内存"},
		{"disk_gb", cfg.DiskGB, "硬盘"},
		{"bandwidth_mbps", cfg.BandwidthMbps, "带宽"},
		{"traffic_gb", cfg.TrafficGB, "流量"},
	}
	for _, item := range numeric {
		raw, ok := options[item.key]
		if !ok || raw == "" {
			return ErrBadRequest("请选择%s", item.label)
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return ErrBadRequest("%s格式不正确", item.label)
		}
		if value < item.rng.Min || value > item.rng.Max {
			return ErrBadRequest("%s超出可选范围（%d-%d）", item.label, item.rng.Min, item.rng.Max)
		}
		step := item.rng.Step
		if step <= 0 {
			// 历史弹性配置没有 Step 字段，按 1 兼容读取；新配置由
			// normalizeProvisionConfig 保证步长必须显式大于 0。
			step = 1
		}
		if (value-item.rng.Min)%step != 0 {
			return ErrBadRequest("%s必须按 %d 的步长选择", item.label, step)
		}
	}
	if options["image_id"] == "" {
		return ErrBadRequest("请选择操作系统")
	}
	return nil
}

// provisionOptionPrice 返回接口商品选配相对基础规格的加价。
// 缺失的历史 Step/UnitPriceCents 按 1/0 处理，确保旧商品仍可购买。
func provisionOptionPrice(cfg model.ProvisionSpec, options map[string]string) int64 {
	numeric := []struct {
		key string
		rng model.SpecRange
	}{
		{"cpu", cfg.CPU},
		{"memory_mb", cfg.MemoryMB},
		{"disk_gb", cfg.DiskGB},
		{"bandwidth_mbps", cfg.BandwidthMbps},
		{"traffic_gb", cfg.TrafficGB},
	}
	var extra int64
	for _, item := range numeric {
		value, err := strconv.Atoi(options[item.key])
		if err != nil {
			continue
		}
		step := item.rng.Step
		if step <= 0 {
			step = 1
		}
		extra += int64((value-item.rng.Min)/step) * item.rng.UnitPriceCents
	}
	return extra
}

// checkAgreement 校验购买协议：商品绑定了协议文章时必须传 agree=true。
func checkAgreement(tx *gorm.DB, lines []OrderLine, agree bool) error {
	if agree {
		return nil
	}
	for _, line := range lines {
		var product model.Product
		if err := tx.First(&product, line.ProductID).Error; err != nil {
			continue
		}
		if product.AgreementArticleID != nil {
			return ErrBadRequest("请先阅读并同意商品协议后再下单")
		}
	}
	return nil
}

// isUpstreamProduct 报告商品是否为上游对接商品。
func isUpstreamProduct(product *model.Product) bool {
	if product.InterfaceID != 0 {
		return true
	}
	return product.UpstreamPluginID != "" && product.UpstreamProductID != ""
}

// truncateProvisionError 把上游错误截断到服务表的字段长度。
func truncateProvisionError(s string) string {
	r := []rune(s)
	if len(r) > 500 {
		return string(r[:500])
	}
	return s
}

// CreateFromCart 用当前购物车创建待支付订单，同时建出待付账单。
//
// 全程在一个事务内完成：读购物车 → 建订单与明细 → 建待付账单 → 清空购物车。
// agree 为 true 表示用户已同意商品绑定的购买协议。
func (s *OrderService) CreateFromCart(userID uint, agree ...bool) (*model.Order, error) {
	agreed := len(agree) > 0 && agree[0]
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
		if err := s.create(tx, userID, lines, agreed, &order); err != nil {
			return err
		}
		return s.cart.Clear(tx, userID)
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// CreateDirect 按给定明细直接创建订单，不经过购物车，同时建出待付账单。
//
// 开放接口用它下单：机器调用与用户浏览器里的购物车是两回事，共用一个购物车
// 会让 API 下单把用户正在挑的东西一并结掉。
func (s *OrderService) CreateDirect(userID uint, lines []OrderLine, agree ...bool) (*model.Order, error) {
	if len(lines) == 0 {
		return nil, ErrBadRequest("请至少提供一条商品明细")
	}
	if len(lines) > MaxOrderLines {
		return nil, ErrBadRequest("单笔订单最多 %d 条明细", MaxOrderLines)
	}
	agreed := len(agree) > 0 && agree[0]

	var order model.Order
	err := s.db.Transaction(func(tx *gorm.DB) error {
		return s.create(tx, userID, lines, agreed, &order)
	})
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// create 在事务内写入订单、明细与待付账单。
//
// 账单在下单时即建（unpaid），支付时只标记已付：外部支付已收钱后本地必须有
// 对应的待付账单可结算，否则会出现「钱已收、账对不上」的悬空。
func (s *OrderService) create(tx *gorm.DB, userID uint, lines []OrderLine, agree bool, out *model.Order) error {
	if len(lines) == 0 {
		return ErrBadRequest("请至少提供一条商品明细")
	}
	if len(lines) > MaxOrderLines {
		return ErrBadRequest("单笔订单最多 %d 条明细", MaxOrderLines)
	}
	orderItems, total, err := buildOrderItems(tx, userID, lines)
	if err != nil {
		return err
	}
	if err := checkAgreement(tx, lines, agree); err != nil {
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

	// 下单即建待付账单，明细按商品快照逐份展开，此时不关联服务。
	now := time.Now().UTC()
	invoiceNo, err := serialNo("INV")
	if err != nil {
		return err
	}
	invoice := model.Invoice{
		InvoiceNo:  invoiceNo,
		UserID:     userID,
		OrderID:    &order.ID,
		Status:     model.InvoiceUnpaid,
		TotalCents: total,
		DueAt:      &now,
	}
	if err := tx.Create(&invoice).Error; err != nil {
		return err
	}
	invoiceItems := make([]model.InvoiceItem, 0, len(orderItems))
	for _, item := range orderItems {
		for i := 0; i < item.Quantity; i++ {
			invoiceItems = append(invoiceItems, model.InvoiceItem{
				InvoiceID:   invoice.ID,
				Description: fmt.Sprintf("%s（%s）", item.ProductName, item.BillingCyc),
				AmountCents: item.PriceCents,
			})
		}
	}
	if len(invoiceItems) > 0 {
		if err := tx.Create(&invoiceItems).Error; err != nil {
			return err
		}
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

// Pay 用余额支付订单并开通服务。
//
// 两阶段提交：事务内只做钱与记录（扣款→订单已付→账单已付→建服务，其中上游
// 商品只建 pending 待开通），事务提交后再逐个向上游开通。开通失败只把服务
// 置为 failed 并记录原因，钱不退，可重试。
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
	s.provisionPending(out)
	return out, nil
}

// payInTx settles an order using the caller's transaction. External payment
// finalization uses this helper so intent status and provisioning commit together.
// 本方法只做阶段一（钱与记录）：上游商品建 pending 服务，开通放到提交后。
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

	// 下单时已建待付账单，这里只标记已付；兼容历史订单（无账单时新建）。
	var invoice model.Invoice
	if err := tx.Where("order_id = ?", order.ID).First(&invoice).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		invoiceNo, err := serialNo("INV")
		if err != nil {
			return nil, err
		}
		invoice = model.Invoice{
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
	} else {
		if invoice.Status == model.InvoicePaid {
			return nil, ErrConflict("账单已支付")
		}
		if invoice.Status != model.InvoiceUnpaid {
			return nil, ErrConflict("账单状态已变更，请刷新后重试")
		}
		res := tx.Model(&model.Invoice{}).
			Where("id = ? AND status = ?", invoice.ID, model.InvoiceUnpaid).
			Updates(map[string]any{"status": model.InvoicePaid, "paid_at": now})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			return nil, ErrConflict("账单状态已变更，请刷新后重试")
		}
		invoice.Status = model.InvoicePaid
		invoice.PaidAt = &now
	}

	services := make([]model.Service, 0, len(order.Items))
	needsLink := true
	var existingItems []model.InvoiceItem
	if invoice.ID != 0 {
		_ = tx.Where("invoice_id = ?", invoice.ID).Order("id ASC").Find(&existingItems).Error
	}

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

			if isUpstreamProduct(&product) {
				// 阶段一不调上游：只建 pending 服务并记下插件，提交后再开通。
				pluginID, _, err := resolvePluginForProduct(tx, &product)
				if err != nil {
					service.Status = model.ServiceFailed
					service.ProvisionError = truncateProvisionError(err.Error())
					log.Printf("上游开通预检失败 product=%d order=%s: %v", product.ID, order.OrderNo, err)
				} else {
					service.Status = model.ServicePending
					service.UpstreamPluginID = pluginID
				}
			}

			if err := tx.Create(&service).Error; err != nil {
				return nil, err
			}
			services = append(services, service)
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
	// 把下单时建的待付账单明细关联到新服务；数量对不上时只关联能对上的。
	if needsLink && len(existingItems) > 0 && len(existingItems) == len(services) {
		for i := range services {
			sid := services[i].ID
			if err := tx.Model(&model.InvoiceItem{}).Where("id = ?", existingItems[i].ID).Update("service_id", sid).Error; err != nil {
				return nil, err
			}
			existingItems[i].ServiceID = &sid
		}
		invoice.Items = existingItems
	} else if len(existingItems) == 0 {
		invoiceItems := make([]model.InvoiceItem, 0, len(services))
		for _, svc := range services {
			sid := svc.ID
			invoiceItems = append(invoiceItems, model.InvoiceItem{
				InvoiceID:   invoice.ID,
				ServiceID:   &sid,
				Description: fmt.Sprintf("%s", svc.Name),
				AmountCents: svc.PriceCents,
			})
		}
		if len(invoiceItems) > 0 {
			if err := tx.Create(&invoiceItems).Error; err != nil {
				return nil, err
			}
		}
		invoice.Items = invoiceItems
	} else {
		// 数量不一致时尽量回填，避免悬空。
		n := len(existingItems)
		if len(services) < n {
			n = len(services)
		}
		for i := 0; i < n; i++ {
			sid := services[i].ID
			if err := tx.Model(&model.InvoiceItem{}).Where("id = ?", existingItems[i].ID).Update("service_id", sid).Error; err != nil {
				return nil, err
			}
			existingItems[i].ServiceID = &sid
		}
		invoice.Items = existingItems
	}

	out = PayResult{Order: &order, Invoice: &invoice, Services: services}
	return &out, nil
}

// provisionPending 在事务提交后对 pending 服务逐个开通。
// 成功置为 active 并回填上游信息，失败置为 failed 并记录原因，钱不退。
func (s *OrderService) provisionPending(out *PayResult) {
	if out == nil {
		return
	}
	for i := range out.Services {
		if out.Services[i].Status != model.ServicePending {
			continue
		}
		updated, err := s.provisionOne(out.Services[i].ID)
		if err != nil {
			if updated != nil {
				out.Services[i] = *updated
			} else {
				var cur model.Service
				if e := s.db.First(&cur, out.Services[i].ID).Error; e == nil {
					out.Services[i] = cur
				}
			}
			continue
		}
		out.Services[i] = *updated
	}
}

// provisionOne 对单个待开通或开通失败的服务发起上游开通。
func (s *OrderService) provisionOne(serviceID uint) (*model.Service, error) {
	var svc model.Service
	if err := s.db.First(&svc, serviceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.Status != model.ServicePending && svc.Status != model.ServiceFailed {
		return &svc, ErrConflict("服务当前无需重试")
	}
	var product model.Product
	if err := s.db.First(&product, svc.ProductID).Error; err != nil {
		msg := truncateProvisionError("商品不存在，无法开通")
		_ = s.db.Model(&svc).Updates(map[string]any{"status": model.ServiceFailed, "provision_error": msg}).Error
		svc.Status = model.ServiceFailed
		svc.ProvisionError = msg
		log.Printf("上游开通失败 service=%d product=%d: 商品不存在", svc.ID, svc.ProductID)
		return &svc, ErrBadRequest("商品不存在，无法开通")
	}
	pluginID, _, err := resolvePluginForProduct(s.db, &product)
	if err != nil {
		msg := truncateProvisionError(err.Error())
		_ = s.db.Model(&svc).Updates(map[string]any{"status": model.ServiceFailed, "provision_error": msg}).Error
		svc.Status = model.ServiceFailed
		svc.ProvisionError = msg
		log.Printf("上游开通失败 service=%d product=%d: %v", svc.ID, product.ID, err)
		return &svc, err
	}
	var user model.User
	if err := s.db.First(&user, svc.UserID).Error; err != nil {
		return nil, err
	}
	orderNo := fmt.Sprintf("service-%d", svc.ID)
	opts := map[string]string{}
	if svc.OrderID != 0 {
		var order model.Order
		if err := s.db.Preload("Items").First(&order, svc.OrderID).Error; err == nil {
			orderNo = order.OrderNo
			for _, it := range order.Items {
				if it.ProductID == product.ID {
					opts = map[string]string(it.Options)
					break
				}
			}
		}
		if product.InterfaceID == 0 && len(opts) == 0 {
			opts = map[string]string{}
		}
	} else if product.InterfaceID != 0 {
		opts = defaultProvisionOptions(product.ProvisionConfig)
	}
	hostID, expiry, err := createUpstreamOrder(s.plugins, s.db, &product, svc.BillingCyc, orderNo, user.Email, opts)
	if err != nil {
		msg := truncateProvisionError(err.Error())
		_ = s.db.Model(&svc).Updates(map[string]any{"status": model.ServiceFailed, "provision_error": msg, "upstream_plugin_id": pluginID}).Error
		svc.Status = model.ServiceFailed
		svc.ProvisionError = msg
		svc.UpstreamPluginID = pluginID
		log.Printf("上游开通失败 service=%d product=%d order=%s: %v", svc.ID, product.ID, orderNo, err)
		return &svc, err
	}
	updates := map[string]any{"status": model.ServiceActive, "upstream_plugin_id": pluginID, "upstream_host_id": hostID, "provision_error": ""}
	if expiry != nil {
		updates["next_due_at"] = *expiry
		updates["expires_at"] = *expiry
		svc.NextDueAt = expiry
		svc.ExpiresAt = expiry
	}
	if err := s.db.Model(&svc).Updates(updates).Error; err != nil {
		return nil, err
	}
	svc.Status = model.ServiceActive
	svc.UpstreamPluginID = pluginID
	svc.UpstreamHostID = hostID
	svc.ProvisionError = ""
	return &svc, nil
}

// RetryProvision 重试单个服务的上游开通，仅限 failed/pending 且属于该用户。
func (s *OrderService) RetryProvision(userID, serviceID uint) (*model.Service, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.Status != model.ServicePending && svc.Status != model.ServiceFailed {
		return nil, ErrConflict("服务当前无需重试")
	}
	return s.provisionOne(svc.ID)
}

// AdminRetryProvision 以管理员身份重试任意用户的服务开通。
func (s *OrderService) AdminRetryProvision(serviceID uint) (*model.Service, error) {
	var svc model.Service
	if err := s.db.First(&svc, serviceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if svc.Status != model.ServicePending && svc.Status != model.ServiceFailed {
		return nil, ErrConflict("服务当前无需重试")
	}
	return s.provisionOne(svc.ID)
}

// provisionUpstream 调用上游插件下单开通，返回插件 ID、上游服务实例 ID 与到期时间。
//
// 实际下单逻辑在 createUpstreamOrder（与订单支付共用）；这里补齐用户邮箱
// 与事务内上下文。接口商品会把接口配置与用户选配一并透传给插件。
func (s *OrderService) provisionUpstream(tx *gorm.DB, userID uint, product *model.Product, cycle, orderNo string, options model.OptionMap) (string, string, *time.Time, error) {
	// 上游可能用客户邮箱建账号，取下单用户的邮箱。
	var user model.User
	if err := tx.First(&user, userID).Error; err != nil {
		return "", "", nil, err
	}

	pluginID, _, err := resolvePluginForProduct(s.db, product)
	if err != nil {
		return "", "", nil, err
	}

	hostID, expiry, err := createUpstreamOrder(s.plugins, s.db, product, cycle, orderNo, user.Email, map[string]string(options))
	if err != nil {
		log.Printf("上游开通失败 plugin=%s product=%d order=%s: %v", pluginID, product.ID, orderNo, err)
		return "", "", nil, err
	}
	return pluginID, hostID, expiry, nil
}

// Cancel 取消待支付订单，并同步取消其未付账单。
func (s *OrderService) Cancel(userID, orderID uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Order{}).
			Where("id = ? AND user_id = ? AND status = ?", orderID, userID, model.OrderPending).
			Update("status", model.OrderCancelled)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrConflict("订单不存在或状态不允许取消")
		}
		if err := tx.Model(&model.Invoice{}).
			Where("order_id = ? AND status = ?", orderID, model.InvoiceUnpaid).
			Updates(map[string]any{"status": model.InvoiceCancelled}).Error; err != nil {
			return err
		}
		return nil
	})
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
