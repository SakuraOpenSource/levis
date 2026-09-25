package service

import (
	"crypto/rand"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// CouponService 处理优惠码的管理与核销。
type CouponService struct {
	db *gorm.DB
}

// NewCouponService 构造 CouponService。
func NewCouponService(db *gorm.DB) *CouponService {
	return &CouponService{db: db}
}

// couponCodePattern 限定优惠码字符集：大写字母与数字，便于口播与印刷。
var couponCodePattern = regexp.MustCompile(`^[A-Z0-9]+$`)

// CouponInput 是优惠码的创建与更新入参。时间字段为指针：
// nil 表示不设该边界（立即开始 / 永不过期）。
type CouponInput struct {
	Code             string     `json:"code"`
	Name             string     `json:"name"`
	Type             string     `json:"type"`
	PercentOff       int        `json:"percent_off"`
	AmountCents      int64      `json:"amount_cents"`
	MaxDiscountCents int64      `json:"max_discount_cents"`
	MinOrderCents    int64      `json:"min_order_cents"`
	Status           string     `json:"status"`
	StartsAt         *time.Time `json:"starts_at"`
	ExpiresAt        *time.Time `json:"expires_at"`
	MaxUses          int        `json:"max_uses"`
	MaxUsesPerUser   int        `json:"max_uses_per_user"`
	NewUserOnly      bool       `json:"new_user_only"`
	ProductIDs       []uint     `json:"product_ids"`
}

// validateCoupon 校验并归一化入参（大写化 code、补默认值）。
func validateCoupon(in *CouponInput) error {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	if in.Code == "" {
		return ErrBadRequest("优惠码不能为空")
	}
	if len(in.Code) > 64 || !couponCodePattern.MatchString(in.Code) {
		return ErrBadRequest("优惠码只能包含大写字母与数字，长度不超过 64")
	}
	if len([]rune(strings.TrimSpace(in.Name))) > 128 {
		return ErrBadRequest("优惠码名称过长")
	}
	switch in.Type {
	case model.CouponTypePercent:
		if in.PercentOff < 1 || in.PercentOff > 99 {
			return ErrBadRequest("减免百分比必须在 1-99 之间")
		}
	case model.CouponTypeFixed:
		if in.AmountCents <= 0 {
			return ErrBadRequest("立减金额必须大于零")
		}
	default:
		return ErrBadRequest("无效的折扣类型")
	}
	if in.MaxDiscountCents < 0 || in.MinOrderCents < 0 || in.AmountCents < 0 {
		return ErrBadRequest("金额不能为负数")
	}
	if in.Status == "" {
		in.Status = model.CouponActive
	}
	if in.Status != model.CouponActive && in.Status != model.CouponDisabled {
		return ErrBadRequest("无效的优惠码状态")
	}
	if in.StartsAt != nil && in.ExpiresAt != nil && !in.ExpiresAt.After(*in.StartsAt) {
		return ErrBadRequest("结束时间必须晚于开始时间")
	}
	if in.MaxUses < 0 || in.MaxUsesPerUser < 0 {
		return ErrBadRequest("使用次数限制不能为负数")
	}
	if len(in.ProductIDs) > 500 {
		return ErrBadRequest("参与商品数量过多")
	}
	return nil
}

// couponFromInput 把入参落到模型（创建与更新共用）。
func couponFromInput(in CouponInput) model.Coupon {
	return model.Coupon{
		Code:             in.Code,
		Name:             strings.TrimSpace(in.Name),
		Type:             in.Type,
		PercentOff:       in.PercentOff,
		AmountCents:      in.AmountCents,
		MaxDiscountCents: in.MaxDiscountCents,
		MinOrderCents:    in.MinOrderCents,
		Status:           in.Status,
		StartsAt:         in.StartsAt,
		ExpiresAt:        in.ExpiresAt,
		MaxUses:          in.MaxUses,
		MaxUsesPerUser:   in.MaxUsesPerUser,
		NewUserOnly:      in.NewUserOnly,
		ProductIDs:       model.JSONUintArray(in.ProductIDs),
	}
}

// generateCouponCode 生成指定长度的大写字母数字优惠码，避免 0/O、1/I 混淆。
func generateCouponCode(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, length)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		buf[i] = alphabet[n.Int64()]
	}
	return string(buf), nil
}

// GenerateCodes 批量生成不重复的优惠码（管理员「自动生成」入口）。
// 只保证与库内现有 code 不冲突；返回的入参已通过校验字段之外的规则检查。
func (s *CouponService) GenerateCodes(in CouponInput, count int) ([]model.Coupon, error) {
	if count < 1 || count > 100 {
		return nil, ErrBadRequest("生成数量必须在 1-100 之间")
	}
	// 生成码不校验/大写化入参里的 code —— 它会被覆盖。
	in.Status = defaultString(in.Status, model.CouponActive)
	if err := validateCoupon(&in); err != nil {
		// 生成场景下 code 是自动填的，这里只关心其余字段合法。
		if !strings.Contains(err.Error(), "优惠码") {
			return nil, err
		}
	}
	var out []model.Coupon
	err := s.db.Transaction(func(tx *gorm.DB) error {
		out = nil
		for i := 0; i < count; i++ {
			var code string
			for attempt := 0; attempt < 10; attempt++ {
				candidate, err := generateCouponCode(12)
				if err != nil {
					return err
				}
				var count int64
				if err := tx.Model(&model.Coupon{}).Where("code = ?", candidate).Count(&count).Error; err != nil {
					return err
				}
				if count == 0 {
					code = candidate
					break
				}
			}
			if code == "" {
				return ErrConflict("优惠码空间不足，生成失败，请重试")
			}
			item := couponFromInput(in)
			item.Code = code
			if err := tx.Create(&item).Error; err != nil {
				return err
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// defaultString 空串回落默认值。
func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Create 创建单个优惠码（管理员手写 code）。
func (s *CouponService) Create(in CouponInput) (*model.Coupon, error) {
	if err := validateCoupon(&in); err != nil {
		return nil, err
	}
	item := couponFromInput(in)
	if err := s.db.Create(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrConflict("优惠码已存在")
		}
		return nil, err
	}
	return &item, nil
}

// Update 更新优惠码。code 一经创建不可改（核销记录靠 code 快照追溯）。
func (s *CouponService) Update(id uint, in CouponInput) (*model.Coupon, error) {
	var item model.Coupon
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("优惠码不存在")
		}
		return nil, err
	}
	if err := validateCoupon(&in); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"name":               strings.TrimSpace(in.Name),
		"type":               in.Type,
		"percent_off":        in.PercentOff,
		"amount_cents":       in.AmountCents,
		"max_discount_cents": in.MaxDiscountCents,
		"min_order_cents":    in.MinOrderCents,
		"status":             in.Status,
		"starts_at":          in.StartsAt,
		"expires_at":         in.ExpiresAt,
		"max_uses":           in.MaxUses,
		"max_uses_per_user":  in.MaxUsesPerUser,
		"new_user_only":      in.NewUserOnly,
		"product_ids":        model.JSONUintArray(in.ProductIDs),
	}
	if err := s.db.Model(&item).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// Delete 删除优惠码。核销记录保留（code 快照可追溯），仅此之后不可再使用。
func (s *CouponService) Delete(id uint) error {
	result := s.db.Delete(&model.Coupon{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("优惠码不存在")
	}
	return nil
}

// AdminList 分页返回优惠码，status 为空时不过滤。
func (s *CouponService) AdminList(status string, offset, limit int) ([]model.Coupon, int64, error) {
	query := s.db.Model(&model.Coupon{})
	if status == model.CouponActive || status == model.CouponDisabled {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Coupon
	if err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminGet 读取单个优惠码。
func (s *CouponService) AdminGet(id uint) (*model.Coupon, error) {
	var item model.Coupon
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("优惠码不存在")
		}
		return nil, err
	}
	return &item, nil
}

// CouponPreview 是优惠码试算结果（购物车页展示用）。
type CouponPreview struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	DiscountCents int64  `json:"discount_cents"`
	// SubtotalCents 是参与商品的小计（减免基数）。
	SubtotalCents int64 `json:"subtotal_cents"`
}

// couponEligibleSubtotal 在 items 里挑出优惠码参与的商品并求小计。
// items 的 ProductID 唯一（同一商品在订单里合并为一条明细）。
func couponEligibleSubtotal(coupon *model.Coupon, items []model.OrderItem) int64 {
	if len(coupon.ProductIDs) == 0 {
		var sum int64
		for _, item := range items {
			sum += item.PriceCents * int64(item.Quantity)
		}
		return sum
	}
	set := make(map[uint]struct{}, len(coupon.ProductIDs))
	for _, id := range coupon.ProductIDs {
		set[id] = struct{}{}
	}
	var sum int64
	for _, item := range items {
		if _, ok := set[item.ProductID]; ok {
			sum += item.PriceCents * int64(item.Quantity)
		}
	}
	return sum
}

// couponDiscountCents 按类型计算减免额：percent 先按比例再封顶，fixed 立减；
// 结果永远不超过参与商品小计（不允许减成负数）。
func couponDiscountCents(coupon *model.Coupon, subtotal int64) int64 {
	if subtotal <= 0 {
		return 0
	}
	var discount int64
	switch coupon.Type {
	case model.CouponTypePercent:
		discount = subtotal * int64(coupon.PercentOff) / 100
		if coupon.MaxDiscountCents > 0 && discount > coupon.MaxDiscountCents {
			discount = coupon.MaxDiscountCents
		}
	case model.CouponTypeFixed:
		discount = coupon.AmountCents
	}
	if discount > subtotal {
		discount = subtotal
	}
	if discount < 0 {
		discount = 0
	}
	return discount
}

// checkCouponUsable 校验优惠码对 userID 当前是否可用（不含商品匹配与门槛，
// 那两项依赖购物车内容，由 ApplyToItems 一并判定）。tx 为事务句柄。
func checkCouponUsable(tx *gorm.DB, coupon *model.Coupon, userID uint, now time.Time) error {
	if coupon.Status != model.CouponActive {
		return ErrBadRequest("优惠码已停用")
	}
	if coupon.StartsAt != nil && now.Before(*coupon.StartsAt) {
		return ErrBadRequest("优惠码未到使用时间")
	}
	if coupon.ExpiresAt != nil && now.After(*coupon.ExpiresAt) {
		return ErrBadRequest("优惠码已过期")
	}
	if coupon.MaxUses > 0 && coupon.UsedCount >= int64(coupon.MaxUses) {
		return ErrBadRequest("优惠码已被领完")
	}
	if coupon.MaxUsesPerUser > 0 {
		var used int64
		if err := tx.Model(&model.CouponRedemption{}).
			Where("coupon_id = ? AND user_id = ?", coupon.ID, userID).
			Count(&used).Error; err != nil {
			return err
		}
		if used >= int64(coupon.MaxUsesPerUser) {
			return ErrBadRequest("该优惠码你已使用过最大次数")
		}
	}
	if coupon.NewUserOnly {
		var paid int64
		if err := tx.Model(&model.Order{}).
			Where("user_id = ? AND status = ?", userID, model.OrderPaid).
			Count(&paid).Error; err != nil {
			return err
		}
		if paid > 0 {
			return ErrBadRequest("优惠码仅限新用户使用")
		}
	}
	return nil
}

// Preview 对给定明细试算优惠码（购物车页「验证优惠码」入口）。
// 只读不核销：真正占次数发生在订单创建事务里。
func (s *CouponService) Preview(userID uint, code string, items []model.OrderItem) (*CouponPreview, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, ErrBadRequest("请输入优惠码")
	}
	var coupon model.Coupon
	err := s.db.First(&coupon, "code = ?", code).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("优惠码不存在")
		}
		return nil, err
	}
	if err := checkCouponUsable(s.db, &coupon, userID, time.Now().UTC()); err != nil {
		return nil, err
	}
	subtotal := couponEligibleSubtotal(&coupon, items)
	if subtotal <= 0 {
		return nil, ErrBadRequest("购物车中没有该优惠码参与的商品")
	}
	if coupon.MinOrderCents > 0 && subtotal < coupon.MinOrderCents {
		return nil, ErrBadRequest("参与商品小计未达到优惠码使用门槛")
	}
	return &CouponPreview{
		Code:          coupon.Code,
		Name:          coupon.Name,
		Type:          coupon.Type,
		DiscountCents: couponDiscountCents(&coupon, subtotal),
		SubtotalCents: subtotal,
	}, nil
}

// applyCoupon 在订单创建事务内核销优惠码：校验 → 原子占额 → 写核销记录。
// 返回减免金额与归一化后的码；code 为空时是空操作（返回 0 与空串）。
// orderID 必须是本事务内已创建的订单 ID。
func applyCoupon(tx *gorm.DB, userID uint, code string, items []model.OrderItem, orderID uint, now time.Time) (int64, string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return 0, "", nil
	}
	var coupon model.Coupon
	err := tx.First(&coupon, "code = ?", code).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "", ErrBadRequest("优惠码不存在")
		}
		return 0, "", err
	}
	if err := checkCouponUsable(tx, &coupon, userID, now); err != nil {
		return 0, "", err
	}
	subtotal := couponEligibleSubtotal(&coupon, items)
	if subtotal <= 0 {
		return 0, "", ErrBadRequest("购物车中没有该优惠码参与的商品")
	}
	if coupon.MinOrderCents > 0 && subtotal < coupon.MinOrderCents {
		return 0, "", ErrBadRequest("参与商品小计未达到优惠码使用门槛")
	}
	discount := couponDiscountCents(&coupon, subtotal)

	// 原子占额：UPDATE ... WHERE used_count < max 并发下单只有一个能赢。
	if coupon.MaxUses > 0 {
		res := tx.Model(&model.Coupon{}).
			Where("id = ? AND used_count < ?", coupon.ID, coupon.MaxUses).
			Updates(map[string]any{"used_count": gorm.Expr("used_count + 1")})
		if res.Error != nil {
			return 0, "", res.Error
		}
		if res.RowsAffected == 0 {
			return 0, "", ErrBadRequest("优惠码已被领完")
		}
	} else if err := tx.Model(&model.Coupon{}).Where("id = ?", coupon.ID).
		Updates(map[string]any{"used_count": gorm.Expr("used_count + 1")}).Error; err != nil {
		return 0, "", err
	}

	redemption := model.CouponRedemption{
		CouponID:      coupon.ID,
		UserID:        userID,
		OrderID:       orderID,
		DiscountCents: discount,
		Code:          coupon.Code,
	}
	if err := tx.Create(&redemption).Error; err != nil {
		return 0, "", err
	}
	return discount, coupon.Code, nil
}

// releaseCoupon 在订单取消时回滚一次核销（次数 -1、删核销记录）。
// orderID 无核销记录时是空操作。必须在订单取消的同一事务里调用。
func releaseCoupon(tx *gorm.DB, orderID uint) error {
	var redemption model.CouponRedemption
	err := tx.First(&redemption, "order_id = ?", orderID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	// SQLite 没有 GREATEST，用 CASE 守住下限 0（并发取消不至于减成负数）。
	if err := tx.Model(&model.Coupon{}).Where("id = ?", redemption.CouponID).
		Updates(map[string]any{"used_count": gorm.Expr("CASE WHEN used_count > 0 THEN used_count - 1 ELSE 0 END")}).Error; err != nil {
		return err
	}
	return tx.Delete(&redemption).Error
}

// CartCouponView 是购物车叠加优惠码后的视图：小计、减免与应付。
type CartCouponView struct {
	Items         []model.CartItem `json:"items"`
	SubtotalCents int64            `json:"subtotal_cents"`
	TotalCents    int64            `json:"total_cents"`
	Coupon        *CouponPreview   `json:"coupon,omitempty"`
}

// CartCouponPreview 对当前购物车试算优惠码：按下单同款定价（含代理折扣），
// 只读不核销。购物车为空返回 400。
func (s *OrderService) CartCouponPreview(userID uint, code string) (*CartCouponView, error) {
	var cartItems []model.CartItem
	if err := s.db.Preload("Product").Where("user_id = ?", userID).
		Order("id ASC").Find(&cartItems).Error; err != nil {
		return nil, err
	}
	view := CartCouponView{Items: make([]model.CartItem, 0, len(cartItems))}
	lines := make([]OrderLine, 0, len(cartItems))
	for _, item := range cartItems {
		if item.Product == nil || item.Product.Status != model.ProductActive {
			continue
		}
		view.Items = append(view.Items, item)
		lines = append(lines, OrderLine{
			ProductID:  item.ProductID,
			Quantity:   item.Quantity,
			BillingCyc: item.BillingCyc,
		})
	}
	if len(lines) == 0 {
		return nil, ErrBadRequest("购物车为空")
	}
	orderItems, subtotal, err := buildOrderItems(s.db, userID, lines)
	if err != nil {
		return nil, err
	}
	view.SubtotalCents = subtotal
	view.TotalCents = subtotal
	preview, err := NewCouponService(s.db).Preview(userID, code, orderItems)
	if err != nil {
		return nil, err
	}
	view.Coupon = preview
	view.TotalCents = subtotal - preview.DiscountCents
	return &view, nil
}
