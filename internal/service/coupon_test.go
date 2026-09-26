package service

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// seedCouponFixture 是优惠码测试的公共数据集：一个用户 + 一个可购商品。
type couponFixture struct {
	db       *gorm.DB
	orders   *OrderService
	coupons  *CouponService
	cart     *CartService
	user     *model.User
	product  *model.Product
	product2 *model.Product
}

// newCouponFixture 建库、用户（余额充足）、两个上架商品并装进购物车。
func newCouponFixture(t *testing.T) couponFixture {
	t.Helper()
	db := newTestDB(t)
	wallet := NewWalletService(db)
	cart := NewCartService(db)
	orders := NewOrderService(db, cart, wallet, nil)
	coupons := NewCouponService(db)
	user := seedUser(t, db, "coupon-user", 1_000_00)
	cat := model.ProductCategory{Name: "测试分组", Slug: "coupon-cat"}
	if err := db.Create(&cat).Error; err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	p1 := model.Product{CategoryID: cat.ID, Name: "云服务器", PriceCents: 100_00, BillingCyc: model.CycleMonthly, Stock: -1, Status: model.ProductActive}
	p2 := model.Product{CategoryID: cat.ID, Name: "虚拟主机", PriceCents: 50_00, BillingCyc: model.CycleMonthly, Stock: -1, Status: model.ProductActive}
	if err := db.Create(&p1).Error; err != nil {
		t.Fatalf("创建商品失败: %v", err)
	}
	if err := db.Create(&p2).Error; err != nil {
		t.Fatalf("创建商品2失败: %v", err)
	}
	if err := cart.Add(user.ID, AddRequest{ProductID: p1.ID, Quantity: 2}); err != nil {
		t.Fatalf("装购物车失败: %v", err)
	}
	if err := cart.Add(user.ID, AddRequest{ProductID: p2.ID, Quantity: 1}); err != nil {
		t.Fatalf("装购物车失败: %v", err)
	}
	return couponFixture{db: db, orders: orders, coupons: coupons, cart: cart, user: user, product: &p1, product2: &p2}
}

// TestCouponPercentDiscount 验证百分比减免：购物车 250 元打 8 折 → 减 50 元。
func TestCouponPercentDiscount(t *testing.T) {
	fx := newCouponFixture(t)
	coupon, err := fx.coupons.Create(CouponInput{
		Code: "SAVE20", Type: model.CouponTypePercent, PercentOff: 20,
		MaxUsesPerUser: 1,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	// 试算：不占次数。
	preview, err := fx.orders.CartCouponPreview(fx.user.ID, "save20")
	if err != nil {
		t.Fatalf("试算失败: %v", err)
	}
	if preview.SubtotalCents != 250_00 {
		t.Fatalf("小计错误: %d", preview.SubtotalCents)
	}
	if preview.Coupon.DiscountCents != 50_00 {
		t.Fatalf("减免错误: %d", preview.Coupon.DiscountCents)
	}
	// 试算不占次数：重新读库验证 used_count 仍为 0。
	var fresh model.Coupon
	if err := fx.db.First(&fresh, coupon.ID).Error; err != nil {
		t.Fatalf("读取优惠码失败: %v", err)
	}
	if fresh.UsedCount != 0 {
		t.Fatalf("试算不应占用次数: %d", fresh.UsedCount)
	}
	// 下单核销。
	order, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "save20", false)
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if order.TotalCents != 200_00 {
		t.Fatalf("应付总额错误: %d", order.TotalCents)
	}
	if order.CouponCode != "SAVE20" || order.CouponDiscountCents != 50_00 {
		t.Fatalf("优惠快照错误: %s %d", order.CouponCode, order.CouponDiscountCents)
	}
}

// TestCouponFixedDiscountWithCap 固定减免 + 参与商品限定 + 门槛。
func TestCouponFixedWithProductScopeAndMinOrder(t *testing.T) {
	fx := newCouponFixture(t)
	// 只参与云服务器（小计 200），门槛 150，立减 30。
	_, err := fx.coupons.Create(CouponInput{
		Code: "FIX30", Type: model.CouponTypeFixed, AmountCents: 30_00,
		MinOrderCents: 150_00, ProductIDs: []uint{fx.product.ID},
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	order, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "FIX30", false)
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	// 250 - 30 = 220。
	if order.TotalCents != 220_00 {
		t.Fatalf("应付总额错误: %d", order.TotalCents)
	}
}

// TestCouponMinOrderNotMet 门槛未达 → 下单失败且事务回滚（订单不落库）。
func TestCouponMinOrderNotMet(t *testing.T) {
	fx := newCouponFixture(t)
	_, err := fx.coupons.Create(CouponInput{
		Code: "BIG", Type: model.CouponTypeFixed, AmountCents: 10_00,
		MinOrderCents: 999_00,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	_, err = fx.orders.CreateFromCartCoupon(fx.user.ID, "BIG", false)
	if err == nil {
		t.Fatal("门槛未达应失败")
	}
	var count int64
	fx.db.Model(&model.Order{}).Count(&count)
	if count != 0 {
		t.Fatalf("失败下单不应落库: %d", count)
	}
	// 购物车应保留（事务回滚）。
	var cartCount int64
	fx.db.Model(&model.CartItem{}).Where("user_id = ?", fx.user.ID).Count(&cartCount)
	if cartCount != 2 {
		t.Fatalf("购物车应保留: %d", cartCount)
	}
}

// TestCouponNewUserOnly 老用户（已有支付订单）不可用新用户码。
func TestCouponNewUserOnly(t *testing.T) {
	fx := newCouponFixture(t)
	// 用户已有一笔支付订单。
	old := model.Order{OrderNo: "OLDDDDDD1", UserID: fx.user.ID, Status: model.OrderPaid, TotalCents: 100}
	if err := fx.db.Create(&old).Error; err != nil {
		t.Fatalf("创建历史订单失败: %v", err)
	}
	_, err := fx.coupons.Create(CouponInput{
		Code: "NEWBIE", Type: model.CouponTypePercent, PercentOff: 10, NewUserOnly: true,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	if _, err := fx.orders.CartCouponPreview(fx.user.ID, "NEWBIE"); err == nil {
		t.Fatal("老用户应被拒绝")
	}
}

// TestCouponPerUserLimit 每用户限一次：第二次下单被拒。
func TestCouponPerUserLimit(t *testing.T) {
	fx := newCouponFixture(t)
	_, err := fx.coupons.Create(CouponInput{
		Code: "ONCE", Type: model.CouponTypeFixed, AmountCents: 5_00, MaxUsesPerUser: 1,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	if _, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "ONCE", false); err != nil {
		t.Fatalf("第一次下单失败: %v", err)
	}
	// 重新装购物车再下单。
	if err := fx.cart.Add(fx.user.ID, AddRequest{ProductID: fx.product2.ID, Quantity: 1}); err != nil {
		t.Fatalf("装购物车失败: %v", err)
	}
	if _, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "ONCE", false); err == nil {
		t.Fatal("第二次使用应被拒绝")
	}
}

// TestCouponCancelReleases 订单取消回滚核销次数，码可再次使用。
func TestCouponCancelReleases(t *testing.T) {
	fx := newCouponFixture(t)
	coupon, err := fx.coupons.Create(CouponInput{
		Code: "BACK", Type: model.CouponTypeFixed, AmountCents: 5_00, MaxUsesPerUser: 1,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	order, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "BACK", false)
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if err := fx.orders.Cancel(fx.user.ID, order.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	var after model.Coupon
	if err := fx.db.First(&after, coupon.ID).Error; err != nil {
		t.Fatalf("读取优惠码失败: %v", err)
	}
	if after.UsedCount != 0 {
		t.Fatalf("取消后应回滚次数: %d", after.UsedCount)
	}
	var redemptions int64
	fx.db.Model(&model.CouponRedemption{}).Where("coupon_id = ?", coupon.ID).Count(&redemptions)
	if redemptions != 0 {
		t.Fatalf("核销记录应删除: %d", redemptions)
	}
	// 可再次使用。
	if err := fx.cart.Add(fx.user.ID, AddRequest{ProductID: fx.product2.ID, Quantity: 1}); err != nil {
		t.Fatalf("装购物车失败: %v", err)
	}
	if _, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "BACK", false); err != nil {
		t.Fatalf("取消后应可再次使用: %v", err)
	}
}

// TestCouponTimeWindow 时间窗口：未开始与已过期都要拒绝。
func TestCouponTimeWindow(t *testing.T) {
	fx := newCouponFixture(t)
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-2 * time.Hour)
	_, err := fx.coupons.Create(CouponInput{
		Code: "FUTURE", Type: model.CouponTypeFixed, AmountCents: 5_00, StartsAt: &future,
	})
	if err != nil {
		t.Fatalf("创建未开始码失败: %v", err)
	}
	if _, err := fx.orders.CartCouponPreview(fx.user.ID, "FUTURE"); err == nil {
		t.Fatal("未开始的码应被拒绝")
	}
	_, err = fx.coupons.Create(CouponInput{
		Code: "PASTED", Type: model.CouponTypeFixed, AmountCents: 5_00, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("创建已过期码失败: %v", err)
	}
	if _, err := fx.orders.CartCouponPreview(fx.user.ID, "PASTED"); err == nil {
		t.Fatal("已过期的码应被拒绝")
	}
}

// TestCouponMaxUsesGlobal 全局次数用尽后被拒。
func TestCouponMaxUsesGlobal(t *testing.T) {
	fx := newCouponFixture(t)
	coupon, err := fx.coupons.Create(CouponInput{
		Code: "LIMIT", Type: model.CouponTypeFixed, AmountCents: 5_00, MaxUses: 1,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	if _, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "LIMIT", false); err != nil {
		t.Fatalf("第一次下单失败: %v", err)
	}
	// 另一个用户装购物车使用同一码。
	other := seedUser(t, fx.db, "coupon-other", 0)
	if err := fx.cart.Add(other.ID, AddRequest{ProductID: fx.product2.ID, Quantity: 1}); err != nil {
		t.Fatalf("装购物车失败: %v", err)
	}
	if _, err := fx.orders.CreateFromCartCoupon(other.ID, "LIMIT", false); err == nil {
		t.Fatal("全局次数用尽应被拒绝")
	}
	_ = coupon
}

// TestCouponPercentCap 百分比封顶：50% 但封顶 60 元，250 元减 125 → 封到 60。
func TestCouponPercentCap(t *testing.T) {
	fx := newCouponFixture(t)
	_, err := fx.coupons.Create(CouponInput{
		Code: "CAP60", Type: model.CouponTypePercent, PercentOff: 50, MaxDiscountCents: 60_00,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	order, err := fx.orders.CreateFromCartCoupon(fx.user.ID, "CAP60", false)
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if order.CouponDiscountCents != 60_00 || order.TotalCents != 190_00 {
		t.Fatalf("封顶减免错误: %d %d", order.CouponDiscountCents, order.TotalCents)
	}
}

// TestCouponGenerateCodes 批量生成：码不重复且符合字符集。
func TestCouponGenerateCodes(t *testing.T) {
	fx := newCouponFixture(t)
	items, err := fx.coupons.GenerateCodes(CouponInput{
		Type: model.CouponTypePercent, PercentOff: 10, Name: "双十码",
	}, 5)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("生成数量错误: %d", len(items))
	}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.Code] {
			t.Fatalf("码重复: %s", item.Code)
		}
		seen[item.Code] = true
		if item.Name != "双十码" || item.Type != model.CouponTypePercent || item.PercentOff != 10 {
			t.Fatalf("生成属性错误: %+v", item)
		}
	}
}

// TestCouponDuplicateCode 重复码创建被拒。
func TestCouponDuplicateCode(t *testing.T) {
	fx := newCouponFixture(t)
	_, err := fx.coupons.Create(CouponInput{Code: "DUP", Type: model.CouponTypeFixed, AmountCents: 5_00})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := fx.coupons.Create(CouponInput{Code: "DUP", Type: model.CouponTypeFixed, AmountCents: 6_00}); err == nil {
		t.Fatal("重复码应被拒绝")
	}
}

// TestCouponInvalidInput 各类非法入参被拒。
func TestCouponInvalidInput(t *testing.T) {
	fx := newCouponFixture(t)
	cases := []CouponInput{
		{Code: "", Type: model.CouponTypeFixed, AmountCents: 5_00},                 // 空码
		{Code: "有中文", Type: model.CouponTypeFixed, AmountCents: 5_00},              // 非法字符
		{Code: "P0", Type: model.CouponTypePercent, PercentOff: 0},                 // 百分比越界
		{Code: "P100", Type: model.CouponTypePercent, PercentOff: 100},             // 百分比越界
		{Code: "F0", Type: model.CouponTypeFixed, AmountCents: 0},                  // 立减为零
		{Code: "T?", Type: model.CouponTypeFixed, AmountCents: 5_00, Status: "xx"}, // 非法状态
	}
	for i, in := range cases {
		if _, err := fx.coupons.Create(in); err == nil {
			t.Fatalf("case %d 应被拒绝: %+v", i, in)
		}
	}
	// 结束早于开始。
	start := time.Now().UTC().Add(time.Hour)
	end := time.Now().UTC()
	if _, err := fx.coupons.Create(CouponInput{
		Code: "TWIN", Type: model.CouponTypeFixed, AmountCents: 5_00, StartsAt: &start, ExpiresAt: &end,
	}); err == nil {
		t.Fatal("时间窗口倒挂应被拒绝")
	}
}

// TestBuyNowCouponPreview 直购试算：单明细（含数量）按同款定价算减免，不占次数。
func TestBuyNowCouponPreview(t *testing.T) {
	fx := newCouponFixture(t)
	if _, err := fx.coupons.Create(CouponInput{
		Code: "DIRECT10", Type: model.CouponTypePercent, PercentOff: 10,
	}); err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	// 单条直购明细：云服务器 100 元 × 3 = 300 元，9 折 → 减 30 元。
	view, err := fx.orders.BuyNowCouponPreview(fx.user.ID, OrderLine{
		ProductID: fx.product.ID, Quantity: 3, BillingCyc: model.CycleMonthly,
	}, "direct10")
	if err != nil {
		t.Fatalf("试算失败: %v", err)
	}
	if view.SubtotalCents != 300_00 || view.Coupon.DiscountCents != 30_00 || view.TotalCents != 270_00 {
		t.Fatalf("试算金额错误: subtotal=%d discount=%d total=%d",
			view.SubtotalCents, view.Coupon.DiscountCents, view.TotalCents)
	}
	// 试算不占次数。
	var fresh model.Coupon
	if err := fx.db.First(&fresh, "code = ?", "DIRECT10").Error; err != nil {
		t.Fatalf("读取优惠码失败: %v", err)
	}
	if fresh.UsedCount != 0 {
		t.Fatalf("试算不应占用次数: %d", fresh.UsedCount)
	}
	// 无效码报错。
	if _, err := fx.orders.BuyNowCouponPreview(fx.user.ID, OrderLine{
		ProductID: fx.product.ID, Quantity: 1, BillingCyc: model.CycleMonthly,
	}, "NOPE"); err == nil {
		t.Fatal("无效码应报错")
	}
}

// TestBuyNowWithCoupon 直购下单核销优惠码：订单快照记录减免，取消回滚次数。
func TestBuyNowWithCoupon(t *testing.T) {
	fx := newCouponFixture(t)
	coupon, err := fx.coupons.Create(CouponInput{
		Code: "BUYNOW", Type: model.CouponTypeFixed, AmountCents: 15_00, MaxUsesPerUser: 1,
	})
	if err != nil {
		t.Fatalf("创建优惠码失败: %v", err)
	}
	order, err := fx.orders.CreateDirectCoupon(fx.user.ID, []OrderLine{{
		ProductID: fx.product.ID, Quantity: 2, BillingCyc: model.CycleMonthly,
	}}, "buynow", false)
	if err != nil {
		t.Fatalf("直购下单失败: %v", err)
	}
	if order.TotalCents != 185_00 || order.CouponCode != "BUYNOW" || order.CouponDiscountCents != 15_00 {
		t.Fatalf("订单快照错误: total=%d code=%s discount=%d",
			order.TotalCents, order.CouponCode, order.CouponDiscountCents)
	}
	// 已用过 → 二次直购被拒。
	if _, err := fx.orders.CreateDirectCoupon(fx.user.ID, []OrderLine{{
		ProductID: fx.product.ID, Quantity: 1, BillingCyc: model.CycleMonthly,
	}}, "BUYNOW", false); err == nil {
		t.Fatal("超限应被拒绝")
	}
	// 取消回滚。
	if err := fx.orders.Cancel(fx.user.ID, order.ID); err != nil {
		t.Fatalf("取消订单失败: %v", err)
	}
	var fresh model.Coupon
	if err := fx.db.First(&fresh, coupon.ID).Error; err != nil {
		t.Fatalf("读取优惠码失败: %v", err)
	}
	if fresh.UsedCount != 0 {
		t.Fatalf("取消后应回滚次数: %d", fresh.UsedCount)
	}
}
