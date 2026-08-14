// Package server 组装 HTTP 路由。
package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/handler"
	"github.com/SakuraOpenSource/levis/internal/middleware"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/web"
)

// maxBodyBytes 限制请求体大小，避免超大 JSON 打满内存。
const maxBodyBytes = 1 << 20 // 1 MiB

// New 构造 gin 引擎，挂载 API 与前端静态资源。
func New(rt *runtime.Runtime, debug bool) *gin.Engine {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery(), limitBody(), securityHeaders())
	// 前端为 SPA，所有未命中的路径都交给它做客户端路由。
	engine.RedirectTrailingSlash = false

	h := handler.New(rt)

	// CSRF 挂在整个 /api 上：GET 请求负责播种令牌，写请求负责校验。
	// 前端启动时必然先调 GET /api/bootstrap，因此安装、登录、注册这些
	// 「第一次写操作」总能拿到可用的令牌。
	api := engine.Group("/api", middleware.CSRF())

	// 安装相关接口不能要求已安装 —— 未安装时正是要靠它们完成安装。
	api.GET("/bootstrap", h.Bootstrap)
	install := api.Group("/install")
	install.POST("/test-db", h.TestDatabase)
	install.POST("", h.Install)

	// 其余接口一律要求已安装。
	guarded := api.Group("", middleware.RequireInstalled(rt))

	// 验证码是公开接口：登录、注册页在尚无登录态时就要取图。
	// 但它要读站点配置，所以必须在 guarded 之下（未安装时无库可读）。
	guarded.GET("/captcha", h.Captcha)

	authGroup := guarded.Group("/auth")
	authGroup.POST("/register", h.Register)
	authGroup.POST("/login", h.Login)
	authGroup.POST("/logout", h.Logout)

	catalog := guarded.Group("/catalog")
	catalog.GET("/categories", h.Categories)
	catalog.GET("/products", h.Products)
	catalog.GET("/products/:id", h.Product)

	// 以下均需登录。
	authed := guarded.Group("", middleware.RequireAuth(rt))

	authed.GET("/me", h.Me)
	authed.PATCH("/me/email", h.UpdateEmail)
	authed.POST("/me/password", h.UpdatePassword)

	cart := authed.Group("/cart")
	cart.GET("/items", h.Cart)
	cart.POST("/items", h.AddToCart)
	cart.PATCH("/items/:id", h.UpdateCartItem)
	cart.DELETE("/items/:id", h.RemoveCartItem)

	orders := authed.Group("/orders")
	orders.GET("", h.Orders)
	orders.POST("", h.CreateOrder)
	orders.GET("/:id", h.Order)
	orders.POST("/:id/pay", h.PayOrder)
	orders.POST("/:id/cancel", h.CancelOrder)

	services := authed.Group("/services")
	services.GET("", h.Services)
	services.GET("/:id", h.Service)
	services.POST("/:id/renew", h.RenewService)

	wallet := authed.Group("/wallet")
	wallet.GET("", h.Wallet)
	wallet.GET("/transactions", h.Transactions)
	wallet.POST("/recharge", h.Recharge)

	invoices := authed.Group("/invoices")
	invoices.GET("", h.Invoices)
	invoices.GET("/:id", h.Invoice)

	// 以下均需管理员权限。
	admin := authed.Group("/admin", middleware.RequireAdmin())
	admin.GET("/stats", h.AdminStats)
	admin.GET("/users", h.AdminUsers)
	admin.POST("/users", h.AdminCreateUser)
	admin.PATCH("/users/:id", h.AdminUpdateUser)
	admin.DELETE("/users/:id", h.AdminDeleteUser)
	admin.GET("/categories", h.AdminCategories)
	admin.POST("/categories", h.AdminCreateCategory)
	admin.PATCH("/categories/:id", h.AdminUpdateCategory)
	admin.DELETE("/categories/:id", h.AdminDeleteCategory)
	admin.GET("/products", h.AdminProducts)
	admin.POST("/products", h.AdminCreateProduct)
	admin.PATCH("/products/:id", h.AdminUpdateProduct)
	admin.DELETE("/products/:id", h.AdminDeleteProduct)
	admin.GET("/users/:id/services", h.AdminUserServices)
	admin.PATCH("/services/:id", h.AdminUpdateService)
	admin.DELETE("/services/:id", h.AdminDeleteService)
	admin.GET("/settings/captcha", h.AdminCaptchaSettings)
	admin.PUT("/settings/captcha", h.AdminUpdateCaptchaSettings)

	// 未匹配的 API 路径返回 JSON 404；其余交给前端。
	frontend := gin.WrapF(web.Handler())
	engine.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/api" {
			handler.NotFound(c, "接口不存在")
			return
		}
		frontend(c)
	})

	return engine
}

// limitBody 限制请求体大小。
func limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	}
}

// securityHeaders 添加基础安全响应头。
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	}
}
