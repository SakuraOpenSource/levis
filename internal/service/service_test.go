package service

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// newTestDB 建立一个内存 SQLite 库并完成迁移。
//
// 每个测试用独立的命名库（cache=shared 让同一连接池内可见），互不干扰。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatalf("获取连接池失败: %v", err)
	}
	// 内存库随最后一个连接关闭而消失，限制为单连接以保证生命周期稳定。
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })

	if err := db.AutoMigrate(model.AllModels()...); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	return db
}

// seedUser 建一个余额为 balance 的普通用户。
func seedUser(t *testing.T, db *gorm.DB, username string, balance int64) *model.User {
	t.Helper()
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("生成密码哈希失败: %v", err)
	}
	user := model.User{
		Username:     username,
		Email:        username + "@example.com",
		PasswordHash: hash,
		Role:         model.RoleUser,
		Status:       model.UserActive,
		BalanceCents: balance,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return &user
}

// seedProduct 建一个上架商品。
func seedProduct(t *testing.T, db *gorm.DB, name string, price int64) *model.Product {
	t.Helper()
	category := model.ProductCategory{Name: "香港", Slug: "hk-" + name}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("创建测试分组失败: %v", err)
	}
	product := model.Product{
		CategoryID: category.ID,
		Name:       name,
		PriceCents: price,
		BillingCyc: model.CycleMonthly,
		Stock:      -1,
		Status:     model.ProductActive,
	}
	if err := db.Create(&product).Error; err != nil {
		t.Fatalf("创建测试商品失败: %v", err)
	}
	return &product
}

// balanceOf 读取用户当前余额。
func balanceOf(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()
	var user model.User
	if err := db.First(&user, userID).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return user.BalanceCents
}

// TestRegisterCannotEscalateRole 确认注册接口无法用于自我提权。
//
// RegisterRequest 里没有 Role 字段，因此即使请求体带了 role=admin 也无处落地；
// 这个测试锁定该行为，防止日后有人图省事把请求直接绑到 model.User。
func TestRegisterCannotEscalateRole(t *testing.T) {
	db := newTestDB(t)
	users := NewUserService(db)

	user, err := users.Register(RegisterRequest{
		Username: "alice",
		Email:    "alice@example.com",
		Password: "password123",
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if user.Role != model.RoleUser {
		t.Fatalf("新注册用户角色应为 %q，实际为 %q", model.RoleUser, user.Role)
	}
	if user.BalanceCents != 0 {
		t.Fatalf("新注册用户余额应为 0，实际为 %d", user.BalanceCents)
	}
}

// TestRegisterRejectsDuplicate 确认重复用户名或邮箱被拒绝。
func TestRegisterRejectsDuplicate(t *testing.T) {
	db := newTestDB(t)
	users := NewUserService(db)

	req := RegisterRequest{Username: "bob", Email: "bob@example.com", Password: "password123"}
	if _, err := users.Register(req); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}

	_, err := users.Register(RegisterRequest{
		Username: "bob2", Email: "BOB@example.com", Password: "password123",
	})
	if err == nil {
		t.Fatal("邮箱重复（大小写不同）时应注册失败")
	}
	var bizErr *Error
	if !errors.As(err, &bizErr) || bizErr.Status != 409 {
		t.Fatalf("应返回 409 冲突错误，实际为 %v", err)
	}
}

// TestPayInsufficientBalanceRollsBack 是最关键的一条：余额不足时整个支付必须
// 完整回滚，不能留下已开通的服务、账单或流水。
func TestPayInsufficientBalanceRollsBack(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "carol", 500) // 余额 5 元
	product := seedProduct(t, db, "HK1", 1000)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet)

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 订单 10 元 > 余额 5 元，支付必须失败。
	if _, err := orders.Pay(user.ID, order.ID); err == nil {
		t.Fatal("余额不足时支付应失败")
	}

	if got := balanceOf(t, db, user.ID); got != 500 {
		t.Errorf("余额不应变动，期望 500，实际 %d", got)
	}
	for _, check := range []struct {
		name  string
		model any
	}{
		{"服务", &model.Service{}},
		{"账单", &model.Invoice{}},
		{"流水", &model.Transaction{}},
	} {
		var count int64
		if err := db.Model(check.model).Count(&count).Error; err != nil {
			t.Fatalf("统计%s失败: %v", check.name, err)
		}
		if count != 0 {
			t.Errorf("支付失败后不应留下%s记录，实际有 %d 条", check.name, count)
		}
	}

	var reloaded model.Order
	if err := db.First(&reloaded, order.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if reloaded.Status != model.OrderPending {
		t.Errorf("支付失败后订单应仍为 %q，实际为 %q", model.OrderPending, reloaded.Status)
	}
}

// TestPaySucceedsAndKeepsLedgerConsistent 确认支付成功后余额、流水与业务数据一致。
func TestPaySucceedsAndKeepsLedgerConsistent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "dave", 5000)
	product := seedProduct(t, db, "HK2", 1200)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet)

	// 买 2 份，总价 24 元。
	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 2}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if order.TotalCents != 2400 {
		t.Fatalf("订单总额应为 2400，实际 %d", order.TotalCents)
	}

	result, err := orders.Pay(user.ID, order.ID)
	if err != nil {
		t.Fatalf("支付失败: %v", err)
	}

	if got := balanceOf(t, db, user.ID); got != 2600 {
		t.Errorf("支付后余额应为 2600，实际 %d", got)
	}
	// 数量为 2 应开通 2 个独立服务实例。
	if len(result.Services) != 2 {
		t.Errorf("应开通 2 个服务，实际 %d 个", len(result.Services))
	}
	for _, service := range result.Services {
		if service.Status != model.ServiceActive {
			t.Errorf("服务状态应为 %q，实际 %q", model.ServiceActive, service.Status)
		}
		// 月付服务必须有下次续费时间。
		if service.NextDueAt == nil {
			t.Error("月付服务应有下次续费时间")
		}
	}
	if result.Invoice.Status != model.InvoicePaid {
		t.Errorf("账单状态应为 %q，实际 %q", model.InvoicePaid, result.Invoice.Status)
	}
	if result.Invoice.TotalCents != 2400 {
		t.Errorf("账单金额应为 2400，实际 %d", result.Invoice.TotalCents)
	}

	// 流水的 balance_after 必须与真实余额吻合，这是对账的基础。
	var tx model.Transaction
	if err := db.First(&tx, "user_id = ? AND type = ?", user.ID, model.TxPayment).Error; err != nil {
		t.Fatalf("读取支付流水失败: %v", err)
	}
	if tx.AmountCents != -2400 {
		t.Errorf("支付流水金额应为 -2400，实际 %d", tx.AmountCents)
	}
	if tx.BalanceAfterCents != 2600 {
		t.Errorf("流水记录的变动后余额应为 2600，实际 %d", tx.BalanceAfterCents)
	}

	// 购物车应已清空。
	view, err := cart.List(user.ID)
	if err != nil {
		t.Fatalf("读取购物车失败: %v", err)
	}
	if len(view.Items) != 0 {
		t.Errorf("下单后购物车应清空，实际还有 %d 条", len(view.Items))
	}
}

// TestPayTwiceRejected 确认已支付订单无法重复支付（防止重复扣款）。
func TestPayTwiceRejected(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "erin", 10000)
	product := seedProduct(t, db, "HK3", 1000)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet)

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(user.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	if _, err := orders.Pay(user.ID, order.ID); err != nil {
		t.Fatalf("首次支付失败: %v", err)
	}
	if _, err := orders.Pay(user.ID, order.ID); err == nil {
		t.Fatal("重复支付应被拒绝")
	}
	if got := balanceOf(t, db, user.ID); got != 9000 {
		t.Errorf("重复支付被拒后余额应仍为 9000，实际 %d", got)
	}
}

// TestPayOtherUsersOrderRejected 确认无法支付他人订单。
func TestPayOtherUsersOrderRejected(t *testing.T) {
	db := newTestDB(t)
	owner := seedUser(t, db, "frank", 10000)
	attacker := seedUser(t, db, "grace", 10000)
	product := seedProduct(t, db, "HK4", 1000)

	cart := NewCartService(db)
	wallet := NewWalletService(db)
	orders := NewOrderService(db, cart, wallet)

	if err := cart.Add(owner.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	order, err := orders.CreateFromCart(owner.ID)
	if err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	if _, err := orders.Pay(attacker.ID, order.ID); err == nil {
		t.Fatal("支付他人订单应被拒绝")
	}
	if got := balanceOf(t, db, attacker.ID); got != 10000 {
		t.Errorf("越权支付被拒后余额不应变动，实际 %d", got)
	}
}

// TestCartCrossUserAccessRejected 确认无法操作他人购物车条目。
func TestCartCrossUserAccessRejected(t *testing.T) {
	db := newTestDB(t)
	owner := seedUser(t, db, "heidi", 0)
	attacker := seedUser(t, db, "ivan", 0)
	product := seedProduct(t, db, "HK5", 1000)

	cart := NewCartService(db)
	if err := cart.Add(owner.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	var item model.CartItem
	if err := db.First(&item, "user_id = ?", owner.ID).Error; err != nil {
		t.Fatalf("读取购物车条目失败: %v", err)
	}

	if err := cart.UpdateQuantity(attacker.ID, item.ID, 99); err == nil {
		t.Error("修改他人购物车应被拒绝")
	}
	if err := cart.Remove(attacker.ID, item.ID); err == nil {
		t.Error("删除他人购物车条目应被拒绝")
	}

	var reloaded model.CartItem
	if err := db.First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("购物车条目应仍存在: %v", err)
	}
	if reloaded.Quantity != 1 {
		t.Errorf("数量不应被改动，期望 1，实际 %d", reloaded.Quantity)
	}
}

// TestWalletRechargeRecordsTransaction 确认充值写入流水。
func TestWalletRechargeRecordsTransaction(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "judy", 0)
	wallet := NewWalletService(db)

	record, err := wallet.Recharge(user.ID, 3000)
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if record.BalanceAfterCents != 3000 {
		t.Errorf("流水记录的变动后余额应为 3000，实际 %d", record.BalanceAfterCents)
	}
	if got := balanceOf(t, db, user.ID); got != 3000 {
		t.Errorf("充值后余额应为 3000，实际 %d", got)
	}

	// 非正数金额必须拒绝，否则可用负数充值反向刷余额。
	for _, amount := range []int64{0, -100} {
		if _, err := wallet.Recharge(user.ID, amount); err == nil {
			t.Errorf("充值金额 %d 应被拒绝", amount)
		}
	}
	if got := balanceOf(t, db, user.ID); got != 3000 {
		t.Errorf("非法充值被拒后余额应仍为 3000，实际 %d", got)
	}
}

// TestAdminBalanceAdjustmentLeavesAuditTrail 确认管理员改余额会留下流水，
// 且余额与流水记录保持一致。
func TestAdminBalanceAdjustmentLeavesAuditTrail(t *testing.T) {
	db := newTestDB(t)
	admin := seedUser(t, db, "root", 0)
	if err := db.Model(admin).Update("role", model.RoleAdmin).Error; err != nil {
		t.Fatalf("提升管理员失败: %v", err)
	}
	target := seedUser(t, db, "kevin", 1000)

	adminSvc := NewAdminService(db, NewWalletService(db))
	newBalance := int64(2500)
	updated, err := adminSvc.UpdateUser(admin.ID, target.ID, UpdateUserRequest{
		BalanceCents: &newBalance,
	})
	if err != nil {
		t.Fatalf("调整余额失败: %v", err)
	}
	if updated.BalanceCents != 2500 {
		t.Errorf("余额应为 2500，实际 %d", updated.BalanceCents)
	}

	var tx model.Transaction
	err = db.First(&tx, "user_id = ? AND type = ?", target.ID, model.TxAdjust).Error
	if err != nil {
		t.Fatalf("应留下一条 adjust 流水: %v", err)
	}
	if tx.AmountCents != 1500 {
		t.Errorf("流水金额应为差额 1500，实际 %d", tx.AmountCents)
	}
	if tx.BalanceAfterCents != 2500 {
		t.Errorf("流水记录的变动后余额应为 2500，实际 %d", tx.BalanceAfterCents)
	}
}

// TestAdminCannotDemoteSelf 确认管理员无法自我降权或自我禁用 —— 否则可能出现
// 系统中没有任何可用管理员的死局。
func TestAdminCannotDemoteSelf(t *testing.T) {
	db := newTestDB(t)
	admin := seedUser(t, db, "root2", 0)
	if err := db.Model(admin).Update("role", model.RoleAdmin).Error; err != nil {
		t.Fatalf("提升管理员失败: %v", err)
	}
	adminSvc := NewAdminService(db, NewWalletService(db))

	role := model.RoleUser
	if _, err := adminSvc.UpdateUser(admin.ID, admin.ID, UpdateUserRequest{Role: &role}); err == nil {
		t.Error("管理员不应能移除自己的管理员权限")
	}
	status := model.UserDisabled
	if _, err := adminSvc.UpdateUser(admin.ID, admin.ID, UpdateUserRequest{Status: &status}); err == nil {
		t.Error("管理员不应能禁用自己的账号")
	}
	if err := adminSvc.DeleteUser(admin.ID, admin.ID); err == nil {
		t.Error("管理员不应能删除自己的账号")
	}
}

// TestAdminCannotRemoveLastAdmin 确认系统始终保留至少一名管理员。
func TestAdminCannotRemoveLastAdmin(t *testing.T) {
	db := newTestDB(t)
	first := seedUser(t, db, "admin1", 0)
	second := seedUser(t, db, "admin2", 0)
	for _, user := range []*model.User{first, second} {
		if err := db.Model(user).Update("role", model.RoleAdmin).Error; err != nil {
			t.Fatalf("提升管理员失败: %v", err)
		}
	}
	adminSvc := NewAdminService(db, NewWalletService(db))

	// 两名管理员时，降权其中一个是允许的。
	role := model.RoleUser
	if _, err := adminSvc.UpdateUser(first.ID, second.ID, UpdateUserRequest{Role: &role}); err != nil {
		t.Fatalf("存在另一名管理员时应允许降权: %v", err)
	}
	// 此时 first 是唯一管理员，删除它必须被拒绝。
	if err := adminSvc.DeleteUser(second.ID, first.ID); err == nil {
		t.Error("删除最后一名管理员应被拒绝")
	}
}

// TestLoginRejectsDisabledUser 确认被禁用账号无法登录。
func TestLoginRejectsDisabledUser(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "mallory", 0)
	if err := db.Model(user).Update("status", model.UserDisabled).Error; err != nil {
		t.Fatalf("禁用用户失败: %v", err)
	}

	users := NewUserService(db)
	if _, err := users.Login("mallory", "password123"); err == nil {
		t.Fatal("被禁用账号应无法登录")
	}
}

// TestLoginWithWrongPassword 确认密码错误时登录失败。
func TestLoginWithWrongPassword(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "nancy", 0)
	users := NewUserService(db)

	if _, err := users.Login("nancy", "wrong-password"); err == nil {
		t.Fatal("密码错误时应登录失败")
	}
	// 用邮箱登录同样要能通过。
	if _, err := users.Login("nancy@example.com", "password123"); err != nil {
		t.Fatalf("用邮箱登录应成功: %v", err)
	}
}

// TestCategoryDepthLimited 确认分组层级被限制在两级。
func TestCategoryDepthLimited(t *testing.T) {
	db := newTestDB(t)
	adminSvc := NewAdminService(db, NewWalletService(db))

	root, err := adminSvc.CreateCategory(CategoryInput{Name: "香港"})
	if err != nil {
		t.Fatalf("创建大类失败: %v", err)
	}
	child, err := adminSvc.CreateCategory(CategoryInput{Name: "HK1", ParentID: &root.ID})
	if err != nil {
		t.Fatalf("创建小类失败: %v", err)
	}
	// 第三级必须被拒绝。
	if _, err := adminSvc.CreateCategory(CategoryInput{Name: "HK1-A", ParentID: &child.ID}); err == nil {
		t.Error("三级分组应被拒绝")
	}

	// 中文名称无法直接转 slug，应自动回退到可用值且不重复。
	if root.Slug == "" || child.Slug == "" {
		t.Error("分组 slug 不应为空")
	}
	if root.Slug == child.Slug {
		t.Error("不同分组的 slug 不应重复")
	}
}

// TestDeleteCategoryWithProductsRejected 确认有商品的分组不能直接删除。
func TestDeleteCategoryWithProductsRejected(t *testing.T) {
	db := newTestDB(t)
	adminSvc := NewAdminService(db, NewWalletService(db))
	product := seedProduct(t, db, "HK6", 1000)

	if err := adminSvc.DeleteCategory(product.CategoryID); err == nil {
		t.Error("分组下有商品时应拒绝删除")
	}
}

// TestCatalogTreeExcludesHiddenProducts 确认下架商品不出现在商店。
func TestCatalogTreeExcludesHiddenProducts(t *testing.T) {
	db := newTestDB(t)
	product := seedProduct(t, db, "HK7", 1000)
	if err := db.Model(product).Update("status", model.ProductHidden).Error; err != nil {
		t.Fatalf("下架商品失败: %v", err)
	}

	tree, err := NewCatalogService(db).Tree()
	if err != nil {
		t.Fatalf("读取分组树失败: %v", err)
	}
	for _, category := range tree {
		if len(category.Products) != 0 {
			t.Errorf("下架商品不应出现在商店，分组 %q 下有 %d 个商品",
				category.Name, len(category.Products))
		}
	}
}

// TestCartSkipsHiddenProduct 确认商品下架后不计入购物车总额。
func TestCartSkipsHiddenProduct(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "olivia", 0)
	product := seedProduct(t, db, "HK8", 1000)
	cart := NewCartService(db)

	if err := cart.Add(user.ID, AddRequest{ProductID: product.ID, Quantity: 1}); err != nil {
		t.Fatalf("加入购物车失败: %v", err)
	}
	if err := db.Model(product).Update("status", model.ProductHidden).Error; err != nil {
		t.Fatalf("下架商品失败: %v", err)
	}

	view, err := cart.List(user.ID)
	if err != nil {
		t.Fatalf("读取购物车失败: %v", err)
	}
	if view.TotalCents != 0 {
		t.Errorf("下架商品不应计入总额，实际 %d", view.TotalCents)
	}
	if len(view.Items) != 0 {
		t.Errorf("下架商品不应出现在购物车，实际 %d 条", len(view.Items))
	}
}

// TestPasswordChangeRequiresOldPassword 确认改密必须提供正确的原密码。
func TestPasswordChangeRequiresOldPassword(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "peggy", 0)
	users := NewUserService(db)

	if err := users.ChangePassword(user.ID, "wrong", "newpassword123"); err == nil {
		t.Error("原密码错误时应拒绝改密")
	}
	if err := users.ChangePassword(user.ID, "password123", "short"); err == nil {
		t.Error("新密码过短时应拒绝")
	}
	if err := users.ChangePassword(user.ID, "password123", "newpassword123"); err != nil {
		t.Fatalf("正常改密应成功: %v", err)
	}
	// 改密后旧密码必须失效。
	if _, err := users.Login("peggy", "password123"); err == nil {
		t.Error("改密后旧密码应失效")
	}
	if _, err := users.Login("peggy", "newpassword123"); err != nil {
		t.Errorf("改密后应能用新密码登录: %v", err)
	}
}

// TestProductSpecsRoundTrip 确认规格列表能原样存取。
//
// 规格以 JSON 文本存在单列里（driver.Valuer / sql.Scanner），顺序与内容都必须
// 保持不变 —— 卡片上的展示顺序由管理员填写顺序决定。
func TestProductSpecsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	adminSvc := NewAdminService(db, NewWalletService(db))
	category, err := adminSvc.CreateCategory(CategoryInput{Name: "美国"})
	if err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}

	specs := model.SpecList{
		{Label: "CPU", Value: "双 E5-2680v2"},
		{Label: "内存", Value: "16 GB DDR3"},
		{Label: "硬盘", Value: "500 GB SSD"},
	}
	created, err := adminSvc.CreateProduct(ProductInput{
		CategoryID: category.ID,
		Name:       "美国二区·高配",
		Specs:      specs,
		PriceCents: 2000,
	})
	if err != nil {
		t.Fatalf("创建商品失败: %v", err)
	}

	// 重新读库，确认走的是 Scan 而不是内存里的原对象。
	var loaded model.Product
	if err := db.First(&loaded, created.ID).Error; err != nil {
		t.Fatalf("读取商品失败: %v", err)
	}
	if len(loaded.Specs) != len(specs) {
		t.Fatalf("规格条数应为 %d，实际 %d", len(specs), len(loaded.Specs))
	}
	for i, spec := range specs {
		if loaded.Specs[i] != spec {
			t.Errorf("第 %d 条规格应为 %+v，实际 %+v", i, spec, loaded.Specs[i])
		}
	}

	// 更新为空列表后必须真的清空，不能残留旧值。
	if _, err := adminSvc.UpdateProduct(created.ID, ProductInput{
		CategoryID: category.ID,
		Name:       "美国二区·高配",
		PriceCents: 2000,
	}); err != nil {
		t.Fatalf("更新商品失败: %v", err)
	}
	if err := db.First(&loaded, created.ID).Error; err != nil {
		t.Fatalf("重新读取商品失败: %v", err)
	}
	if len(loaded.Specs) != 0 {
		t.Errorf("清空规格后应为空，实际 %+v", loaded.Specs)
	}
}

// TestProductSpecsNormalized 确认规格入库前被清理与限制。
func TestProductSpecsNormalized(t *testing.T) {
	db := newTestDB(t)
	adminSvc := NewAdminService(db, NewWalletService(db))
	category, err := adminSvc.CreateCategory(CategoryInput{Name: "香港"})
	if err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	newInput := func(specs model.SpecList) ProductInput {
		return ProductInput{
			CategoryID: category.ID,
			Name:       "HK1",
			Specs:      specs,
			PriceCents: 1000,
		}
	}

	// 前端表单常留空白行，应被丢弃而不是报错；两端空白也要去掉。
	product, err := adminSvc.CreateProduct(newInput(model.SpecList{
		{Label: "  CPU  ", Value: "  4 核  "},
		{Label: "", Value: ""},
	}))
	if err != nil {
		t.Fatalf("创建商品失败: %v", err)
	}
	if len(product.Specs) != 1 {
		t.Fatalf("空白行应被丢弃，实际 %d 条", len(product.Specs))
	}
	if product.Specs[0] != (model.Spec{Label: "CPU", Value: "4 核"}) {
		t.Errorf("规格应被去空白，实际 %+v", product.Specs[0])
	}

	// 只填一半属于输入错误，必须明确拒绝。
	if _, err := adminSvc.CreateProduct(newInput(model.SpecList{
		{Label: "CPU", Value: ""},
	})); err == nil {
		t.Error("规格只填名称时应拒绝")
	}
	if _, err := adminSvc.CreateProduct(newInput(model.SpecList{
		{Label: "", Value: "4 核"},
	})); err == nil {
		t.Error("规格只填内容时应拒绝")
	}

	// 过长内容与超量条数都会撑爆卡片布局。
	if _, err := adminSvc.CreateProduct(newInput(model.SpecList{
		{Label: strings.Repeat("长", 33), Value: "4 核"},
	})); err == nil {
		t.Error("规格名称过长时应拒绝")
	}
	tooMany := make(model.SpecList, 0, maxSpecs+1)
	for i := 0; i <= maxSpecs; i++ {
		tooMany = append(tooMany, model.Spec{Label: "K" + strconv.Itoa(i), Value: "V"})
	}
	if _, err := adminSvc.CreateProduct(newInput(tooMany)); err == nil {
		t.Errorf("规格超过 %d 条时应拒绝", maxSpecs)
	}
}
