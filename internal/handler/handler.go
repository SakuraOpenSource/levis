package handler

import (
	"log"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/captcha"
	"github.com/SakuraOpenSource/levis/internal/notify"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/pluginhost"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
	"github.com/SakuraOpenSource/levis/internal/storage"
)

// Handler 聚合各业务 service，并通过 Runtime 获取数据库句柄。
//
// 数据库连接在安装完成后才存在，因此 service 不能在启动时一次性构造好；
// 这里按请求惰性构造 —— service 本身是无状态的薄封装，构造成本可忽略。
type Handler struct {
	rt      *runtime.Runtime
	install *service.InstallService
	// plugins 管理插件子进程。它持有进程与连接，整个进程内必须共用一份，
	// 不能像其它 service 那样按请求新建。可为 nil（测试里不需要插件时）。
	plugins *plugin.Manager
	// captchaStore 是唯一的例外：它持有已签发但未校验的验证码，必须在整个
	// 进程内共用一份，按请求新建会让上一次发出去的验证码立刻查无此码。
	captchaStore *captcha.Store
	// storage 只依赖数据目录，该值在进程生命周期内不变，因此可以在启动时
	// 就建好，不必像其它 service 那样等数据库。
	storage *storage.Store
	// notify 是异步通知投递器，持有队列与 worker，同样全进程共用一份。
	// 可为 nil —— nil 上调用任何方法都是空操作，调用点不必判空。
	notify *notify.Notifier
}

// New 构造 Handler。plugins 可为 nil，此时插件管理接口一律返回「未启用」，
// 通知邮件也不发（没有插件就没有发信能力）。
func New(rt *runtime.Runtime, plugins *plugin.Manager) *Handler {
	return &Handler{
		rt:           rt,
		install:      service.NewInstallService(rt),
		plugins:      plugins,
		captchaStore: captcha.NewStore(),
		storage:      storage.New(rt.DataDir()),
		notify:       pluginhost.NewNotifier(rt, plugins, log.Printf),
	}
}

// Close 释放 Handler 持有的后台资源。
func (h *Handler) Close() { h.notify.Close() }

func (h *Handler) db() *gorm.DB                { return h.rt.DB() }
func (h *Handler) users() *service.UserService { return service.NewUserService(h.db()) }

// agentProgram 构造代理加盟服务。
func (h *Handler) agentProgram() *service.AgentProgramService {
	return service.NewAgentProgramService(h.db())
}
func (h *Handler) cart() *service.CartService     { return service.NewCartService(h.db()) }
func (h *Handler) wallet() *service.WalletService { return service.NewWalletService(h.db()) }

func (h *Handler) settings() *service.SettingService {
	return service.NewSettingService(h.db())
}

func (h *Handler) captcha() *service.CaptchaService {
	return service.NewCaptchaService(h.db(), h.captchaStore)
}

func (h *Handler) catalog() *service.CatalogService {
	return service.NewCatalogService(h.db())
}

func (h *Handler) upstream() *service.UpstreamService {
	return service.NewUpstreamService(h.db(), h.plugins)
}

func (h *Handler) billing() *service.BillingService {
	return service.NewBillingService(h.db(), h.wallet(), h.plugins)
}

func (h *Handler) orders() *service.OrderService {
	return service.NewOrderService(h.db(), h.cart(), h.wallet(), h.plugins)
}

func (h *Handler) tickets() *service.TicketService {
	return service.NewTicketService(h.db(), h.storage)
}

func (h *Handler) kyc() *service.KYCService {
	return service.NewKYCService(h.db(), h.storage, h.plugins)
}

func (h *Handler) apiKeys() *service.APIKeyService {
	return service.NewAPIKeyService(h.db())
}

func (h *Handler) admin() *service.AdminService {
	return service.NewAdminService(h.db(), h.wallet(), h.storage, h.plugins)
}

func (h *Handler) payments() *service.PaymentService {
	return service.NewPaymentService(h.db(), h.plugins, h.wallet(), h.orders(), h.billing())
}

func (h *Handler) pluginSvc() *service.PluginService {
	return service.NewPluginService(h.db())
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
	log.Printf("handler internal error: %v", err)
	Internal(c, "服务器内部错误")
}
