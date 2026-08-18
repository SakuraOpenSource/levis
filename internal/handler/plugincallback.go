package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/middleware"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// 本文件是 /api/plugin/v1 的接口层，供插件回调主程序。
//
// 这组接口以系统身份操作任意用户的资源，因此每个 handler 都要显式从凭证里
// 取插件 ID —— 绝不接受请求体里自称的插件身份，那等于让插件冒充别人写入。

// PluginCreditRequest 是插件报告到账的入参。
type PluginCreditRequest struct {
	// ExternalID 由主程序在 CreatePayment 时下发，插件原样带回，作为幂等键。
	ExternalID  string `json:"external_id"`
	UserID      uint   `json:"user_id"`
	AmountCents int64  `json:"amount_cents"`
	GatewayRef  string `json:"gateway_ref"`
}

// PluginCredit 按插件报告的到账给用户加余额。
//
// 幂等：同一 (插件, external_id) 重复调用只加一次钱，重复请求返回 200 与首次
// 的结果。渠道的重试是常态，回 409 会让它一直重试到人工介入。
func (h *Handler) PluginCredit(c *gin.Context) {
	pluginID := middleware.CurrentPluginID(c)
	if pluginID == "" {
		// 中间件保证走不到这里；真到了说明路由挂错了组，宁可 401 也不要写库。
		httpx.Unauthorized(c, "请在 Authorization 头中提供插件凭证")
		return
	}
	var req PluginCreditRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.UserID == 0 {
		BadRequest(c, "请提供 user_id")
		return
	}
	record, err := h.wallet().CreditExternal(
		pluginID, req.ExternalID, req.UserID, req.AmountCents, req.GatewayRef)
	respond(c, record, err)
}

// PluginUser 返回用户资料，供插件发信时取用户名与邮箱。
func (h *Handler) PluginUser(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	user, err := h.pluginCallback().User(id)
	respond(c, user, err)
}

// PluginOrder 返回订单详情，供插件确认「付的是什么」。
func (h *Handler) PluginOrder(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	order, err := h.pluginCallback().Order(id)
	respond(c, order, err)
}

// pluginCallback 构造插件回调 service。
func (h *Handler) pluginCallback() *service.PluginCallbackService {
	return service.NewPluginCallbackService(h.db())
}
