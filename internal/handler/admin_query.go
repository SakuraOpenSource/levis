package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// AdminOrders 分页返回全部订单，支持 user_id 与 status 过滤。
func (h *Handler) AdminOrders(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.admin().AdminOrders(httpx.QueryUint(c, "user_id"), c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminOrder 返回单个订单（含明细）。
func (h *Handler) AdminOrder(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.admin().AdminOrder(id)
	respond(c, item, err)
}

// AdminInvoices 分页返回全部账单，支持 user_id 与 status 过滤。
func (h *Handler) AdminInvoices(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.admin().AdminInvoices(httpx.QueryUint(c, "user_id"), c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminInvoice 返回单个账单（含明细与关联的外部支付）。
func (h *Handler) AdminInvoice(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.admin().AdminInvoice(id)
	respond(c, item, err)
}

// AdminServices 跨用户分页返回服务，支持 user_id、product_id 与 status 过滤。
func (h *Handler) AdminServices(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.admin().AdminServices(httpx.QueryUint(c, "user_id"), httpx.QueryUint(c, "product_id"), c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminRetryService 以管理员身份重试任意服务的上游开通。
func (h *Handler) AdminRetryService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.admin().AdminRetryProvision(id)
	respond(c, item, err)
}
