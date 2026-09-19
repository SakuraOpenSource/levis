package server

import (
	"net/http"
	"testing"
)

// TestRefundRoutesRequireAuth 确认退款接口全部需要登录。
func TestRefundRoutesRequireAuth(t *testing.T) {
	_, handler := installedServer(t)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/refunds"},
		{http.MethodPost, "/api/refunds"},
		{http.MethodPost, "/api/refunds/1/cancel"},
	} {
		if rec := do(t, handler, tc.method, tc.path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s %s 应返回 401，实际 %d", tc.method, tc.path, rec.Code)
		}
	}
}
