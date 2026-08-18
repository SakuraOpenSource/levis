package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// doAs 与 do 相同，但额外带上登录态 cookie。
func doAs(t *testing.T, handler http.Handler, method, path string, body any, cookies []*http.Cookie) *httptest.ResponseRecorder {
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
	var csrf string
	for _, cookie := range cookies {
		req.AddCookie(cookie)
		if cookie.Name == auth.CookieCSRF {
			csrf = cookie.Value
		}
	}
	if method != http.MethodGet && method != http.MethodHead {
		// 传入的 cookie 里没有 CSRF 令牌时补一个，双提交只要求两处一致。
		if csrf == "" {
			csrf = "test-csrf-token"
			req.AddCookie(&http.Cookie{Name: auth.CookieCSRF, Value: csrf})
		}
		req.Header.Set(auth.HeaderCSRF, csrf)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// installedServer 起一个已完成安装的服务。
func installedServer(t *testing.T) (*runtime.Runtime, http.Handler) {
	t.Helper()
	rt, handler := newTestServer(t)
	installVia(t, rt, handler)
	return rt, handler
}

// installVia 走一遍安装接口把 rt 激活。
//
// 与 installedServer 分开是为了让自带插件管理器的服务（见 adminplugin_test.go）
// 也能复用同一套安装参数 —— 那种服务必须在 New 时就把 Manager 传进去，没法
// 从 installedServer 的返回值往回改。
func installVia(t *testing.T, rt *runtime.Runtime, handler http.Handler) {
	t.Helper()
	req := map[string]any{
		"database": map[string]any{
			"driver": "sqlite",
			"path":   filepath.Join(rt.DataDir(), "levis.db"),
		},
		"site_name":      "测试站点",
		"admin_username": "admin",
		"admin_email":    "admin@example.com",
		"admin_password": "password123",
	}
	if rec := do(t, handler, http.MethodPost, "/api/install", req); rec.Code != http.StatusOK {
		t.Fatalf("安装失败: %d %s", rec.Code, rec.Body.String())
	}
}

// loginAs 登录并返回登录态 cookie。
func loginAs(t *testing.T, handler http.Handler, identifier, password string) []*http.Cookie {
	t.Helper()
	rec := do(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
		"identifier": identifier,
		"password":   password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("登录 %s 失败: %d %s", identifier, rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()
}

// captchaConfig 是设置接口的响应体。
type captchaConfig struct {
	LoginEnabled    bool   `json:"login_enabled"`
	RegisterEnabled bool   `json:"register_enabled"`
	Charset         string `json:"charset"`
	Length          int    `json:"length"`
}

// decodeJSON 解析响应体。
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("解析响应失败: %v，响应: %s", err, rec.Body.String())
	}
}

// 验证码接口要读站点配置，未安装时应和其他业务接口一样返回 503。
func TestCaptchaBlockedBeforeInstall(t *testing.T) {
	_, handler := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/captcha", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未安装时应返回 503，实际 %d", rec.Code)
	}
}

// 签发接口必须对未登录用户开放 —— 登录、注册页正是在没有登录态时用它。
func TestCaptchaIssueIsPublic(t *testing.T) {
	_, handler := installedServer(t)
	rec := do(t, handler, http.MethodGet, "/api/captcha", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("未登录取验证码应返回 200，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID        string `json:"id"`
		Image     string `json:"image"`
		ExpiresIn int    `json:"expires_in"`
	}
	decodeJSON(t, rec, &body)
	if body.ID == "" {
		t.Error("响应缺少验证码 id")
	}
	if !strings.HasPrefix(body.Image, "data:image/png;base64,") {
		t.Errorf("图片应为 PNG data URL，实际 %.40s", body.Image)
	}
	if body.ExpiresIn <= 0 {
		t.Errorf("expires_in 应为正数，实际 %d", body.ExpiresIn)
	}
	// 响应里只允许这三个字段：多出任何字段都可能是把答案一起发了出去。
	var raw map[string]any
	decodeJSON(t, rec, &raw)
	for key := range raw {
		switch key {
		case "id", "image", "expires_in":
		default:
			t.Errorf("响应出现意料之外的字段 %q，疑似泄露答案", key)
		}
	}
}

// 每次请求都应是一张新图，不能复用同一个 id。
func TestCaptchaIssueReturnsFreshChallenge(t *testing.T) {
	_, handler := installedServer(t)
	seen := make(map[string]bool)
	for range 3 {
		rec := do(t, handler, http.MethodGet, "/api/captcha", nil)
		var body struct{ ID string }
		decodeJSON(t, rec, &body)
		if seen[body.ID] {
			t.Fatalf("验证码 id 重复: %s", body.ID)
		}
		seen[body.ID] = true
	}
}

// 默认注册开启验证码：不带验证码的注册必须被拒，否则脚本可以直接批量刷号。
func TestRegisterRequiresCaptchaByDefault(t *testing.T) {
	rt, handler := installedServer(t)
	rec := do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"email":    "alice@example.com",
		"password": "password123",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少验证码的注册应返回 400，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	// 拦下之后不能留下半个账号。
	var count int64
	if err := rt.DB().Table("users").Where("username = ?", "alice").Count(&count).Error; err != nil {
		t.Fatalf("统计用户失败: %v", err)
	}
	if count != 0 {
		t.Fatal("验证码校验失败却仍然建了用户")
	}
}

// 答案错误同样要被拒，且要给出可读提示。
func TestRegisterRejectsWrongCaptcha(t *testing.T) {
	_, handler := installedServer(t)
	issue := do(t, handler, http.MethodGet, "/api/captcha", nil)
	var challenge struct{ ID string }
	decodeJSON(t, issue, &challenge)

	rec := do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username":     "alice",
		"email":        "alice@example.com",
		"password":     "password123",
		"captcha_id":   challenge.ID,
		"captcha_code": "@@@@@@",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("错误验证码应返回 400，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	var body struct{ Message string }
	decodeJSON(t, rec, &body)
	if body.Message == "" {
		t.Error("应给出可读的错误提示")
	}
}

// 默认登录不开验证码：老用户的登录流程不能被这次改动打断。
func TestLoginWorksWithoutCaptchaByDefault(t *testing.T) {
	_, handler := installedServer(t)
	rec := do(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
		"identifier": "admin",
		"password":   "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("登录验证码默认关闭，应能直接登录，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
}

// 开启登录验证码后，不带验证码的登录必须被拒 —— 而且要在校验密码之前拦下。
func TestLoginRequiresCaptchaWhenEnabled(t *testing.T) {
	_, handler := installedServer(t)
	cookies := loginAs(t, handler, "admin", "password123")

	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		LoginEnabled:    true,
		RegisterEnabled: true,
		Charset:         "mixed",
		Length:          5,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存设置应返回 200，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}

	// 密码正确但没带验证码，仍应被拒。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
		"identifier": "admin",
		"password":   "password123",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("开启后缺少验证码的登录应返回 400，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
}

// 关闭注册验证码后，注册应恢复为一步完成。
func TestRegisterWorksWhenCaptchaDisabled(t *testing.T) {
	_, handler := installedServer(t)
	cookies := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		LoginEnabled:    false,
		RegisterEnabled: false,
		Charset:         "digit",
		Length:          6,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存设置应返回 200，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"email":    "alice@example.com",
		"password": "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭验证码后注册应成功，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
}

// 设置接口只对管理员开放。
func TestCaptchaSettingsRequireAdmin(t *testing.T) {
	_, handler := installedServer(t)

	// 未登录。
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := do(t, handler, method, "/api/admin/settings/captcha", captchaConfig{})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s 应返回 401，实际 %d", method, rec.Code)
		}
	}

	// 先关掉注册验证码，才能造一个普通用户。
	adminCookies := loginAs(t, handler, "admin", "password123")
	if rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		Charset: "digit", Length: 6,
	}, adminCookies); rec.Code != http.StatusOK {
		t.Fatalf("关闭注册验证码失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice", "email": "alice@example.com", "password": "password123",
	}); rec.Code != http.StatusOK {
		t.Fatalf("注册普通用户失败: %d %s", rec.Code, rec.Body.String())
	}

	userCookies := loginAs(t, handler, "alice", "password123")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := doAs(t, handler, method, "/api/admin/settings/captcha", captchaConfig{
			Charset: "digit", Length: 6,
		}, userCookies)
		if rec.Code != http.StatusForbidden {
			t.Errorf("普通用户 %s 应返回 403，实际 %d，响应: %s", method, rec.Code, rec.Body.String())
		}
	}
}

// 空库读设置应给出默认值：注册开、登录关、6 位纯数字。
func TestCaptchaSettingsReturnDefaults(t *testing.T) {
	_, handler := installedServer(t)
	cookies := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodGet, "/api/admin/settings/captcha", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置应返回 200，实际 %d，响应: %s", rec.Code, rec.Body.String())
	}
	var got captchaConfig
	decodeJSON(t, rec, &got)
	want := captchaConfig{LoginEnabled: false, RegisterEnabled: true, Charset: "digit", Length: 6}
	if got != want {
		t.Fatalf("默认设置 = %+v，期望 %+v", got, want)
	}
}

// 保存后 bootstrap 要跟着变：前端靠它决定登录、注册页是否显示验证码。
func TestBootstrapReflectsCaptchaSettings(t *testing.T) {
	_, handler := installedServer(t)

	rec := do(t, handler, http.MethodGet, "/api/bootstrap", nil)
	var before struct {
		Captcha struct {
			Login    bool   `json:"login"`
			Register bool   `json:"register"`
			Charset  string `json:"charset"`
		} `json:"captcha"`
	}
	decodeJSON(t, rec, &before)
	if before.Captcha.Login || !before.Captcha.Register || before.Captcha.Charset != "digit" {
		t.Fatalf("bootstrap 默认值不对: %+v", before.Captcha)
	}

	cookies := loginAs(t, handler, "admin", "password123")
	if rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		LoginEnabled: true, RegisterEnabled: false, Charset: "letter", Length: 4,
	}, cookies); rec.Code != http.StatusOK {
		t.Fatalf("保存设置失败: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, handler, http.MethodGet, "/api/bootstrap", nil)
	var after struct {
		Captcha struct {
			Login    bool   `json:"login"`
			Register bool   `json:"register"`
			Charset  string `json:"charset"`
		} `json:"captcha"`
	}
	decodeJSON(t, rec, &after)
	if !after.Captcha.Login || after.Captcha.Register || after.Captcha.Charset != "letter" {
		t.Fatalf("bootstrap 未跟随设置变化: %+v", after.Captcha)
	}
}

// bootstrap 只暴露开关与字符集，不能把位数一并交出去 —— 位数是穷举时的关键信息。
func TestBootstrapDoesNotExposeCaptchaLength(t *testing.T) {
	_, handler := installedServer(t)
	rec := do(t, handler, http.MethodGet, "/api/bootstrap", nil)
	var body struct {
		Captcha map[string]any `json:"captcha"`
	}
	decodeJSON(t, rec, &body)
	if _, ok := body.Captcha["length"]; ok {
		t.Errorf("bootstrap 不应包含 length：%v", body.Captcha)
	}
}

func TestCaptchaSettingsRejectInvalidPayload(t *testing.T) {
	_, handler := installedServer(t)
	cookies := loginAs(t, handler, "admin", "password123")
	cases := []struct {
		name string
		body captchaConfig
	}{
		{"未知字符集", captchaConfig{Charset: "number", Length: 6}},
		{"字符集为空", captchaConfig{Charset: "", Length: 6}},
		{"位数过小", captchaConfig{Charset: "digit", Length: 3}},
		{"位数过大", captchaConfig{Charset: "digit", Length: 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", tc.body, cookies)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%+v 应返回 400，实际 %d，响应: %s", tc.body, rec.Code, rec.Body.String())
			}
		})
	}
	// 全部拒绝之后设置应保持默认。
	rec := doAs(t, handler, http.MethodGet, "/api/admin/settings/captcha", nil, cookies)
	var got captchaConfig
	decodeJSON(t, rec, &got)
	if got != (captchaConfig{RegisterEnabled: true, Charset: "digit", Length: 6}) {
		t.Fatalf("非法请求污染了设置: %+v", got)
	}
}

// 保存设置是写操作，必须受 CSRF 保护。
func TestCaptchaSettingsRequireCSRF(t *testing.T) {
	_, handler := installedServer(t)
	cookies := loginAs(t, handler, "admin", "password123")

	body, _ := json.Marshal(captchaConfig{Charset: "digit", Length: 6})
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/captcha", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	// 刻意不设置 X-CSRF-Token。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 令牌应返回 403，实际 %d", rec.Code)
	}
}
