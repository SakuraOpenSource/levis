package server

import (
	"net/http"
	"testing"
)

// 本文件覆盖 P0 安全加固的接口行为：登录失败限速与管理员入口分离。
// 限速器本身的时间维度逻辑（锁定时长、退避、清扫）在 internal/loginlimit
// 的单元测试里用假时钟验证；这里只验证接口层的接线正确。

// loginAttemptPayload 构造一次登录请求的请求体。
func loginAttemptPayload(identifier, password, captchaCode string) map[string]string {
	return map[string]string{
		"identifier":   identifier,
		"password":     password,
		"captcha_id":   "test",
		"captcha_code": captchaCode,
	}
}

// 连续密码错误达到阈值后账号被锁定：正确密码也进不来，直到锁定期结束。
func TestLoginRateLimitLocksAccount(t *testing.T) {
	_, handler := installedServer(t)
	// 先注册一个普通用户，避免管理员在普通入口被定向拒绝的干扰。
	admin := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		Charset: "digit",
		Length:  6,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭验证码失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "alice",
		"email":    "alice@example.com",
		"password": "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", rec.Code, rec.Body.String())
	}

	// 5 次错误密码（阈值）。
	for i := 0; i < 5; i++ {
		rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("alice", "wrong-password", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应返回 401，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	// 第 6 次换成正确密码：仍应被 429 拒绝 —— 锁定期内不校验凭证，
	// 否则爆破方可以继续把这里当预言机用。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("alice", "password123", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定后正确密码也应返回 429，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 响应应带 Retry-After 头")
	}
	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "TOO_MANY_REQUESTS" {
		t.Fatalf("错误码应为 TOO_MANY_REQUESTS，实际 %q", body.Code)
	}
	// 锁定按账号计：换一个 IP 概念上无效（httptest 固定来源），但别的账号不受影响。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("admin", "password123", testCaptchaAnswer))
	if rec.Code != http.StatusForbidden {
		// admin 走普通入口本来就 403（入口分离）；只要不是 429 就说明
		// alice 的锁定没有外溢到别的账号。
		t.Fatalf("别的账号不应被连坐，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 不存在的账号同样计数：撞库探测「这个账号在不在」不该有免费的无限次。
func TestLoginRateLimitCountsUnknownIdentifier(t *testing.T) {
	_, handler := installedServer(t)
	for i := 0; i < 5; i++ {
		rec := do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("nosuchuser", "whatever", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应返回 401，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("nosuchuser", "whatever", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("未知账号刷满阈值后应返回 429，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 入口分离：普通入口拒绝管理员并给出专用入口引导；管理员入口拒绝
// 普通用户；两个入口的失败计数互不干扰。
func TestAdminLoginEntrySeparation(t *testing.T) {
	_, handler := installedServer(t)
	// 建一个普通用户。
	admin := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		Charset: "digit",
		Length:  6,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭验证码失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "bob",
		"email":    "bob@example.com",
		"password": "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", rec.Code, rec.Body.String())
	}

	// 管理员走普通入口：403 + ADMIN_ENTRY_REQUIRED。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("admin", "password123", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("管理员走普通入口应 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "ADMIN_ENTRY_REQUIRED" {
		t.Fatalf("错误码应为 ADMIN_ENTRY_REQUIRED，实际 %q", body.Code)
	}

	// 管理员入口必须带验证码：站点关闭登录验证码也不放行（VerifyForced）。
	rec = do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("admin", "password123", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("管理员入口无验证码应 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 带魔法验证码的管理员入口登录成功。
	rec = do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("admin", "password123", testCaptchaAnswer))
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员入口登录应成功，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 普通用户走管理员入口：403（凭证正确仍拒，防止把管理员入口变成
	// 普通账号的密码预言机）。
	rec = do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("bob", "password123", testCaptchaAnswer))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户走管理员入口应 403，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 普通用户在管理员入口失败若干次不影响他在普通入口登录（scope 隔离）。
	// 注意 403 也按失败计，bob 在管理员 scope 已有 1 次：再来 3 次错误密码
	// 共 4 次，仍在阈值内（全部 401），尚未锁定。
	for i := 0; i < 3; i++ {
		rec = do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("bob", "wrong-password", testCaptchaAnswer))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("bob", "password123", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员入口的失败不应锁普通入口，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 反过来：把 admin 在普通入口彻底锁死（此前两次 403 已计 2 次失败，
	// 两次错误密码到 4 次，再来一次正确密码被 403 拒并达到 5 次阈值）。
	for i := 0; i < 2; i++ {
		rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("admin", "wrong-password", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("普通入口第 %d 次错误密码应 401，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	// 正确密码也进不了普通入口（仍 403，且这次把计数推到阈值触发锁定）。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("admin", "password123", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("管理员走普通入口应 403，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 锁定生效：普通入口任何尝试直接 429。
	rec = do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("admin", "anything", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("普通入口锁定后应 429，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 管理员入口不受影响 —— 攻击者不能靠在普通入口刷 admin 的错误密码，
	// 把真管理员锁在门外。
	rec = do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("admin", "password123", testCaptchaAnswer))
	if rec.Code != http.StatusOK {
		t.Fatalf("普通入口的锁定不应影响管理员入口，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 管理员会话有效期应比普通用户短（12 小时 vs 7 天）。
func TestAdminSessionIsShorter(t *testing.T) {
	_, handler := installedServer(t)
	adminRec := do(t, handler, http.MethodPost, "/api/admin/login", loginAttemptPayload("admin", "password123", testCaptchaAnswer))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("管理员登录失败: %d %s", adminRec.Code, adminRec.Body.String())
	}
	adminTTL := cookieMaxAge(t, adminRec.Result().Cookies())

	// 建普通用户并登录。
	admin := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodPut, "/api/admin/settings/captcha", captchaConfig{
		Charset: "digit",
		Length:  6,
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭验证码失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, handler, http.MethodPost, "/api/auth/register", map[string]string{
		"username": "carol",
		"email":    "carol@example.com",
		"password": "password123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", rec.Code, rec.Body.String())
	}
	userRec := do(t, handler, http.MethodPost, "/api/auth/login", loginAttemptPayload("carol", "password123", ""))
	if userRec.Code != http.StatusOK {
		t.Fatalf("普通登录失败: %d %s", userRec.Code, userRec.Body.String())
	}
	userTTL := cookieMaxAge(t, userRec.Result().Cookies())

	if adminTTL >= userTTL {
		t.Fatalf("管理员会话（%d 秒）应短于普通用户（%d 秒）", adminTTL, userTTL)
	}
}

// cookieMaxAge 找出 levis_token cookie 的 MaxAge。
func cookieMaxAge(t *testing.T, cookies []*http.Cookie) int {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == "levis_token" {
			return cookie.MaxAge
		}
	}
	t.Fatal("响应里没有 levis_token cookie")
	return 0
}
