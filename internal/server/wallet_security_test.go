package server

import (
	"net/http"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// 遗留假充值入口不得在没有已核验支付的情况下给已登录用户增加余额。
func TestLegacyRechargeCannotCreditWallet(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")
	var before model.User
	if err := rt.DB().First(&before, "username = ?", "alice").Error; err != nil {
		t.Fatalf("读取测试用户失败: %v", err)
	}
	rec := doAs(t, handler, http.MethodPost, "/api/wallet/recharge", map[string]int64{
		"amount_cents": 100_000_000,
	}, users["alice"])
	if rec.Code != http.StatusForbidden {
		t.Errorf("无支付凭证的充值必须被拒绝，实际 %d", rec.Code)
	}
	var after model.User
	if err := rt.DB().First(&after, before.ID).Error; err != nil {
		t.Fatalf("重新读取测试用户失败: %v", err)
	}
	if after.BalanceCents != before.BalanceCents {
		t.Errorf("未支付不能增加余额: before=%d after=%d", before.BalanceCents, after.BalanceCents)
	}
	var count int64
	if err := rt.DB().Model(&model.Transaction{}).Where("user_id = ?", before.ID).Count(&count).Error; err != nil {
		t.Fatalf("查询流水失败: %v", err)
	}
	if count != 0 {
		t.Errorf("拒绝的充值不应写入流水，实际 %d 条", count)
	}
}
