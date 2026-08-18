package service

import (
	"sync"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// TestCreditExternalIsIdempotent 是本轮最要紧的一条断言：同一笔到账报两次，
// 余额只能加一次。
//
// 支付渠道一定会重复回调（超时重试、人工补发），少了这道幂等用户就会多拿钱。
func TestCreditExternalIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "ivan", 0)
	wallet := NewWalletService(db)

	first, err := wallet.CreditExternal("alipay", "ext-1", user.ID, 5000, "gw-1")
	if err != nil {
		t.Fatalf("首次到账失败: %v", err)
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Fatalf("首次到账后余额应为 5000，实际 %d", got)
	}

	// 重复回调：必须成功返回（返回错误会让渠道一直重试），且金额与首次一致。
	again, err := wallet.CreditExternal("alipay", "ext-1", user.ID, 5000, "gw-1")
	if err != nil {
		t.Fatalf("重复到账应当成功返回，实际报错: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("重复回调应返回首次的记录 %d，实际 %d", first.ID, again.ID)
	}
	if again.AmountCents != first.AmountCents {
		t.Errorf("重复回调返回的金额应与首次一致，%d != %d", again.AmountCents, first.AmountCents)
	}
	if got := balanceOf(t, db, user.ID); got != 5000 {
		t.Errorf("重复回调后余额仍应为 5000，实际 %d", got)
	}

	// 流水也只能有一条，否则对账时会看到两笔入账。
	var count int64
	err = db.Model(&model.Transaction{}).
		Where("user_id = ? AND ref_type = ?", user.ID, "plugin_payment").
		Count(&count).Error
	if err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	if count != 1 {
		t.Errorf("应只有 1 条插件到账流水，实际 %d", count)
	}

	// 不同 external_id 是另一笔付款，应当照常入账。
	if _, err := wallet.CreditExternal("alipay", "ext-2", user.ID, 1000, "gw-2"); err != nil {
		t.Fatalf("另一笔到账失败: %v", err)
	}
	if got := balanceOf(t, db, user.ID); got != 6000 {
		t.Errorf("第二笔到账后余额应为 6000，实际 %d", got)
	}

	// 同一 external_id 换个插件也是另一笔：两个插件的编号空间互不干扰。
	if _, err := wallet.CreditExternal("wechat", "ext-1", user.ID, 700, "gw-3"); err != nil {
		t.Fatalf("另一插件的同名单号应当独立入账，实际 %v", err)
	}
	if got := balanceOf(t, db, user.ID); got != 6700 {
		t.Errorf("第三笔到账后余额应为 6700，实际 %d", got)
	}
}

// TestCreditExternalConcurrentIsIdempotent 确认并发的重复回调也只加一次钱。
//
// 「先查再写」式的幂等在这个用例下会失败：两个请求都查到「不存在」，然后各
// 加一次。这里靠数据库唯一索引兜底，所以并发下依然只有一次入账。
func TestCreditExternalConcurrentIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "nina", 0)
	wallet := NewWalletService(db)

	const attempts = 6
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := wallet.CreditExternal("alipay", "race-1", user.ID, 2500, "gw-race")
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("并发回调不应报错: %v", err)
	}
	if got := balanceOf(t, db, user.ID); got != 2500 {
		t.Errorf("并发 %d 次回调后余额应为 2500，实际 %d", attempts, got)
	}
	var count int64
	if err := db.Model(&model.PluginPayment{}).Count(&count).Error; err != nil {
		t.Fatalf("统计支付记录失败: %v", err)
	}
	if count != 1 {
		t.Errorf("应只有 1 条支付记录，实际 %d", count)
	}
}

// TestCreditExternalRejectsBadInput 确认金额与单号的边界被挡住。
func TestCreditExternalRejectsBadInput(t *testing.T) {
	db := newTestDB(t)
	user := seedUser(t, db, "oscar", 0)
	wallet := NewWalletService(db)

	cases := []struct {
		name       string
		externalID string
		amount     int64
	}{
		{"零金额", "e1", 0},
		{"负金额", "e2", -100}, // 否则插件能靠「负数到账」把用户余额刷成负
		{"金额过大", "e3", 100_000_001},
		{"缺单号", "", 100}, // 没有单号就没有幂等键
	}
	for _, c := range cases {
		if _, err := wallet.CreditExternal("alipay", c.externalID, user.ID, c.amount, ""); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
	if got := balanceOf(t, db, user.ID); got != 0 {
		t.Errorf("非法到账被拒后余额应仍为 0，实际 %d", got)
	}
}

// TestCreditExternalUnknownUserWritesNothing 确认用户不存在时不留下支付记录。
//
// adjustBalance 在用户缺失时报错，整个事务必须回滚 —— 否则会留下一条
// 「已到账」但没有对应流水的记录，日后对账查不明白。
func TestCreditExternalUnknownUserWritesNothing(t *testing.T) {
	db := newTestDB(t)
	wallet := NewWalletService(db)

	if _, err := wallet.CreditExternal("alipay", "ghost", 99999, 1000, ""); err == nil {
		t.Fatal("用户不存在时应报错")
	}
	var count int64
	if err := db.Model(&model.PluginPayment{}).Count(&count).Error; err != nil {
		t.Fatalf("统计支付记录失败: %v", err)
	}
	if count != 0 {
		t.Errorf("事务应已回滚，不应留下支付记录，实际 %d 条", count)
	}
}
