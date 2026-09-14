package server

import (
	"fmt"
	"net/http"
	"testing"
)

// TestServiceMetricsRouteExists 回归：服务实时监控路由必须存在。
// 未绑定上游的服务应返回 400（路由存在、业务校验工作），而不是 404。
func TestServiceMetricsRouteExists(t *testing.T) {
	_, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	// 建一个本地商品并由管理员代开，得到未绑定上游的服务。
	cat := doAs(t, handler, http.MethodPost, "/api/admin/categories", map[string]any{
		"name": "本地", "slug": "local",
	}, admin)
	if cat.Code != http.StatusOK {
		t.Fatalf("建分组失败: %d %s", cat.Code, cat.Body.String())
	}
	var catOut struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, cat, &catOut)
	prod := doAs(t, handler, http.MethodPost, "/api/admin/products", map[string]any{
		"category_id": catOut.ID, "name": "本地包", "price_cents": 100,
		"billing_cycle": "monthly", "stock": -1, "status": "active",
	}, admin)
	if prod.Code != http.StatusOK {
		t.Fatalf("建商品失败: %d %s", prod.Code, prod.Body.String())
	}
	var prodOut struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, prod, &prodOut)
	svc := doAs(t, handler, http.MethodPost, "/api/admin/users/1/services", map[string]any{
		"product_id": prodOut.ID, "billing_cycle": "monthly",
	}, admin)
	if svc.Code != http.StatusOK {
		t.Fatalf("代开服务失败: %d %s", svc.Code, svc.Body.String())
	}
	var svcOut struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, svc, &svcOut)
	if svcOut.ID == 0 {
		t.Fatal("代开服务应返回服务 ID")
	}

	// 未绑定上游：400 证明路由存在；404 则说明路由缺失。
	rec := doAs(t, handler, http.MethodGet, fmt.Sprintf("/api/services/%d/metrics", svcOut.ID), nil, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未绑定上游的服务应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}
