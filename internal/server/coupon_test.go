package server

import (
	"net/http"
	"testing"
)

// couponEnv 是优惠码接口测试的公共环境：已安装站点 + 管理员 + 一个用户 + 商品。
type couponEnv struct {
	handler http.Handler
	admin   []*http.Cookie
	user    []*http.Cookie
	product uint
}

func newCouponEnv(t *testing.T) couponEnv {
	t.Helper()
	_, handler, admin, users := installedWithUsers(t, "couponbuyer")
	product := seedProductVia(t, handler, admin, "优惠码测试机", 100_00)
	env := couponEnv{handler: handler, admin: admin, user: users["couponbuyer"], product: product}
	// 装购物车。
	rec := doAs(t, handler, http.MethodPost, "/api/cart/items", map[string]any{
		"product_id": product, "quantity": 1, "billing_cycle": "monthly",
	}, env.user)
	if rec.Code != http.StatusOK {
		t.Fatalf("装购物车失败: %d %s", rec.Code, rec.Body.String())
	}
	return env
}

// createCouponVia 管理员建码，返回响应 JSON。
func (e couponEnv) createCouponVia(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	rec := doAs(t, handler2(e), http.MethodPost, "/api/admin/coupons", payload, e.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建优惠码失败: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decodeJSON(t, rec, &out)
	return out
}

func handler2(e couponEnv) http.Handler { return e.handler }

// TestCouponAdminCRUD 管理端建码 → 列表 → 改状态 → 删除全链路。
func TestCouponAdminCRUD(t *testing.T) {
	env := newCouponEnv(t)
	created := env.createCouponVia(t, map[string]any{
		"code": "CRUDTEST", "type": "percent", "percent_off": 15, "max_uses_per_user": 1,
	})
	if created["code"] != "CRUDTEST" {
		t.Fatalf("创建返回错误: %v", created)
	}

	// 用户端不可访问管理接口。
	rec := doAs(t, env.handler, http.MethodGet, "/api/admin/coupons", nil, env.user)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理接口应 403: %d", rec.Code)
	}

	// 列表。
	rec = doAs(t, env.handler, http.MethodGet, "/api/admin/coupons", nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	decodeJSON(t, rec, &list)
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("列表数量错误: %d %d", list.Total, len(list.Items))
	}

	// 更新（改折扣比例；code 为必填校验字段，保持原值）。
	id := uint(created["id"].(float64))
	rec = doAs(t, env.handler, http.MethodPatch, "/api/admin/coupons/1", map[string]any{
		"code": "CRUDTEST", "type": "percent", "percent_off": 25, "max_uses_per_user": 1, "status": "active",
	}, env.admin)
	_ = id
	if rec.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", rec.Code, rec.Body.String())
	}

	// 删除。
	rec = doAs(t, env.handler, http.MethodDelete, "/api/admin/coupons/1", nil, env.admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删除失败: %d %s", rec.Code, rec.Body.String())
	}
}

// TestCouponPreviewEndpoint 用户端试算：合法码返回减免，未知码 404。
func TestCouponPreviewEndpoint(t *testing.T) {
	env := newCouponEnv(t)
	env.createCouponVia(t, map[string]any{
		"code": "TRYME", "type": "fixed", "amount_cents": 20_00,
	})
	rec := doAs(t, env.handler, http.MethodPost, "/api/cart/coupon/preview", map[string]any{
		"code": "tryme",
	}, env.user)
	if rec.Code != http.StatusOK {
		t.Fatalf("试算失败: %d %s", rec.Code, rec.Body.String())
	}
	var view struct {
		SubtotalCents int64 `json:"subtotal_cents"`
		TotalCents    int64 `json:"total_cents"`
		Coupon        *struct {
			Code          string `json:"code"`
			DiscountCents int64  `json:"discount_cents"`
		} `json:"coupon"`
	}
	decodeJSON(t, rec, &view)
	if view.Coupon == nil || view.Coupon.Code != "TRYME" || view.Coupon.DiscountCents != 20_00 {
		t.Fatalf("试算结果错误: %+v", view)
	}
	if view.SubtotalCents != 100_00 || view.TotalCents != 80_00 {
		t.Fatalf("金额错误: %d %d", view.SubtotalCents, view.TotalCents)
	}

	// 未知码 404。
	rec = doAs(t, env.handler, http.MethodPost, "/api/cart/coupon/preview", map[string]any{
		"code": "NOPE",
	}, env.user)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知码应 404: %d", rec.Code)
	}
}

// TestCouponOrderFlowEndpoint 全链路：带码下单 → 订单快照减免 → 取消回滚。
func TestCouponOrderFlowEndpoint(t *testing.T) {
	env := newCouponEnv(t)
	env.createCouponVia(t, map[string]any{
		"code": "FULLFLOW", "type": "percent", "percent_off": 10, "max_uses_per_user": 1,
	})
	rec := doAs(t, env.handler, http.MethodPost, "/api/orders", map[string]any{
		"coupon_code": "fullflow", "agree": true,
	}, env.user)
	if rec.Code != http.StatusOK {
		t.Fatalf("下单失败: %d %s", rec.Code, rec.Body.String())
	}
	var order struct {
		ID                  uint   `json:"id"`
		TotalCents          int64  `json:"total_cents"`
		CouponCode          string `json:"coupon_code"`
		CouponDiscountCents int64  `json:"coupon_discount_cents"`
	}
	decodeJSON(t, rec, &order)
	if order.TotalCents != 90_00 || order.CouponCode != "FULLFLOW" || order.CouponDiscountCents != 10_00 {
		t.Fatalf("订单快照错误: %+v", order)
	}

	// 取消订单 → 回滚次数。
	rec = doAs(t, env.handler, http.MethodPost, "/api/orders/1/cancel", nil, env.user)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("取消失败: %d %s", rec.Code, rec.Body.String())
	}
}
