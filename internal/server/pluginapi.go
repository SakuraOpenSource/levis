package server

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/handler"
	"github.com/SakuraOpenSource/levis/internal/middleware"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// mountPluginAPI 挂载 /api/plugin/v1，供插件回调主程序。
//
// 与 /api/open/v1 同样直接挂在 engine 上、不进 /api 组：那一组带着 CSRF
// 中间件，而插件凭证认证里没有浏览器隐式凭证，双提交令牌既无从获取也无意义。
//
// 这一组比开放接口更敏感 —— wallet/credit 能给任意用户加任意金额。所以
// RequirePluginKey 完全不看 Cookie：带着管理员登录态但不带凭证的请求一律
// 401，杜绝「诱导管理员点开一个页面就能凭空造钱」这条路径。
func mountPluginAPI(engine *gin.Engine, rt *runtime.Runtime, h *handler.Handler) {
	group := engine.Group("/api/plugin/v1",
		middleware.RequireInstalled(rt), middleware.RequirePluginKey(rt))

	credit := group.Group("", middleware.RequirePluginScope(model.PluginScopeWalletCredit))
	credit.POST("/wallet/credit", h.PluginCredit)

	users := group.Group("", middleware.RequirePluginScope(model.PluginScopeUserRead))
	users.GET("/users/:id", h.PluginUser)

	orders := group.Group("", middleware.RequirePluginScope(model.PluginScopeOrderRead))
	orders.GET("/orders/:id", h.PluginOrder)

	// 支付渠道的异步回调通知：不需 CSRF、不需 PluginKey（由插件验签），
	// 但必须在已安装状态下才能处理。
	notify := engine.Group("/api/plugin/v1/payment-notify",
		middleware.RequireInstalled(rt))
	notify.Any("/:plugin", h.PaymentNotify)
}
