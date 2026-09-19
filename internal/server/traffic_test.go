package server

import (
	"fmt"
	"net/http"
	"testing"
)

// TestServiceTrafficEndpoint 覆盖流量进度端点的核心行为：
// 归属校验（他人服务 404）、不限流量（unlimited=true、percent=0）、
// 有限配额的百分比封顶与超限标记。
func TestServiceTrafficEndpoint(t *testing.T) {
	rt, handler, admin, users := installedWithUsers(t, "alice")
	userCookies := users["alice"]
	_ = rt

	// 本地商品 + 代开服务（admin=1，alice=2）。
	cat := doAs(t, handler, http.MethodPost, "/api/admin/categories", map[string]any{
		"name": "本地", "slug": "local-traffic",
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
	// 服务开给 alice（代开接口按用户名找用户）。
	aliceRow := doAs(t, handler, http.MethodGet, "/api/admin/users?keyword=alice", nil, admin)
	if aliceRow.Code != http.StatusOK {
		t.Fatalf("查询用户失败: %d %s", aliceRow.Code, aliceRow.Body.String())
	}
	var usersOut struct {
		Items []struct {
			ID       uint   `json:"id"`
			Username string `json:"username"`
		} `json:"items"`
	}
	decodeJSON(t, aliceRow, &usersOut)
	if len(usersOut.Items) == 0 {
		t.Fatal("应能查到 alice")
	}
	svc := doAs(t, handler, http.MethodPost, fmt.Sprintf("/api/admin/users/%d/services", usersOut.Items[0].ID), map[string]any{
		"product_id": prodOut.ID, "billing_cycle": "monthly",
	}, admin)
	if svc.Code != http.StatusOK {
		t.Fatalf("代开服务失败: %d %s", svc.Code, svc.Body.String())
	}
	var svcOut struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, svc, &svcOut)

	type trafficProgress struct {
		UsedBytes     int64   `json:"used_bytes"`
		QuotaGB       int     `json:"quota_gb"`
		UsedGB        float64 `json:"used_gb"`
		Percent       float64 `json:"percent"`
		Unlimited     bool    `json:"unlimited"`
		Exceeded      bool    `json:"exceeded"`
		SuspendReason string  `json:"suspend_reason"`
	}

	// 不限流量：本地服务无 traffic 选配无加购。
	rec := doAs(t, handler, http.MethodGet, fmt.Sprintf("/api/services/%d/traffic", svcOut.ID), nil, userCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取流量进度失败: %d %s", rec.Code, rec.Body.String())
	}
	var progress trafficProgress
	decodeJSON(t, rec, &progress)
	if !progress.Unlimited || progress.QuotaGB != 0 || progress.Percent != 0 {
		t.Fatalf("应不限流量：unlimited=%v quota=%d percent=%f", progress.Unlimited, progress.QuotaGB, progress.Percent)
	}

	// 归属校验：admin 读 alice 的服务，不是本人即 404（与 metrics 一致）。
	rec = doAs(t, handler, http.MethodGet, fmt.Sprintf("/api/services/%d/traffic", svcOut.ID), nil, admin)
	if rec.Code == http.StatusOK {
		t.Fatal("非所有者读取他人服务流量应被拒绝")
	}

	// 直接改库制造有限配额 + 超限用量，验证百分比封顶与 exceeded。
	if err := rt.DB().Exec("UPDATE services SET traffic_extra_gb = 1, traffic_used_bytes = 2147483648 WHERE id = ?", svcOut.ID).Error; err != nil {
		t.Fatalf("写入测试流量数据失败: %v", err)
	}
	rec = doAs(t, handler, http.MethodGet, fmt.Sprintf("/api/services/%d/traffic", svcOut.ID), nil, userCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取流量进度失败: %d %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &progress)
	if progress.Unlimited || progress.QuotaGB != 1 {
		t.Fatalf("应有限配额 1GB：unlimited=%v quota=%d", progress.Unlimited, progress.QuotaGB)
	}
	if progress.Percent != 100 {
		t.Fatalf("超限百分比应封顶 100，实际 %f", progress.Percent)
	}
	if !progress.Exceeded {
		t.Fatal("用量 2GiB 超过 1GB 配额应标记 exceeded")
	}
}
