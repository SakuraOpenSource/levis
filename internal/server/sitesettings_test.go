package server

import (
	"net/http"
	"testing"
)

// TestAdminSiteSettingsTrafficPrice 覆盖流量包兜底单价的读写闭环：
// 默认未定价 → 保存生效 → 回读一致 → 不携带字段不清价 → 0 清价 → 非法值被拒。
func TestAdminSiteSettingsTrafficPrice(t *testing.T) {
	_, handler, adminCookies, _ := installedWithUsers(t, "alice")

	type siteSettings struct {
		SiteName               string `json:"site_name"`
		SiteDescription        string `json:"site_description"`
		TrafficPricePerGBCents int64  `json:"traffic_price_per_gb_cents"`
	}

	// 默认未定价。
	rec := doAs(t, handler, http.MethodGet, "/api/admin/settings/site", nil, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取站点设置失败: %d %s", rec.Code, rec.Body.String())
	}
	var settings siteSettings
	decodeJSON(t, rec, &settings)
	if settings.TrafficPricePerGBCents != 0 {
		t.Fatalf("默认流量单价应为 0，实际 %d", settings.TrafficPricePerGBCents)
	}

	// 保存 150 分/GB。
	rec = doAs(t, handler, http.MethodPut, "/api/admin/settings/site", map[string]any{
		"site_name":                  "测试站点",
		"site_description":           "",
		"traffic_price_per_gb_cents": 150,
	}, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存站点设置应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &settings)
	if settings.TrafficPricePerGBCents != 150 {
		t.Fatalf("保存后响应的单价应为 150，实际 %d", settings.TrafficPricePerGBCents)
	}

	// 回读一致。
	rec = doAs(t, handler, http.MethodGet, "/api/admin/settings/site", nil, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("回读站点设置失败: %d %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &settings)
	if settings.TrafficPricePerGBCents != 150 {
		t.Fatalf("回读的流量单价应为 150，实际 %d", settings.TrafficPricePerGBCents)
	}

	// 旧客户端不携带该字段时不应清掉已配好的价格。
	rec = doAs(t, handler, http.MethodPut, "/api/admin/settings/site", map[string]any{
		"site_name": "测试站点2",
	}, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("不携带单价字段的保存应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doAs(t, handler, http.MethodGet, "/api/admin/settings/site", nil, adminCookies)
	decodeJSON(t, rec, &settings)
	if settings.TrafficPricePerGBCents != 150 || settings.SiteName != "测试站点2" {
		t.Fatalf("不携带单价字段时应保留原价并更新名称，实际单价 %d、名称 %q", settings.TrafficPricePerGBCents, settings.SiteName)
	}

	// 显式传 0 清除定价。
	rec = doAs(t, handler, http.MethodPut, "/api/admin/settings/site", map[string]any{
		"site_name":                  "测试站点",
		"traffic_price_per_gb_cents": 0,
	}, adminCookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("传 0 清除定价应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doAs(t, handler, http.MethodGet, "/api/admin/settings/site", nil, adminCookies)
	decodeJSON(t, rec, &settings)
	if settings.TrafficPricePerGBCents != 0 {
		t.Fatalf("传 0 后单价应为 0，实际 %d", settings.TrafficPricePerGBCents)
	}

	// 负数单价被拒绝。
	rec = doAs(t, handler, http.MethodPut, "/api/admin/settings/site", map[string]any{
		"site_name":                  "测试站点",
		"traffic_price_per_gb_cents": -1,
	}, adminCookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("负数单价应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}
