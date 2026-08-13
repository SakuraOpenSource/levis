package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Categories 返回商品分组树（含每个分组下的上架商品）。
func (h *Handler) Categories(c *gin.Context) {
	items, err := h.catalog().Tree()
	respond(c, gin.H{"items": items}, err)
}

// Products 返回上架商品，可按 category_id 过滤。
func (h *Handler) Products(c *gin.Context) {
	categoryID, _ := strconv.ParseUint(c.Query("category_id"), 10, 64)
	items, err := h.catalog().Products(uint(categoryID))
	respond(c, gin.H{"items": items}, err)
}

// Product 返回单个上架商品。
func (h *Handler) Product(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.catalog().Product(id)
	respond(c, item, err)
}

// Cart 返回当前用户购物车。
func (h *Handler) Cart(c *gin.Context) {
	view, err := h.cart().List(httpx.CurrentUserID(c))
	respond(c, view, err)
}

// AddToCart 加入购物车。
func (h *Handler) AddToCart(c *gin.Context) {
	var req service.AddRequest
	if !bindJSON(c, &req) {
		return
	}
	userID := httpx.CurrentUserID(c)
	if err := h.cart().Add(userID, req); err != nil {
		respond(c, nil, err)
		return
	}
	view, err := h.cart().List(userID)
	respond(c, view, err)
}

// UpdateCartItemRequest 是修改购物车数量的入参。
type UpdateCartItemRequest struct {
	Quantity int `json:"quantity"`
}

// UpdateCartItem 修改购物车条目数量。
func (h *Handler) UpdateCartItem(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req UpdateCartItemRequest
	if !bindJSON(c, &req) {
		return
	}
	userID := httpx.CurrentUserID(c)
	if err := h.cart().UpdateQuantity(userID, id, req.Quantity); err != nil {
		respond(c, nil, err)
		return
	}
	view, err := h.cart().List(userID)
	respond(c, view, err)
}

// RemoveCartItem 删除购物车条目。
func (h *Handler) RemoveCartItem(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	userID := httpx.CurrentUserID(c)
	if err := h.cart().Remove(userID, id); err != nil {
		respond(c, nil, err)
		return
	}
	view, err := h.cart().List(userID)
	respond(c, view, err)
}

// CreateOrder 用当前购物车创建待支付订单。
func (h *Handler) CreateOrder(c *gin.Context) {
	order, err := h.orders().CreateFromCart(httpx.CurrentUserID(c))
	respond(c, order, err)
}

// Orders 分页返回当前用户订单。
func (h *Handler) Orders(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.orders().List(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// Order 返回当前用户的单个订单。
func (h *Handler) Order(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	order, err := h.orders().Get(httpx.CurrentUserID(c), id)
	respond(c, order, err)
}

// PayOrder 用余额支付订单（当前为假支付）。
func (h *Handler) PayOrder(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	result, err := h.orders().Pay(httpx.CurrentUserID(c), id)
	respond(c, result, err)
}

// CancelOrder 取消待支付订单。
func (h *Handler) CancelOrder(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.orders().Cancel(httpx.CurrentUserID(c), id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}
