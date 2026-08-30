// Package server 组装 HTTP 路由。
package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/handler"
	"github.com/SakuraOpenSource/levis/internal/middleware"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/web"
)

// 请求体大小上限，避免超大请求打满内存。
const (
	maxBodyBytes = 1 << 20 // 1 MiB，绝大多数接口收发的都是小 JSON
	// maxUploadBytes 给上传接口留出 20 MiB 附件 + 1 MiB multipart 信封。
	maxUploadBytes = 21 << 20
)

// maxMultipartMemory 是 multipart 表单驻留内存的上限，超出部分落临时文件。
//
// gin 默认 32 MiB 全留在内存，几个并发上传就能把内存打满。
const maxMultipartMemory = 8 << 20

// uploadRoutes 是允许大请求体的路由，键为 gin 的路由模板。
//
// gin 在执行中间件链之前已完成路由匹配，因此全局中间件里 c.FullPath() 可用。
// 在同一处按路由抬高上限，两个上限值就不会散落两地。
var uploadRoutes = map[string]bool{
	"/api/tickets":                   true,
	"/api/tickets/:id/replies":       true,
	"/api/admin/tickets/:id/replies": true,
	"/api/kyc":                       true,
	"/api/admin/plugins/install":     true,
}

// New 构造 gin 引擎，挂载 API 与前端静态资源。
//
// plugins 可为 nil，此时插件管理接口一律返回 503；测试里不需要插件时就这么传。
//
// 返回的 close 释放 Handler 持有的后台资源（当前是通知队列的 worker），调用方
// 必须在服务停止后执行它。engine 本身没有需要释放的东西，是 Handler 有。
func New(rt *runtime.Runtime, plugins *plugin.Manager, debug bool) (*gin.Engine, func()) {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery(), limitBody(), securityHeaders())
	// 前端为 SPA，所有未命中的路径都交给它做客户端路由。
	engine.RedirectTrailingSlash = false
	engine.MaxMultipartMemory = maxMultipartMemory

	h := handler.New(rt, plugins)

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
	catalog.GET("/products/:id/os", h.ProductOS)

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
	orders.POST("/direct", h.BuyNow)
	orders.GET("/:id", h.Order)
	orders.POST("/:id/pay", h.PayOrder)
	orders.POST("/:id/cancel", h.CancelOrder)

	services := authed.Group("/services")
	services.GET("", h.Services)
	services.GET("/:id", h.Service)
	services.POST("/:id/renew", h.RenewService)
	services.POST("/:id/power", h.ServicePower)
	services.GET("/:id/upstream", h.ServiceUpstream)
	services.GET("/:id/os", h.ServiceOS)

	payments := authed.Group("/payments")
	payments.GET("/methods", h.PaymentMethods)
	payments.POST("", h.CreatePayment)
	payments.GET("/:id", h.Payment)
	payments.POST("/:id/query", h.QueryPayment)

	wallet := authed.Group("/wallet")
	wallet.GET("", h.Wallet)
	wallet.GET("/transactions", h.Transactions)
	wallet.POST("/recharge", h.Recharge)

	invoices := authed.Group("/invoices")
	invoices.GET("", h.Invoices)
	invoices.GET("/:id", h.Invoice)

	tickets := authed.Group("/tickets")
	tickets.GET("", h.Tickets)
	tickets.POST("", h.CreateTicket)
	tickets.GET("/:id", h.Ticket)
	tickets.POST("/:id/replies", h.ReplyTicket)
	tickets.POST("/:id/close", h.CloseTicket)
	tickets.GET("/:id/attachments/:aid", h.TicketAttachment)

	kyc := authed.Group("/kyc")
	kyc.GET("", h.Verification)
	kyc.POST("", h.SubmitVerification)
	kyc.GET("/photo/:side", h.VerificationPhoto)
	// 第三方实名认证：POST 发起认证拿到跳转地址，GET 轮询认证结果。
	kyc.POST("/external", h.StartExternalVerification)
	kyc.GET("/external", h.QueryExternalVerification)

	apiKeys := authed.Group("/api-keys")
	apiKeys.GET("", h.APIKeys)
	apiKeys.POST("", h.CreateAPIKey)
	apiKeys.DELETE("/:id", h.RevokeAPIKey)

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
	admin.GET("/provision-plugins", h.AdminProvisionPlugins)
	admin.GET("/upstream-products", h.AdminUpstreamProducts)
	admin.POST("/products/:id/sync-info", h.AdminSyncProductInfo)
	admin.GET("/interfaces", h.AdminInterfaces)
	admin.POST("/interfaces", h.AdminCreateInterface)
	admin.PATCH("/interfaces/:id", h.AdminUpdateInterface)
	admin.POST("/interfaces/:id/test", h.AdminTestInterface)
	admin.DELETE("/interfaces/:id", h.AdminDeleteInterface)
	admin.GET("/users/:id/services", h.AdminUserServices)
	admin.POST("/users/:id/services", h.AdminCreateService)
	admin.PATCH("/services/:id", h.AdminUpdateService)
	admin.POST("/services/:id/bind", h.AdminBindService)
	admin.DELETE("/services/:id", h.AdminDeleteService)
	admin.GET("/payment-plugins", h.AdminPaymentPlugins)
	admin.GET("/payment-methods", h.AdminPaymentMethods)
	admin.POST("/payment-methods", h.AdminCreatePaymentMethod)
	admin.PATCH("/payment-methods/:id", h.AdminUpdatePaymentMethod)
	admin.DELETE("/payment-methods/:id", h.AdminDeletePaymentMethod)
	admin.GET("/settings/captcha", h.AdminCaptchaSettings)
	admin.PUT("/settings/captcha", h.AdminUpdateCaptchaSettings)
	admin.GET("/settings/kyc", h.AdminKYCSettings)

	// 代理加盟：管理端整体读写，用户端只读摘要。
	admin.GET("/agent-program", h.AgentProgram)
	admin.PUT("/agent-program", h.UpdateAgentProgram)
	authed.GET("/agent-program/summary", h.AgentProgramSummary)
	authed.POST("/agent-program/apply", h.AgentProgramApply)
	authed.GET("/agent-program/tiers", h.AgentProgramTiers)

	// 代理申请审核。
	admin.GET("/agent-program/applications", h.AgentProgramApplications)
	admin.POST("/agent-program/applications/:id/review", h.AgentProgramReview)
	admin.PUT("/settings/kyc", h.AdminUpdateKYCSettings)
	admin.GET("/tickets", h.AdminTickets)
	admin.GET("/tickets/:id", h.AdminTicket)
	admin.POST("/tickets/:id/replies", h.AdminReplyTicket)
	admin.POST("/tickets/:id/close", h.AdminCloseTicket)
	admin.POST("/tickets/:id/reopen", h.AdminReopenTicket)
	admin.GET("/tickets/:id/attachments/:aid", h.AdminTicketAttachment)
	admin.GET("/plugins", h.AdminPlugins)
	admin.POST("/plugins/install", h.AdminInstallPlugin)
	admin.POST("/plugins/reload", h.AdminReloadPlugins)
	admin.GET("/plugins/:id/frontend/*path", h.AdminPluginFrontend)
	admin.GET("/plugins/:id/frontend-config", h.AdminFrontendPluginConfig)
	admin.PUT("/plugins/:id/frontend-config", h.AdminUpdateFrontendPluginConfig)
	admin.GET("/plugins/:id", h.AdminPlugin)
	admin.PUT("/plugins/:id/config", h.AdminUpdatePluginConfig)
	admin.POST("/plugins/:id/enable", h.AdminEnablePlugin)
	admin.POST("/plugins/:id/disable", h.AdminDisablePlugin)
	admin.GET("/plugins/:id/logs", h.AdminPluginLogs)
	admin.GET("/verifications", h.AdminVerifications)
	admin.GET("/verifications/:id", h.AdminVerification)
	admin.GET("/verifications/:id/photo/:side", h.AdminVerificationPhoto)
	admin.POST("/verifications/:id/approve", h.AdminApproveVerification)
	admin.POST("/verifications/:id/reject", h.AdminRejectVerification)

	// 对外 API 挂在 engine 而不是 api 组下：api 组带着 CSRF 中间件，而 Key
	// 认证不存在浏览器隐式凭证，双提交令牌在这里既无从获取也无意义。
	mountOpenAPI(engine, rt, h)
	// 插件回调同理，且更敏感 —— 详见 pluginapi.go 的说明。
	mountPluginAPI(engine, rt, h)

	// 未匹配的 API 路径返回 JSON 404；其余交给前端。
	frontend := gin.WrapF(web.Handler())
	engine.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") || c.Request.URL.Path == "/api" {
			handler.NotFound(c, "接口不存在")
			return
		}
		frontend(c)
	})

	return engine, h.Close
}

// limitBody 限制请求体大小，上传接口按 uploadRoutes 放宽。
func limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := int64(maxBodyBytes)
		if uploadRoutes[c.FullPath()] {
			limit = maxUploadBytes
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
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
