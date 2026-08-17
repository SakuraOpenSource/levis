package server

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/handler"
	"github.com/SakuraOpenSource/levis/internal/middleware"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// mountOpenAPI 挂载 /api/open/v1 对外接口。
//
// 直接挂在 engine 上，不进 /api 组：那一组带着 CSRF 中间件，而 API Key 认证
// 里没有浏览器隐式凭证，双提交令牌无从获取，也起不到防护作用。
//
// 权限位到接口的映射遵循「写权限隐含其操作对象的读权限」：能下单的 Key
// 自然要能查商品，否则连商品 ID 都拿不到。
//
// 本轮未做限流，是有意的取舍：先把认证与权限边界立住。日后要加，这一组
// 就是唯一的落点。
func mountOpenAPI(engine *gin.Engine, rt *runtime.Runtime, h *handler.Handler) {
	open := engine.Group("/api/open/v1",
		middleware.RequireInstalled(rt), middleware.RequireAPIKey(rt))

	balance := open.Group("", middleware.RequireScope(model.ScopeBalanceRead))
	balance.GET("/account", h.OpenAccount)
	balance.GET("/transactions", h.OpenTransactions)

	orders := open.Group("", middleware.RequireScope(model.ScopeOrderWrite))
	orders.GET("/products", h.OpenProducts)
	orders.GET("/orders", h.OpenOrders)
	orders.POST("/orders", h.OpenCreateOrder)
	orders.GET("/orders/:id", h.OpenOrder)
	orders.POST("/orders/:id/pay", h.OpenPayOrder)

	services := open.Group("", middleware.RequireScope(model.ScopeServiceWrite))
	services.GET("/services", h.OpenServices)
	services.GET("/services/:id", h.OpenService)
	services.POST("/services/:id/renew", h.OpenRenewService)
}
