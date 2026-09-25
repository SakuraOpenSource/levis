package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// ---- 管理端：优惠码 CRUD 与批量生成 ----

// AdminCoupons 分页返回优惠码，支持 status 过滤。
func (h *Handler) AdminCoupons(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.coupons().AdminList(c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminCoupon 返回单个优惠码。
func (h *Handler) AdminCoupon(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.coupons().AdminGet(id)
	respond(c, item, err)
}

// AdminCreateCoupon 创建优惠码（管理员手写 code）。
func (h *Handler) AdminCreateCoupon(c *gin.Context) {
	var req service.CouponInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.coupons().Create(req)
	respond(c, item, err)
}

// AdminGenerateCoupons 批量自动生成优惠码；入参的 code 字段被忽略。
func (h *Handler) AdminGenerateCoupons(c *gin.Context) {
	var req struct {
		service.CouponInput
		Count int `json:"count"`
	}
	if !bindJSON(c, &req) {
		return
	}
	items, err := h.coupons().GenerateCodes(req.CouponInput, req.Count)
	respond(c, gin.H{"items": items}, err)
}

// AdminUpdateCoupon 更新优惠码（code 不可改）。
func (h *Handler) AdminUpdateCoupon(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.CouponInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.coupons().Update(id, req)
	respond(c, item, err)
}

// AdminDeleteCoupon 删除优惠码；历史核销记录保留。
func (h *Handler) AdminDeleteCoupon(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.coupons().Delete(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// ---- 用户端：购物车优惠码试算与下单 ----

// CartCouponPreview 试算优惠码对当前购物车的减免（只读不核销）。
func (h *Handler) CartCouponPreview(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if !bindJSON(c, &req) {
		return
	}
	view, err := h.orders().CartCouponPreview(httpx.CurrentUserID(c), req.Code)
	respond(c, view, err)
}
