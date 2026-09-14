package server

import (
	"net/http"
	"testing"
)

// TestAdminPaymentPluginsRouteExists 回归：支付插件列表路由必须存在。
//
// 曾出现 handler 与前端都就绪、唯独缺了这一行路由注册的情况：
// 前端下拉永远为空，误以为插件不可用。无插件环境下返回空列表 200。
func TestAdminPaymentPluginsRouteExists(t *testing.T) {
	_, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	rec := doAs(t, handler, http.MethodGet, "/api/admin/payment-plugins", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("支付插件列表应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []any `json:"items"`
	}
	decodeJSON(t, rec, &out)
	if out.Items == nil {
		t.Fatal("items 应为数组（可为空），不应缺失")
	}
}
