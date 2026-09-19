package server

import (
	"net/http"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/auth"
)

// 管理员改密后重签的会话仍须使用管理员有效期，不能变成普通用户的七天。
func TestPasswordChangePreservesAdminSessionTTL(t *testing.T) {
	rt, handler := installedServer(t)
	session := loginAs(t, handler, "admin", "password123")
	rec := doAs(t, handler, http.MethodPost, "/api/me/password", map[string]string{
		"old_password": "password123",
		"new_password": "new-admin-password-456",
	}, session)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("管理员改密失败: %d %s", rec.Code, rec.Body.String())
	}
	cookie := tokenCookie(t, rec.Result().Cookies())
	if cookie.MaxAge != int(auth.AdminTokenTTL.Seconds()) {
		t.Errorf("管理员改密后 cookie 有效期应为 %v，实际 %d 秒", auth.AdminTokenTTL, cookie.MaxAge)
	}
	claims, err := auth.ParseToken(rt.JWTSecret(), cookie.Value)
	if err != nil {
		t.Fatalf("解析重签会话失败: %v", err)
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatal("重签会话缺少有效期")
	}
	if ttl := claims.ExpiresAt.Sub(claims.IssuedAt.Time); ttl != auth.AdminTokenTTL {
		t.Errorf("管理员改密后 JWT 有效期应为 %v，实际 %v", auth.AdminTokenTTL, ttl)
	}
	if fresh := doAs(t, handler, http.MethodGet, "/api/me", nil, rec.Result().Cookies()); fresh.Code != http.StatusOK {
		t.Fatalf("改密后重签会话应立即可用: %d %s", fresh.Code, fresh.Body.String())
	}
}
