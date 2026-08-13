package handler

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Handler 聚合各业务 service，并通过 Runtime 获取数据库句柄。
//
// 数据库连接在安装完成后才存在，因此 service 不能在启动时一次性构造好；
// 这里按请求惰性构造 —— service 本身是无状态的薄封装，构造成本可忽略。
type Handler struct {
	rt      *runtime.Runtime
	install *service.InstallService
}

// New 构造 Handler。
func New(rt *runtime.Runtime) *Handler {
	return &Handler{rt: rt, install: service.NewInstallService(rt)}
}

func (h *Handler) db() *gorm.DB                   { return h.rt.DB() }
func (h *Handler) users() *service.UserService    { return service.NewUserService(h.db()) }
func (h *Handler) cart() *service.CartService     { return service.NewCartService(h.db()) }
func (h *Handler) wallet() *service.WalletService { return service.NewWalletService(h.db()) }

func (h *Handler) catalog() *service.CatalogService {
	return service.NewCatalogService(h.db())
}

func (h *Handler) billing() *service.BillingService {
	return service.NewBillingService(h.db())
}

func (h *Handler) orders() *service.OrderService {
	return service.NewOrderService(h.db(), h.cart(), h.wallet())
}

func (h *Handler) admin() *service.AdminService {
	return service.NewAdminService(h.db(), h.wallet())
}

// respond 把 service 层错误映射为 HTTP 响应；err 为 nil 时返回 data。
//
// 可预期的业务错误携带状态码与错误码，直接透出；其余错误视为内部错误，
// 只回一句通用提示，不外泄实现细节。
func respond(c *gin.Context, data any, err error) {
	if err == nil {
		OK(c, data)
		return
	}
	if bizErr, ok := service.AsError(err); ok {
		Fail(c, bizErr.Status, bizErr.Code, bizErr.Message)
		return
	}
	Internal(c, "")
}
