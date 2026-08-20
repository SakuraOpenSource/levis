package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// openOrder 是开放接口订单响应中测试关心的部分。
type openOrder struct {
	ID         uint   `json:"id"`
	OrderNo    string `json:"order_no"`
	Status     string `json:"status"`
	TotalCents int64  `json:"total_cents"`
	Items      []struct {
		ProductID   uint   `json:"product_id"`
		ProductName string `json:"product_name"`
		PriceCents  int64  `json:"price_cents"`
		Quantity    int    `json:"quantity"`
		BillingCyc  string `json:"billing_cycle"`
	} `json:"items"`
}

// openSetup 造好一个已实名的用户、一件商品与一把全权限 Key。
type openSetup struct {
	handler   http.Handler
	admin     []*http.Cookie
	cookies   []*http.Cookie
	secret    string
	productID uint
}

func setupOpenAPI(t *testing.T, balance int64) openSetup {
	t.Helper()
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)
	productID := seedProductVia(t, handler, admin, "vps", 1500)
	if balance > 0 {
		grantBalance(t, handler, alice, balance)
	}
	secret := createKey(t, handler, alice, "全权限", model.AllScopes())
	return openSetup{
		handler:   handler,
		admin:     admin,
		cookies:   alice,
		secret:    secret,
		productID: productID,
	}
}

// 开放接口只认 Key：带着有效登录 cookie 但不带 Key 一律 401。
//
// 这是「浏览器端」与「机器端」的分界线 —— 模糊掉就等于让开放接口继承了
// 浏览器的隐式凭证，而那条路径上没有 CSRF 防护。
func TestOpenAPIRejectsCookieCredentials(t *testing.T) {
	env := setupOpenAPI(t, 0)

	// doAs 会带上完整的登录态 cookie 与 CSRF 令牌，但没有 Authorization 头。
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/open/v1/account"},
		{http.MethodGet, "/api/open/v1/transactions"},
		{http.MethodGet, "/api/open/v1/products"},
		{http.MethodPost, "/api/open/v1/orders"},
		{http.MethodGet, "/api/open/v1/services"},
	} {
		rec := doAs(t, env.handler, tc.method, tc.path, map[string]any{}, env.cookies)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("带 cookie 不带 Key 调 %s 应返回 401，实际 %d %s",
				tc.path, rec.Code, rec.Body.String())
		}
	}
}

// 无效、伪造与格式不对的凭证都是 401。
func TestOpenAPIRejectsBadKeys(t *testing.T) {
	env := setupOpenAPI(t, 0)

	cases := []struct {
		name   string
		secret string
	}{
		{"完全没有", ""},
		{"随便一串", "not-a-key"},
		{"前缀对但内容错", "lvs_ffffffffffffffffffffffffffffffff"},
		{"改了一位", func() string {
			// 随机密钥末位有 1/16 概率本来就是 '0'，直接拼 '0' 会导致用例未实际改动而误判为通过。
			last := env.secret[len(env.secret)-1]
			repl := byte('0')
			if last == '0' {
				repl = '1'
			}
			return env.secret[:len(env.secret)-1] + string(repl)
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doKey(t, env.handler, http.MethodGet, "/api/open/v1/account", nil, tc.secret)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s 应返回 401，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}

	// Bearer 之外的方案不静默接受：否则调用方会误以为自己用对了。
	req := httptest.NewRequest(http.MethodGet, "/api/open/v1/account", nil)
	req.Header.Set("Authorization", "Basic "+env.secret)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Basic 方案应返回 401，实际 %d", rec.Code)
	}

	// X-Levis-Api-Key 是受支持的备选头。
	req = httptest.NewRequest(http.MethodGet, "/api/open/v1/account", nil)
	req.Header.Set("X-Levis-Api-Key", env.secret)
	rec = httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("X-Levis-Api-Key 应可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	// bearer 小写同样有效：RFC 7235 规定 auth-scheme 大小写不敏感。
	req = httptest.NewRequest(http.MethodGet, "/api/open/v1/account", nil)
	req.Header.Set("Authorization", "bearer "+env.secret)
	rec = httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("小写 bearer 应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 开放接口不要求 CSRF 令牌 —— Key 认证里没有浏览器隐式凭证，双提交无从获取。
func TestOpenAPIWorksWithoutCSRF(t *testing.T) {
	env := setupOpenAPI(t, 10000)
	// doKey 刻意不带 cookie 也不带 X-CSRF-Token。
	rec := doKey(t, env.handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{{"product_id": env.productID, "quantity": 1}},
	}, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("不带 CSRF 令牌的写请求应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 缺对应权限位的 Key 调接口一律 403。
func TestOpenAPIScopeEnforcement(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)
	productID := seedProductVia(t, handler, admin, "vps", 1500)
	grantBalance(t, handler, alice, 100000)

	balanceOnly := createKey(t, handler, alice, "只读余额", []string{model.ScopeBalanceRead})
	orderOnly := createKey(t, handler, alice, "只下单", []string{model.ScopeOrderWrite})
	serviceOnly := createKey(t, handler, alice, "只续费", []string{model.ScopeServiceWrite})

	// 先用全权限 Key 备一个订单与一个服务，供后面的越权尝试使用。
	full := createKey(t, handler, alice, "全权限", model.AllScopes())
	rec := doKey(t, handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{{"product_id": productID, "quantity": 1}},
	}, full)
	if rec.Code != http.StatusOK {
		t.Fatalf("备货下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order openOrder
	decodeJSON(t, rec, &order)
	orderID := itoa(order.ID)
	rec = doKey(t, handler, http.MethodPost, "/api/open/v1/orders/"+orderID+"/pay", nil, full)
	if rec.Code != http.StatusOK {
		t.Fatalf("备货支付失败: %d %s", rec.Code, rec.Body.String())
	}
	var paid struct {
		Services []struct {
			ID uint `json:"id"`
		} `json:"services"`
	}
	decodeJSON(t, rec, &paid)
	if len(paid.Services) != 1 {
		t.Fatalf("应开通 1 个服务，实际 %d 个", len(paid.Services))
	}
	serviceID := itoa(paid.Services[0].ID)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		secret string
		want   int
	}{
		{"只读余额查账号", http.MethodGet, "/api/open/v1/account", nil, balanceOnly, http.StatusOK},
		{"只读余额查流水", http.MethodGet, "/api/open/v1/transactions", nil, balanceOnly, http.StatusOK},
		{"只读余额查商品", http.MethodGet, "/api/open/v1/products", nil, balanceOnly, http.StatusForbidden},
		{"只读余额下单", http.MethodPost, "/api/open/v1/orders", map[string]any{
			"items": []map[string]any{{"product_id": productID, "quantity": 1}},
		}, balanceOnly, http.StatusForbidden},
		{"只读余额支付", http.MethodPost, "/api/open/v1/orders/" + orderID + "/pay", nil,
			balanceOnly, http.StatusForbidden},
		{"只读余额查服务", http.MethodGet, "/api/open/v1/services", nil, balanceOnly, http.StatusForbidden},

		// 写权限隐含其操作对象的读权限：能下单的 Key 自然要能查商品。
		{"只下单查商品", http.MethodGet, "/api/open/v1/products", nil, orderOnly, http.StatusOK},
		{"只下单查订单", http.MethodGet, "/api/open/v1/orders", nil, orderOnly, http.StatusOK},
		{"只下单查余额", http.MethodGet, "/api/open/v1/account", nil, orderOnly, http.StatusForbidden},
		{"只下单续费", http.MethodPost, "/api/open/v1/services/" + serviceID + "/renew", nil,
			orderOnly, http.StatusForbidden},

		{"只续费查服务", http.MethodGet, "/api/open/v1/services", nil, serviceOnly, http.StatusOK},
		{"只续费查余额", http.MethodGet, "/api/open/v1/account", nil, serviceOnly, http.StatusForbidden},
		{"只续费下单", http.MethodPost, "/api/open/v1/orders", map[string]any{
			"items": []map[string]any{{"product_id": productID, "quantity": 1}},
		}, serviceOnly, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doKey(t, handler, tc.method, tc.path, tc.body, tc.secret)
			if rec.Code != tc.want {
				t.Fatalf("%s 应返回 %d，实际 %d %s", tc.name, tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// 走完下单 → 支付 → 续费，确认钱包、账单与服务三处对得上。
func TestOpenAPIOrderPayRenewKeepsLedgerConsistent(t *testing.T) {
	const balance = 100000
	env := setupOpenAPI(t, balance)

	// 下单：价格必须由服务端从库里读，不采信调用方传的金额。
	rec := doKey(t, env.handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{
			{"product_id": env.productID, "quantity": 2, "price_cents": 1},
		},
	}, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order openOrder
	decodeJSON(t, rec, &order)
	if order.Status != model.OrderPending {
		t.Errorf("新订单状态应为 pending，实际 %s", order.Status)
	}
	if order.TotalCents != 3000 {
		t.Fatalf("总额应为 2×1500=3000，实际 %d —— 采信了调用方传的金额？", order.TotalCents)
	}
	if len(order.Items) != 1 || order.Items[0].PriceCents != 1500 {
		t.Fatalf("明细价格应快照为 1500，实际 %+v", order.Items)
	}

	// 支付。
	rec = doKey(t, env.handler, http.MethodPost,
		"/api/open/v1/orders/"+itoa(order.ID)+"/pay", nil, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("支付失败: %d %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Order   openOrder `json:"order"`
		Invoice struct {
			TotalCents int64  `json:"total_cents"`
			Status     string `json:"status"`
		} `json:"invoice"`
		Services []struct {
			ID         uint   `json:"id"`
			Status     string `json:"status"`
			PriceCents int64  `json:"price_cents"`
		} `json:"services"`
	}
	decodeJSON(t, rec, &result)
	if result.Order.Status != model.OrderPaid {
		t.Errorf("支付后订单状态应为 paid，实际 %s", result.Order.Status)
	}
	if result.Invoice.TotalCents != 3000 || result.Invoice.Status != model.InvoicePaid {
		t.Errorf("账单不符: %+v", result.Invoice)
	}
	// 数量为 2 应开通 2 个独立服务实例。
	if len(result.Services) != 2 {
		t.Fatalf("应开通 2 个服务，实际 %d 个", len(result.Services))
	}

	// 续费一个服务。
	serviceID := itoa(result.Services[0].ID)
	rec = doKey(t, env.handler, http.MethodPost,
		"/api/open/v1/services/"+serviceID+"/renew", nil, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("续费失败: %d %s", rec.Code, rec.Body.String())
	}

	// 余额：100000 - 3000（支付）- 1500（续费）= 95500。
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/account", nil, env.secret)
	var account struct {
		Username string `json:"username"`
		Wallet   struct {
			BalanceCents  int64 `json:"balance_cents"`
			ServiceActive int64 `json:"active_service_count"`
		} `json:"wallet"`
	}
	decodeJSON(t, rec, &account)
	const want = balance - 3000 - 1500
	if account.Wallet.BalanceCents != want {
		t.Errorf("余额应为 %d，实际 %d", want, account.Wallet.BalanceCents)
	}
	if account.Username != "alice" {
		t.Errorf("账号概要用户名应为 alice，实际 %s", account.Username)
	}
	if account.Wallet.ServiceActive != 2 {
		t.Errorf("在用服务应为 2 个，实际 %d", account.Wallet.ServiceActive)
	}

	// 流水：充值 1 笔 + 支付 1 笔 + 续费 1 笔 = 3 笔，且都经由 adjustBalance
	// 记账，所以每笔都带变动后余额。
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/transactions", nil, env.secret)
	var page struct {
		Items []struct {
			Type              string `json:"type"`
			AmountCents       int64  `json:"amount_cents"`
			BalanceAfterCents int64  `json:"balance_after_cents"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 3 {
		t.Fatalf("应有 3 笔流水，实际 %d 笔: %+v", page.Total, page.Items)
	}
	var sum int64
	for _, item := range page.Items {
		sum += item.AmountCents
		if item.BalanceAfterCents == 0 {
			t.Errorf("流水 %+v 缺少变动后余额，说明没走 adjustBalance", item)
		}
	}
	// 流水求和必须与余额一致 —— 这是「资金路径零复制」的可验证形式。
	if sum != want {
		t.Errorf("流水求和 %d 与余额 %d 不一致", sum, want)
	}
}

// 余额不足时整体回滚：不扣钱、不开服务、订单仍待支付。
func TestOpenAPIPayInsufficientBalanceRollsBack(t *testing.T) {
	// 商品 1500，只给 1000。
	env := setupOpenAPI(t, 1000)

	rec := doKey(t, env.handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{{"product_id": env.productID, "quantity": 1}},
	}, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order openOrder
	decodeJSON(t, rec, &order)

	rec = doKey(t, env.handler, http.MethodPost,
		"/api/open/v1/orders/"+itoa(order.ID)+"/pay", nil, env.secret)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("余额不足应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 余额分毫未动。
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/account", nil, env.secret)
	var account struct {
		Wallet struct {
			BalanceCents  int64 `json:"balance_cents"`
			ServiceActive int64 `json:"active_service_count"`
		} `json:"wallet"`
	}
	decodeJSON(t, rec, &account)
	if account.Wallet.BalanceCents != 1000 {
		t.Errorf("余额应保持 1000，实际 %d", account.Wallet.BalanceCents)
	}
	// 不留半个 service。
	if account.Wallet.ServiceActive != 0 {
		t.Errorf("失败的支付不该开通服务，实际 %d 个", account.Wallet.ServiceActive)
	}
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/services", nil, env.secret)
	var services struct {
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &services)
	if services.Total != 0 {
		t.Errorf("服务列表应为空，实际 %d 条", services.Total)
	}

	// 订单仍是待支付，充值后可以再试。
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/orders/"+itoa(order.ID), nil, env.secret)
	var reread openOrder
	decodeJSON(t, rec, &reread)
	if reread.Status != model.OrderPending {
		t.Errorf("失败支付后订单应仍为 pending，实际 %s", reread.Status)
	}

	// 流水里只有那一笔充值，没有失败的扣款记录。
	rec = doKey(t, env.handler, http.MethodGet, "/api/open/v1/transactions", nil, env.secret)
	var page struct {
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 1 {
		t.Errorf("应只有 1 笔充值流水，实际 %d 笔", page.Total)
	}
}

// 开放接口下单不碰购物车：机器调用不该结掉用户正在挑的东西。
func TestOpenAPIOrderDoesNotTouchCart(t *testing.T) {
	env := setupOpenAPI(t, 100000)

	// 用户在浏览器里往购物车放了 3 份。
	rec := doAs(t, env.handler, http.MethodPost, "/api/cart/items", map[string]any{
		"product_id": env.productID,
		"quantity":   3,
	}, env.cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("加入购物车失败: %d %s", rec.Code, rec.Body.String())
	}

	// API 直接下单 1 份。
	rec = doKey(t, env.handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{{"product_id": env.productID, "quantity": 1}},
	}, env.secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("API 下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order openOrder
	decodeJSON(t, rec, &order)
	if order.TotalCents != 1500 {
		t.Errorf("API 订单应只含 1 份，总额 1500，实际 %d", order.TotalCents)
	}

	// 购物车原封不动。
	rec = doAs(t, env.handler, http.MethodGet, "/api/cart/items", nil, env.cookies)
	var cart struct {
		Items []struct {
			Quantity int `json:"quantity"`
		} `json:"items"`
	}
	decodeJSON(t, rec, &cart)
	if len(cart.Items) != 1 || cart.Items[0].Quantity != 3 {
		t.Fatalf("API 下单动了用户的购物车: %+v", cart)
	}
}

// 直接下单的入参校验：空明细、非法数量、不存在的商品、超量明细。
func TestOpenAPICreateOrderValidation(t *testing.T) {
	env := setupOpenAPI(t, 100000)

	lines := make([]map[string]any, 21)
	for i := range lines {
		lines[i] = map[string]any{"product_id": env.productID, "quantity": 1}
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"没有明细", map[string]any{"items": []map[string]any{}}},
		{"数量为零", map[string]any{"items": []map[string]any{
			{"product_id": env.productID, "quantity": 0},
		}}},
		{"数量为负", map[string]any{"items": []map[string]any{
			{"product_id": env.productID, "quantity": -1},
		}}},
		{"商品不存在", map[string]any{"items": []map[string]any{
			{"product_id": 9999, "quantity": 1},
		}}},
		{"无效周期", map[string]any{"items": []map[string]any{
			{"product_id": env.productID, "quantity": 1, "billing_cycle": "hourly"},
		}}},
		{"明细超量", map[string]any{"items": lines}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doKey(t, env.handler, http.MethodPost, "/api/open/v1/orders", tc.body, env.secret)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s 应返回 400，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// 一把 Key 只能操作它自己账号下的数据。
func TestOpenAPIKeyScopedToOwnAccount(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice", "bob")
	passKYC(t, handler, admin, users["alice"], "张三", validID1)
	passKYC(t, handler, admin, users["bob"], "李四", validID2)
	productID := seedProductVia(t, handler, admin, "vps", 1500)
	grantBalance(t, handler, users["alice"], 100000)

	aliceKey := createKey(t, handler, users["alice"], "alice", model.AllScopes())
	bobKey := createKey(t, handler, users["bob"], "bob", model.AllScopes())

	rec := doKey(t, handler, http.MethodPost, "/api/open/v1/orders", map[string]any{
		"items": []map[string]any{{"product_id": productID, "quantity": 1}},
	}, aliceKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice 下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order openOrder
	decodeJSON(t, rec, &order)
	id := itoa(order.ID)

	// bob 的 Key 读不到也付不了 alice 的订单，且返回 404 而非 403。
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"读订单", http.MethodGet, "/api/open/v1/orders/" + id},
		{"支付订单", http.MethodPost, "/api/open/v1/orders/" + id + "/pay"},
	} {
		rec := doKey(t, handler, tc.method, tc.path, nil, bobKey)
		if rec.Code != http.StatusNotFound {
			t.Errorf("bob %s 应返回 404，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// bob 的列表里也不该出现 alice 的订单。
	rec = doKey(t, handler, http.MethodGet, "/api/open/v1/orders", nil, bobKey)
	var page struct {
		Total int64 `json:"total"`
	}
	decodeJSON(t, rec, &page)
	if page.Total != 0 {
		t.Errorf("bob 的订单列表应为空，实际 %d 条", page.Total)
	}
}

// 账号被停用后 Key 立刻失效 —— 与 RequireAuth 同一口径。
func TestOpenAPIRejectsDisabledUser(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	alice := users["alice"]
	passKYC(t, handler, admin, alice, "张三", validID1)
	secret := createKey(t, handler, alice, "全权限", model.AllScopes())

	if rec := doKey(t, handler, http.MethodGet, "/api/open/v1/account", nil,
		secret); rec.Code != http.StatusOK {
		t.Fatalf("停用前应可用，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 找到 alice 的 ID 再停用。
	rec := doAs(t, handler, http.MethodGet, "/api/admin/users", nil, admin)
	var page struct {
		Items []struct {
			ID       uint   `json:"id"`
			Username string `json:"username"`
		} `json:"items"`
	}
	decodeJSON(t, rec, &page)
	var aliceID uint
	for _, item := range page.Items {
		if item.Username == "alice" {
			aliceID = item.ID
		}
	}
	if aliceID == 0 {
		t.Fatal("没找到 alice")
	}
	rec = doAs(t, handler, http.MethodPatch, "/api/admin/users/"+itoa(aliceID),
		map[string]any{"status": model.UserDisabled}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("停用用户失败: %d %s", rec.Code, rec.Body.String())
	}

	rec = doKey(t, handler, http.MethodGet, "/api/open/v1/account", nil, secret)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("停用后应返回 403，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 未安装时开放接口与其它业务接口一样返回 503。
func TestOpenAPIBlockedBeforeInstall(t *testing.T) {
	_, handler := newTestServer(t)
	rec := doKey(t, handler, http.MethodGet, "/api/open/v1/account", nil, "lvs_"+strings.Repeat("0", 32))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未安装时应返回 503，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 开放接口里不存在的路径要返回 JSON 404，不能落到 SPA 上去。
func TestOpenAPIUnknownPathReturnsJSON(t *testing.T) {
	env := setupOpenAPI(t, 0)
	rec := doKey(t, env.handler, http.MethodGet, "/api/open/v1/nope", nil, env.secret)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径应返回 404，实际 %d", rec.Code)
	}
	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "NOT_FOUND" {
		t.Errorf("应返回 JSON 404，实际 %s", rec.Body.String())
	}
}
