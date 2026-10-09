package handler

import (
	"log"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/captcha"
	"github.com/SakuraOpenSource/levis/internal/loginlimit"
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
	// captchaStore 是验证码存储。签发与校验分属两次请求，必须在整个进程
	// 内共用一份；真实实现是 *captcha.Store，测试注入假存储以构造「答对」
	// 的路径（真实存储的答案只留在服务端，测试读不到）。
	captchaStore service.CaptchaStore
	// storage 只依赖数据目录，该值在进程生命周期内不变，因此可以在启动时
	// 就建好，不必像其它 service 那样等数据库。
	storage *storage.Store
	// notify 是异步通知投递器，持有队列与 worker，同样全进程共用一份。
	// 可为 nil —— nil 上调用任何方法都是空操作，调用点不必判空。
	notify *notify.Notifier
	// loginTracker 统计登录失败并临时锁定，全进程共用一份（见 loginlimit
	// 包的说明）。Close 时停掉它后台的清扫协程。
	loginTracker *loginlimit.Tracker
	// revoker 是登出吊销表，全进程共用一份。持久实现（数据库落地，见
	// auth.PersistentRevocationList）是默认：吊销在重启/多副本下仍然有效
	// （P-AUTH-4）；未安装（尚无数据库）时退回进程内实现兜底。
	revoker auth.SessionRevoker
	// emailSvc 持有邮箱验证码与限流状态，同样全进程共用一份。数据库在
	// 安装完成后才存在，因此首次使用时（必然已安装）惰性构造。
	emailMu   sync.Mutex
	emailSvc  *service.EmailService
	emailInit bool
}

// New 构造 Handler。plugins 可为 nil，此时插件管理接口一律返回「未启用」，
// 通知邮件也不发（没有插件就没有发信能力）。
func New(rt *runtime.Runtime, plugins *plugin.Manager) *Handler {
	return newHandler(rt, plugins, captcha.NewStore())
}

// NewWithCaptchaStore 用指定的验证码存储构造 Handler。
//
// 生产代码一律用 New；测试用它注入假存储 —— 真实存储只把答案留在服务端，
// 测试拿不到答案，也就无法构造「答对」这条路径（管理员入口强制验证码，
// 没有它接口测试根本登录不进管理员）。
func NewWithCaptchaStore(rt *runtime.Runtime, plugins *plugin.Manager, store service.CaptchaStore) *Handler {
	return newHandler(rt, plugins, store)
}

// newHandler 是两个公开构造函数的公共实现。
func newHandler(rt *runtime.Runtime, plugins *plugin.Manager, store service.CaptchaStore) *Handler {
	return &Handler{
		rt:           rt,
		install:      service.NewInstallService(rt),
		plugins:      plugins,
		captchaStore: store,
		storage:      storage.New(rt.DataDir()),
		notify:       pluginhost.NewNotifier(rt, plugins, log.Printf),
		loginTracker: loginlimit.New(),
		// P-AUTH-4: prefer the durable revocation store once the database
		// exists; before installation (no DB yet) the process-local list is
		// the best we can do, and the runtime-backed revoker upgrades itself
		// as soon as a DB handle appears.
		revoker: newRuntimeRevoker(rt),
	}
}

// Close 释放 Handler 持有的后台资源。
func (h *Handler) Close() {
	h.notify.Close()
	h.loginTracker.Close()
	h.revoker.Close()
}

// Revoker 暴露登出吊销表，供路由层装配 RequireAuth 中间件（同一进程必须
// 共用 Handler 里的这一份，否则登出写不进中间件查的那张表）。
func (h *Handler) Revoker() auth.SessionRevoker { return h.revoker }

func (h *Handler) db() *gorm.DB                { return h.rt.DB() }
func (h *Handler) users() *service.UserService { return service.NewUserService(h.db()) }

// agentProgram 构造代理加盟服务。
func (h *Handler) agentProgram() *service.AgentProgramService {
	return service.NewAgentProgramService(h.db())
}
func (h *Handler) cart() *service.CartService     { return service.NewCartService(h.db()) }
func (h *Handler) wallet() *service.WalletService { return service.NewWalletService(h.db()) }
func (h *Handler) email() *service.EmailService {
	h.emailMu.Lock()
	defer h.emailMu.Unlock()
	if !h.emailInit {
		if db := h.db(); db != nil {
			h.emailSvc = service.NewEmailService(db)
			h.emailInit = true
		}
	}
	if h.emailSvc == nil {
		return service.NewEmailService(h.db())
	}
	return h.emailSvc
}

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
func (h *Handler) articles() *service.ArticleService {
	return service.NewArticleService(h.db())
}
func (h *Handler) coupons() *service.CouponService {
	return service.NewCouponService(h.db())
}

// respond 把 service 层错误映射为 HTTP 响应；err 为 nil 时返回 data。
//
// 可预期的业务错误携带状态码与错误码，直接透出；gRPC 状态错误按类别映射
// （PLG-F06）：插件边界的 typed 失败必须以真实的语义状态到达客户端，
// 统统 500 会把「旧插件不支持 / 参数错 / 资源没了 / 超时」全部伪装成
// 服务器故障，浏览器与监控都无法区分；其余错误视为内部错误，只回一句
// 通用提示，不外泄实现细节。
func respond(c *gin.Context, data any, err error) {
	if err == nil {
		OK(c, data)
		return
	}
	if bizErr, ok := service.AsError(err); ok {
		Fail(c, bizErr.Status, bizErr.Code, bizErr.Message)
		return
	}
	if code := grpcStatusToHTTP(err); code != 0 {
		Fail(c, code, grpcHTTPCode(code), grpcMessage(err))
		return
	}
	log.Printf("handler internal error: %v", err)
	Internal(c, "服务器内部错误")
}

// grpcStatusToHTTP maps a gRPC status carried by err to an HTTP status, 0 when
// err carries no gRPC status. Kept in the handler layer: the service layer
// speaks business errors, the wire translation belongs here.
func grpcStatusToHTTP(err error) int {
	code := status.Code(err)
	switch code {
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		// codes.OK only appears for nil errors; anything else is an internal
		// failure we do not want to fingerprint to clients.
		if code == codes.OK {
			return 0
		}
		return http.StatusInternalServerError
	}
}

// grpcHTTPCode gives each mapped failure a distinct machine-readable code so
// frontends can branch without parsing localized messages.
func grpcHTTPCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusGatewayTimeout:
		return "UPSTREAM_TIMEOUT"
	case http.StatusNotImplemented:
		return "FEATURE_UNSUPPORTED"
	case http.StatusServiceUnavailable:
		return "UPSTREAM_UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}

// grpcMessage surfaces the gRPC error message for the mapped categories: these
// originate from our own plugin protocol, not from arbitrary internals.
func grpcMessage(err error) string {
	msg := status.Convert(err).Message()
	if msg == "" {
		return "上游操作失败"
	}
	return msg
}
