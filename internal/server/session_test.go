package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/auth"
)

// 本文件覆盖 P2 的两级会话失效：登出吊销（auth.RevocationList）与
// 改密踢会话（password_changed_at 与 JWT iat 比对）。

// tokenCookie 从登录响应的 cookie 里取出 levis_token。
func tokenCookie(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == auth.CookieToken {
			return cookie
		}
	}
	t.Fatal("登录响应里没有 token cookie")
	return nil
}

// 登出后旧 token 必须立即失效，而不是等它自然过期。
func TestLogoutRevokesToken(t *testing.T) {
	_, handler, _, _ := installedWithUsers(t, "alice")
	session := loginAs(t, handler, "alice", "password123")

	// 登出前可用。
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, session); rec.Code != http.StatusOK {
		t.Fatalf("登出前 /api/me 应 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	if rec := doAs(t, handler, http.MethodPost, "/api/auth/logout", nil, session); rec.Code != http.StatusNoContent {
		t.Fatalf("登出应 204，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 拿着登出前的 token 重放：必须 401。
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("登出后旧 token 应被拒，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 重新登录拿到的是新 token，正常可用。
	fresh := loginAs(t, handler, "alice", "password123")
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, fresh); rec.Code != http.StatusOK {
		t.Fatalf("重新登录后应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 改密后，改密前签发的所有会话（含其他设备）一并失效，改密后签发的正常。
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	_, handler, _, _ := installedWithUsers(t, "alice")
	deviceA := loginAs(t, handler, "alice", "password123")
	deviceB := loginAs(t, handler, "alice", "password123")
	if tokenCookie(t, deviceA).Value == tokenCookie(t, deviceB).Value {
		t.Fatal("两次登录应签发不同的 token")
	}

	// iat 只有秒精度，改密同秒内签发的 token 不在踢除范围（设计取舍：
	// 换来改密后立即重签的新 token 不被自己误伤）。这里跨过秒边界再改密，
	// 保证 deviceA/deviceB 的 iat 落在改密时间的上一秒。
	time.Sleep(1100 * time.Millisecond)
	// 设备 A 改密（成功后服务端会重签新 token）。
	rec := doAs(t, handler, http.MethodPost, "/api/me/password", map[string]string{
		"old_password": "password123",
		"new_password": "new-password-456",
	}, deviceA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("改密应 204，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 设备 B 的旧 token：401（密码已变更）。
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, deviceB); rec.Code != http.StatusUnauthorized {
		t.Fatalf("改密后其他设备的旧 token 应被拒，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 设备 A 的旧 token 同样被踢；改密响应里重签的新 token 可用 ——
	// 简单起见用新密码重新登录验证新凭证路径正常。
	fresh := loginAs(t, handler, "alice", "new-password-456")
	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, fresh); rec.Code != http.StatusOK {
		t.Fatalf("新密码登录应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 旧密码不能再登录。
	if rec := do(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
		"identifier": "alice", "password": "password123",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应登录失败，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 管理员重置用户密码同样踢掉该用户的所有既有会话。
func TestAdminResetRevokesUserSessions(t *testing.T) {
	_, handler, admin, users := installedWithUsers(t, "alice")
	session := users["alice"]

	// 找到 alice 的用户 ID。
	rec := doAs(t, handler, http.MethodGet, "/api/admin/users?page=1&page_size=50", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("拉用户列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			ID       uint   `json:"id"`
			Username string `json:"username"`
		} `json:"items"`
	}
	decodeJSON(t, rec, &list)
	var aliceID uint
	for _, u := range list.Items {
		if u.Username == "alice" {
			aliceID = u.ID
		}
	}
	if aliceID == 0 {
		t.Fatal("用户列表里没找到 alice")
	}

	// 同上：跨过秒边界再重置，alice 会话的 iat 落在重置时间的上一秒。
	time.Sleep(1100 * time.Millisecond)
	rec = doAs(t, handler, http.MethodPatch, "/api/admin/users/"+itoa(aliceID), map[string]any{
		"password": "admin-reset-789",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员重置密码失败: %d %s", rec.Code, rec.Body.String())
	}

	if rec := doAs(t, handler, http.MethodGet, "/api/me", nil, session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("管理员重置密码后旧会话应被拒，实际 %d %s", rec.Code, rec.Body.String())
	}
}
