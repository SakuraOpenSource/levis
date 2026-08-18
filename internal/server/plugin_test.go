package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// pluginEnv 是插件回调测试的公共环境。
type pluginEnv struct {
	rt      *runtime.Runtime
	handler http.Handler
	admin   []*http.Cookie
	// aliceID 是那个会被加钱的普通用户。
	aliceID uint
	// secret 是一把持有全部插件权限的凭证。
	secret string
}

// newPluginEnv 起一个已安装的服务，建好用户并签发一把插件凭证。
func newPluginEnv(t *testing.T, scopes ...string) pluginEnv {
	t.Helper()
	rt, handler, admin, _ := installedWithUsers(t, "alice")

	var alice model.User
	if err := rt.DB().First(&alice, "username = ?", "alice").Error; err != nil {
		t.Fatalf("读取 alice 失败: %v", err)
	}

	if len(scopes) == 0 {
		scopes = model.AllPluginScopes()
	}
	keys := service.NewPluginKeyService(rt.DB())
	secret, err := keys.IssueKey("fake", scopes)
	if err != nil {
		t.Fatalf("签发插件凭证失败: %v", err)
	}
	return pluginEnv{rt: rt, handler: handler, admin: admin, aliceID: alice.ID, secret: secret}
}

// balanceOfUser 读用户当前余额。
func balanceOfUser(t *testing.T, rt *runtime.Runtime, userID uint) int64 {
	t.Helper()
	var user model.User
	if err := rt.DB().Select("balance_cents").First(&user, userID).Error; err != nil {
		t.Fatalf("读取余额失败: %v", err)
	}
	return user.BalanceCents
}

// creditBody 造一个到账请求体。
func creditBody(externalID string, userID uint, amount int64) map[string]any {
	return map[string]any{
		"external_id":  externalID,
		"user_id":      userID,
		"amount_cents": amount,
		"gateway_ref":  "gw-" + externalID,
	}
}

// TestPluginCreditIsIdempotentOverHTTP 是这组接口最要紧的一条：同一 external_id
// 调两次，两次都 200，余额只加一次。
//
// 支付渠道的重复回调是常态；返回 409 会让它一直重试，返回 200 但重复加钱会
// 让用户凭空多拿一份。两者都不能接受。
func TestPluginCreditIsIdempotentOverHTTP(t *testing.T) {
	env := newPluginEnv(t)

	body := creditBody("ext-1", env.aliceID, 5000)
	first := doKey(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit", body, env.secret)
	if first.Code != http.StatusOK {
		t.Fatalf("首次到账应成功，实际 %d %s", first.Code, first.Body.String())
	}
	var firstRecord model.PluginPayment
	decodeJSON(t, first, &firstRecord)

	second := doKey(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit", body, env.secret)
	if second.Code != http.StatusOK {
		t.Fatalf("重复回调应返回 200，实际 %d %s", second.Code, second.Body.String())
	}
	var secondRecord model.PluginPayment
	decodeJSON(t, second, &secondRecord)

	if secondRecord.ID != firstRecord.ID {
		t.Errorf("重复回调应返回首次的记录 %d，实际 %d", firstRecord.ID, secondRecord.ID)
	}
	if secondRecord.AmountCents != firstRecord.AmountCents {
		t.Errorf("两次返回的金额应一致，%d != %d", secondRecord.AmountCents, firstRecord.AmountCents)
	}
	if got := balanceOfUser(t, env.rt, env.aliceID); got != 5000 {
		t.Errorf("重复回调后余额应为 5000，实际 %d", got)
	}

	var count int64
	err := env.rt.DB().Model(&model.Transaction{}).
		Where("user_id = ? AND ref_type = ?", env.aliceID, "plugin_payment").
		Count(&count).Error
	if err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	if count != 1 {
		t.Errorf("应只有 1 条到账流水，实际 %d", count)
	}
}

// TestPluginAPIIgnoresCookies 确认这组接口完全不认浏览器登录态。
//
// 这是本设计里最关键的一条边界：wallet/credit 能给任意用户加任意金额。若它
// 接受 Cookie，一个诱导管理员点开的页面就能靠管理员的登录态凭空造钱。
func TestPluginAPIIgnoresCookies(t *testing.T) {
	env := newPluginEnv(t)

	// 带着管理员登录态（doAs 会自动补 CSRF 双提交）但不带插件凭证。
	rec := doAs(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit",
		creditBody("cookie-1", env.aliceID, 9900), env.admin)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("带登录态但无插件凭证应返回 401，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := balanceOfUser(t, env.rt, env.aliceID); got != 0 {
		t.Errorf("余额不应变动，实际 %d", got)
	}

	// 用户自己的 API Key 也不行：那是另一套凭证体系，前缀就不一样。
	rec = doKey(t, env.handler, http.MethodGet, "/api/plugin/v1/users/"+itoa(env.aliceID), nil,
		"lvs_0123456789abcdef0123456789abcdef")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("用户 Key 调插件接口应返回 401，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestPluginAPIRequiresScope 确认未获授权的权限位一律 403。
func TestPluginAPIRequiresScope(t *testing.T) {
	// 只给读用户的权限，不给加钱的。
	env := newPluginEnv(t, model.PluginScopeUserRead)

	rec := doKey(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit",
		creditBody("scope-1", env.aliceID, 8800), env.secret)
	if rec.Code != http.StatusForbidden {
		t.Errorf("缺 wallet:credit 应返回 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := balanceOfUser(t, env.rt, env.aliceID); got != 0 {
		t.Errorf("余额不应变动，实际 %d", got)
	}

	// 有授权的那一项照常可用，证明 403 不是别的原因造成的。
	rec = doKey(t, env.handler, http.MethodGet, "/api/plugin/v1/users/"+itoa(env.aliceID), nil, env.secret)
	if rec.Code != http.StatusOK {
		t.Errorf("已授权的 user:read 应可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 没勾 order:read，读订单同样 403。
	rec = doKey(t, env.handler, http.MethodGet, "/api/plugin/v1/orders/1", nil, env.secret)
	if rec.Code != http.StatusForbidden {
		t.Errorf("缺 order:read 应返回 403，实际 %d", rec.Code)
	}
}

// TestPluginKeyRevokedImmediately 确认凭证吊销后立刻失效。
//
// 插件进程停止时会吊销它的凭证；若吊销不立即生效，一个被停用的插件仍能继续
// 加钱。
func TestPluginKeyRevokedImmediately(t *testing.T) {
	env := newPluginEnv(t)
	keys := service.NewPluginKeyService(env.rt.DB())

	rec := doKey(t, env.handler, http.MethodGet, "/api/plugin/v1/users/"+itoa(env.aliceID), nil, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("吊销前应可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	if err := keys.RevokeKeys("fake"); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	rec = doKey(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit",
		creditBody("revoked-1", env.aliceID, 100), env.secret)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("吊销后应返回 401，实际 %d %s", rec.Code, rec.Body.String())
	}
	if got := balanceOfUser(t, env.rt, env.aliceID); got != 0 {
		t.Errorf("吊销后不应还能加钱，余额 %d", got)
	}
}

// TestIssueKeyRevokesPrevious 确认重新签发会让旧凭证失效。
//
// 插件每次启动换一把 Key，旧的必须当场作废 —— 否则拷走过旧 Key 的人能一直用。
func TestIssueKeyRevokesPrevious(t *testing.T) {
	env := newPluginEnv(t)
	keys := service.NewPluginKeyService(env.rt.DB())

	fresh, err := keys.IssueKey("fake", model.AllPluginScopes())
	if err != nil {
		t.Fatalf("重新签发失败: %v", err)
	}

	path := "/api/plugin/v1/users/" + itoa(env.aliceID)
	if rec := doKey(t, env.handler, http.MethodGet, path, nil, env.secret); rec.Code != http.StatusUnauthorized {
		t.Errorf("旧凭证应失效，实际 %d", rec.Code)
	}
	if rec := doKey(t, env.handler, http.MethodGet, path, nil, fresh); rec.Code != http.StatusOK {
		t.Errorf("新凭证应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestPluginUserOmitsSensitiveFields 确认插件读到的用户资料不含余额。
//
// 插件要的是「给谁发信」，不需要知道对方有多少钱。
func TestPluginUserOmitsSensitiveFields(t *testing.T) {
	env := newPluginEnv(t)
	if _, err := service.NewWalletService(env.rt.DB()).Recharge(env.aliceID, 12345); err != nil {
		t.Fatalf("预置余额失败: %v", err)
	}

	rec := doKey(t, env.handler, http.MethodGet, "/api/plugin/v1/users/"+itoa(env.aliceID), nil, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取用户失败: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, field := range []string{"balance_cents", "password", "12345"} {
		if strings.Contains(body, field) {
			t.Errorf("响应不应包含 %q，实际 %s", field, body)
		}
	}
	var user service.PluginUser
	decodeJSON(t, rec, &user)
	if user.Email != "alice@example.com" {
		t.Errorf("应返回邮箱，实际 %q", user.Email)
	}
}

// TestPluginCreditRejectsBadInput 确认非法入参在接口层就被挡住。
func TestPluginCreditRejectsBadInput(t *testing.T) {
	env := newPluginEnv(t)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"缺 user_id", map[string]any{"external_id": "b1", "amount_cents": 100}, http.StatusBadRequest},
		{"缺单号", creditBody("", env.aliceID, 100), http.StatusBadRequest},
		{"零金额", creditBody("b2", env.aliceID, 0), http.StatusBadRequest},
		{"负金额", creditBody("b3", env.aliceID, -500), http.StatusBadRequest},
		{"金额过大", creditBody("b4", env.aliceID, 100_000_001), http.StatusBadRequest},
		{"用户不存在", creditBody("b5", 999999, 100), http.StatusNotFound},
	}
	for _, c := range cases {
		rec := doKey(t, env.handler, http.MethodPost, "/api/plugin/v1/wallet/credit", c.body, env.secret)
		if rec.Code != c.want {
			t.Errorf("%s 应返回 %d，实际 %d %s", c.name, c.want, rec.Code, rec.Body.String())
		}
	}
	if got := balanceOfUser(t, env.rt, env.aliceID); got != 0 {
		t.Errorf("非法请求被拒后余额应仍为 0，实际 %d", got)
	}

	// 一条都不该落库，否则会留下没有对应流水的「已到账」记录。
	var count int64
	if err := env.rt.DB().Model(&model.PluginPayment{}).Count(&count).Error; err != nil {
		t.Fatalf("统计支付记录失败: %v", err)
	}
	if count != 0 {
		t.Errorf("不应留下支付记录，实际 %d 条", count)
	}
}
