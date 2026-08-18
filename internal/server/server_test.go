package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// TestMain 把 bcrypt 强度降到下限后再跑整包测试。
//
// 本包的用例几乎每个都要装站、注册、登录，每一步都过一次 bcrypt。按生产
// 强度（cost 12）算，一次哈希约几十毫秒，而 -race 会把它放大约一个数量级：
// 单次装站要 6 秒以上，整包累积起来直接撞上 go test 默认的 10 分钟超时。
//
// 强度只影响哈希的计算成本，不影响正确性 —— 校验走的是哈希串里自带的
// cost，生成与比对仍是同一套代码路径。
func TestMain(m *testing.M) {
	restore := auth.SetPasswordCost(bcrypt.MinCost)
	code := m.Run()
	restore()
	os.Exit(code)
}

// newTestServer 起一个未安装态的服务，数据目录指向临时路径。
//
// plugins 传 nil：本包绝大多数用例与插件无关，而真起一个 Manager 就意味着
// 每个用例都要拉子进程。需要插件的用例见 adminplugin_test.go 里的 newAdminPluginEnv。
func newTestServer(t *testing.T) (*runtime.Runtime, http.Handler) {
	t.Helper()
	rt := runtime.New(t.TempDir())
	engine, close := New(rt, nil, false)
	t.Cleanup(close)
	return rt, engine
}

// do 发一个请求并返回响应记录。
func do(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	// 写操作需要通过 CSRF 双提交校验，这里同时给出 cookie 与请求头。
	if method != http.MethodGet && method != http.MethodHead {
		const token = "test-csrf-token"
		req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: token})
		req.Header.Set(auth.HeaderCSRF, token)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestBootstrapReportsNotInstalled 确认未安装时 bootstrap 如实上报。
func TestBootstrapReportsNotInstalled(t *testing.T) {
	_, handler := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/bootstrap", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap 应返回 200，实际 %d", rec.Code)
	}
	var body struct {
		Installed bool `json:"installed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.Installed {
		t.Error("未安装时 installed 应为 false")
	}
}

// TestProtectedRoutesBlockedBeforeInstall 确认未安装时业务接口一律 503，
// 而不是因为数据库句柄为 nil 而 panic。
func TestProtectedRoutesBlockedBeforeInstall(t *testing.T) {
	_, handler := newTestServer(t)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/catalog/categories"},
		{http.MethodGet, "/api/wallet"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/auth/login"},
	}
	for _, tc := range cases {
		rec := do(t, handler, tc.method, tc.path, map[string]string{})
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s 未安装时应返回 503，实际 %d", tc.method, tc.path, rec.Code)
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s 响应解析失败: %v", tc.method, tc.path, err)
			continue
		}
		if body.Code != "NOT_INSTALLED" {
			t.Errorf("%s %s 错误码应为 NOT_INSTALLED，实际 %q", tc.method, tc.path, body.Code)
		}
	}
}

// TestInstallThenRejectsSecondAttempt 走一遍完整安装，并确认重复安装被拒。
func TestInstallThenRejectsSecondAttempt(t *testing.T) {
	rt, handler := newTestServer(t)

	req := map[string]any{
		"database": map[string]any{
			"driver": "sqlite",
			"path":   filepath.Join(rt.DataDir(), "levis.db"),
		},
		"site_name":        "测试站点",
		"site_description": "用于测试",
		"admin_username":   "admin",
		"admin_email":      "admin@example.com",
		"admin_password":   "password123",
	}

	rec := do(t, handler, http.MethodPost, "/api/install", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("安装应返回 200，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	if !rt.Installed() {
		t.Fatal("安装后 runtime 应处于已安装态")
	}
	// 配置文件必须落盘，且权限收紧到 0600（含数据库密码与 JWT 密钥）。
	if !config.Exists(rt.DataDir()) {
		t.Fatal("安装后应生成 config.json")
	}
	info, err := os.Stat(config.Path(rt.DataDir()))
	if err != nil {
		t.Fatalf("读取配置文件信息失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json 权限应为 0600（含密码与密钥），实际 %#o", perm)
	}

	// 二次安装必须被拒绝，否则可被用来覆盖既有站点。
	rec = do(t, handler, http.MethodPost, "/api/install", req)
	if rec.Code != http.StatusConflict {
		t.Errorf("重复安装应返回 409，实际 %d", rec.Code)
	}

	// 安装完成后业务接口应放行（未登录时是 401 而不再是 503）。
	rec = do(t, handler, http.MethodGet, "/api/me", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("已安装但未登录时 /api/me 应返回 401，实际 %d", rec.Code)
	}

	// 管理员应能登录，且登录态 cookie 是 httpOnly。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
		"identifier": "admin",
		"password":   "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员登录应成功，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	var tokenCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.CookieToken {
			tokenCookie = cookie
		}
	}
	if tokenCookie == nil {
		t.Fatal("登录后应下发 token cookie")
	}
	if !tokenCookie.HttpOnly {
		t.Error("token cookie 必须是 httpOnly，否则 XSS 可直接窃取凭证")
	}
}

// TestCSRFBlocksWriteWithoutToken 确认缺少 CSRF 令牌的写操作被拒。
func TestCSRFBlocksWriteWithoutToken(t *testing.T) {
	rt, handler := newTestServer(t)
	installReq := map[string]any{
		"database": map[string]any{
			"driver": "sqlite",
			"path":   filepath.Join(rt.DataDir(), "levis.db"),
		},
		"site_name":      "测试站点",
		"admin_username": "admin",
		"admin_email":    "admin@example.com",
		"admin_password": "password123",
	}
	if rec := do(t, handler, http.MethodPost, "/api/install", installReq); rec.Code != http.StatusOK {
		t.Fatalf("安装失败: %d %s", rec.Code, rec.Body.String())
	}

	// 不带 CSRF cookie 与请求头，直接发写请求。
	body, _ := json.Marshal(map[string]string{"identifier": "admin", "password": "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("缺少 CSRF 令牌的写请求应返回 403，实际 %d", rec.Code)
	}
}

// TestBootstrapSeedsCSRFToken 锁定一个曾经的真实缺陷：全新访客手里没有任何
// cookie，若 GET 请求不播种 CSRF 令牌，安装/登录/注册这三个「第一次写操作」
// 会被 CSRF 中间件全部挡下，程序根本无法完成安装。
func TestBootstrapSeedsCSRFToken(t *testing.T) {
	_, handler := newTestServer(t)

	// 完全不带 cookie，模拟第一次打开页面。
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var csrf *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.CookieCSRF {
			csrf = cookie
		}
	}
	if csrf == nil || csrf.Value == "" {
		t.Fatal("GET /api/bootstrap 应给新访客下发 CSRF 令牌")
	}
	if csrf.HttpOnly {
		t.Error("CSRF cookie 不能是 httpOnly，前端拦截器需要读取它")
	}

	// 用刚拿到的令牌应能通过写操作的 CSRF 校验（此处安装参数为空，
	// 因此预期是参数校验失败 400，而不是 CSRF 的 403）。
	installReq := httptest.NewRequest(http.MethodPost, "/api/install", bytes.NewReader([]byte("{}")))
	installReq.Header.Set("Content-Type", "application/json")
	installReq.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: csrf.Value})
	installReq.Header.Set(auth.HeaderCSRF, csrf.Value)
	installRec := httptest.NewRecorder()
	handler.ServeHTTP(installRec, installReq)

	if installRec.Code == http.StatusForbidden {
		t.Error("带上播种令牌后写请求不应再被 CSRF 拦截")
	}
}

// TestUnknownAPIReturnsJSON404 确认未匹配的 API 路径返回 JSON 而不是 SPA 页面。
func TestUnknownAPIReturnsJSON404(t *testing.T) {
	_, handler := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/does-not-exist", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 API 应返回 404，实际 %d", rec.Code)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("未知 API 应返回 JSON 错误体: %v", err)
	}
	if body.Code != "NOT_FOUND" {
		t.Errorf("错误码应为 NOT_FOUND，实际 %q", body.Code)
	}
}

// TestSPAFallbackServesFrontend 确认前端深层路由不会 404，而是回退到 SPA 入口。
func TestSPAFallbackServesFrontend(t *testing.T) {
	_, handler := newTestServer(t)
	for _, path := range []string{"/", "/login", "/dashboard/invoices"} {
		rec := do(t, handler, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 应返回 200（SPA 回退），实际 %d", path, rec.Code)
		}
	}
}
