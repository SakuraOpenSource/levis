package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// 本文件是 /api/open/v1 的接口层。
//
// 它刻意只做「取参数、调既有 service、返回」这点事：资金相关的动作
// （支付、续费）直接复用浏览器端走的同一批 service，全都经由
// WalletService.adjustBalance 变动余额 —— 不为机器调用另开一条资金路径。

// OpenAccount 返回账号概要与余额。
func (h *Handler) OpenAccount(c *gin.Context) {
	user := httpx.CurrentUser(c)
	overview, err := h.wallet().Overview(user.ID)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
		"wallet":   overview,
	})
}

// OpenTransactions 分页返回余额流水。
func (h *Handler) OpenTransactions(c *gin.Context) {
	h.Transactions(c)
}

// OpenProducts 返回上架商品，供调用方查商品 ID 与价格。
func (h *Handler) OpenProducts(c *gin.Context) {
	h.Products(c)
}

// OpenCreateOrderRequest 是直接下单的入参。
type OpenCreateOrderRequest struct {
	Items []service.OrderLine `json:"items"`
}

// OpenCreateOrder 按明细直接创建订单，不经过购物车。
//
// 不走购物车是刻意的：购物车属于用户的浏览器会话，API 下单若共用它，
// 就会把用户正在挑选的东西一并结掉。
func (h *Handler) OpenCreateOrder(c *gin.Context) {
	var req OpenCreateOrderRequest
	if !bindJSON(c, &req) {
		return
	}
	order, err := h.orders().CreateDirect(httpx.CurrentUserID(c), req.Items)
	respond(c, order, err)
}

// OpenOrders 分页返回订单。
func (h *Handler) OpenOrders(c *gin.Context) {
	h.Orders(c)
}

// OpenOrder 返回单个订单。
func (h *Handler) OpenOrder(c *gin.Context) {
	h.Order(c)
}

// OpenPayOrder 用余额支付订单。
func (h *Handler) OpenPayOrder(c *gin.Context) {
	h.PayOrder(c)
}

// OpenServices 分页返回已购服务。
func (h *Handler) OpenServices(c *gin.Context) {
	h.Services(c)
}

// OpenService 返回单个已购服务。
func (h *Handler) OpenService(c *gin.Context) {
	h.Service(c)
}

// OpenRenewService 为已购服务续费一个周期。
func (h *Handler) OpenRenewService(c *gin.Context) {
	h.RenewService(c)
}
