package server

import (
	"fmt"
	"net/http"
	"testing"
)

// TestPublicArticleNeedsPublished 确认公开读（按 slug 与按 ID）仅放行已发布文章。
func TestPublicArticleNeedsPublished(t *testing.T) {
	_, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	// 不存在的 ID 返回 404，非法 ID 返回 400。
	if rec := do(t, handler, http.MethodGet, "/api/articles/by-id/999999", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的文章应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, handler, http.MethodGet, "/api/articles/by-id/abc", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 ID 应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 建一篇草稿。
	var draft struct {
		ID uint `json:"id"`
	}
	rec := doAs(t, handler, http.MethodPost, "/api/admin/articles", map[string]any{
		"slug": "e2e-draft", "title": "草稿", "content_md": "# hi", "status": "draft",
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建草稿失败: %d %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &draft)
	if draft.ID == 0 {
		t.Fatal("创建草稿应返回文章 ID")
	}

	// 草稿：两种公开读法都应 404，避免探测草稿存在。
	if rec := do(t, handler, http.MethodGet, "/api/articles/e2e-draft", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("草稿按 slug 应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	byID := fmt.Sprintf("/api/articles/by-id/%d", draft.ID)
	if rec := do(t, handler, http.MethodGet, byID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("草稿按 ID 应返回 404，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 发布后两种公开读法都应 200。
	update := fmt.Sprintf("/api/admin/articles/%d", draft.ID)
	if rec := doAs(t, handler, http.MethodPatch, update, map[string]any{
		"slug": "e2e-draft", "title": "草稿", "content_md": "# hi", "status": "published",
	}, admin); rec.Code != http.StatusOK {
		t.Fatalf("发布失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, handler, http.MethodGet, "/api/articles/e2e-draft", nil); rec.Code != http.StatusOK {
		t.Fatalf("已发布按 slug 应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, handler, http.MethodGet, byID, nil); rec.Code != http.StatusOK {
		t.Fatalf("已发布按 ID 应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 公开列表只含已发布文章，且不泄露正文。
	if rec := do(t, handler, http.MethodGet, "/api/articles", nil); rec.Code != http.StatusOK {
		t.Fatalf("公开列表应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	} else {
		var items []struct {
			Slug      string `json:"slug"`
			ContentMD string `json:"content_md"`
		}
		decodeJSON(t, rec, &items)
		if len(items) != 1 || items[0].Slug != "e2e-draft" {
			t.Fatalf("公开列表应只含已发布文章，实际 %+v", items)
		}
		if items[0].ContentMD != "" {
			t.Fatal("公开列表不应返回正文 content_md")
		}
	}
}

// TestAdminBillingListsRequireAdmin 确认账单/订单/业务全局查询仅管理员可用。
func TestAdminBillingListsRequireAdmin(t *testing.T) {
	_, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	for _, path := range []string{
		"/api/admin/orders", "/api/admin/orders/1",
		"/api/admin/invoices", "/api/admin/invoices/1",
		"/api/admin/services",
	} {
		// 未登录 401。
		if rec := do(t, handler, http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("未登录访问 %s 应返回 401，实际 %d", path, rec.Code)
		}
		// 管理员空库 200（订单/账单明细不存在时 404 也可接受，只验权限门）。
		rec := doAs(t, handler, http.MethodGet, path, nil, admin)
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			t.Errorf("管理员访问 %s 应返回 200/404，实际 %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
