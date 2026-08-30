package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
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

// ProductOS 返回接口商品在购买时可选的系统镜像（按商品的驱动过滤）。
func (h *Handler) ProductOS(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	items, err := h.upstream().ProductOS(id)
	respond(c, gin.H{"items": items}, err)
}

// BuyNowRequest 是「立即购买」的入参：绕过购物车直接生成待支付订单。
// 接口商品的选配（规格与系统）必须随单提交。
type BuyNowRequest struct {
	service.OrderLine
}

// BuyNow 为当前用户创建一笔单明细的直购订单，返回后前端跳转支付。
func (h *Handler) BuyNow(c *gin.Context) {
	var req BuyNowRequest
	if !bindJSON(c, &req) {
		return
	}
	order, err := h.orders().CreateDirect(httpx.CurrentUserID(c), []service.OrderLine{req.OrderLine})
	respond(c, order, err)
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
	userID := httpx.CurrentUserID(c)
	result, err := h.orders().Pay(userID, id)
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 支付与开通都已提交，此刻才发凭据通知；发不出去不影响这笔已经完成的支付。
	h.notify.OrderPaid(userID, result.Order.OrderNo, result.Order.TotalCents)
	OK(c, result)
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

// AgentProgramSummary 返回当前用户的代理加盟信息（等级/下一档/生效折扣）。
func (h *Handler) AgentProgramSummary(c *gin.Context) {
	summary, err := h.agentProgram().Summary(httpx.CurrentUserID(c))
	respond(c, summary, err)
}

// AgentProgramApply 提交代理申请。
func (h *Handler) AgentProgramApply(c *gin.Context) {
	var in service.ApplyInput
	if !bindJSON(c, &in) {
		return
	}
	application, err := h.agentProgram().Apply(httpx.CurrentUserID(c), in)
	respond(c, application, err)
}

// AgentProgramApplications 返回代理申请列表（管理端）。
func (h *Handler) AgentProgramApplications(c *gin.Context) {
	items, err := h.agentProgram().Applications(c.Query("status"))
	respond(c, gin.H{"items": items}, err)
}

// AgentProgramReview 审核代理申请（管理端）。
func (h *Handler) AgentProgramReview(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var in service.ReviewInput
	if !bindJSON(c, &in) {
		return
	}
	application, err := h.agentProgram().Review(id, httpx.CurrentUserID(c), in)
	respond(c, application, err)
}

// AgentProgramTiers 返回可申请的代理等级（用户端申请表单用）。
func (h *Handler) AgentProgramTiers(c *gin.Context) {
	svc := h.agentProgram()
	if !svc.Enabled() {
		respond(c, gin.H{"items": []any{}}, nil)
		return
	}
	var tiers []model.AgentTier
	if err := svc.DB().Order("sort ASC, min_balance_cents ASC").Find(&tiers).Error; err != nil {
		respond(c, nil, err)
		return
	}
	respond(c, gin.H{"items": tiers}, nil)
}
