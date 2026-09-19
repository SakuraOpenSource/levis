package handler

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Bootstrap 返回安装状态与站点信息。前端启动时首先调用此接口。
func (h *Handler) Bootstrap(c *gin.Context) {
	OK(c, h.install.Bootstrap())
}

// TestDatabase 测试数据库连接参数是否可用。
func (h *Handler) TestDatabase(c *gin.Context) {
	if h.rt.Installed() {
		Conflict(c, "程序已完成安装")
		return
	}
	var req config.Database
	if !bindJSON(c, &req) {
		return
	}
	if err := h.install.TestDatabase(req); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// Install 执行安装。
func (h *Handler) Install(c *gin.Context) {
	var req service.InstallRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.install.Install(req); err != nil {
		respond(c, nil, err)
		return
	}

	// 安装完成后直接把管理员登录态写下来，省掉一次手动登录。
	user, err := h.users().Login(req.AdminUsername, req.AdminPassword)
	if err != nil {
		OK(c, gin.H{"ok": true})
		return
	}
	if err := h.issueAdminSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"ok": true, "user": user})
}

// CaptchaFields 是带验证码的表单公共入参。
//
// 单独拎出来而不是塞进 service 层的请求结构体：验证码是接口层的准入检查，
// 与「注册一个用户」这件事本身无关，service 不该知道它的存在。
type CaptchaFields struct {
	CaptchaID   string `json:"captcha_id"`
	CaptchaCode string `json:"captcha_code"`
}

// RegisterRequest 是注册入参。
type RegisterRequest struct {
	service.RegisterRequest
	CaptchaFields
	// EmailCode 是邮箱验证码；仅在站点开启注册邮箱验证码时必填。
	EmailCode string `json:"email_code"`
}

// Register 注册普通用户。
func (h *Handler) Register(c *gin.Context) {
	// 用独立的请求结构体接收，绝不直接绑定到 model.User，
	// 否则客户端可传 role=admin 自我提权。
	var req RegisterRequest
	if !bindJSON(c, &req) {
		return
	}
	// 验证码必须先于建号校验，否则脚本可以完全无视验证码批量刷号。
	if err := h.captcha().Verify(service.CaptchaSceneRegister, req.CaptchaID, req.CaptchaCode); err != nil {
		respond(c, nil, err)
		return
	}
	// 站点开启注册邮箱验证码时，先验码再建号。
	if h.email().EmailCodeEnabled(service.EmailSceneRegister) {
		if err := h.email().VerifyEmailCode(service.EmailSceneRegister, req.RegisterRequest.Email, req.EmailCode); err != nil {
			respond(c, nil, err)
			return
		}
	}
	user, err := h.users().Register(req.RegisterRequest)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if err := h.issueSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"user": user})
}

// LoginRequest 是登录入参。identifier 可以是用户名或邮箱。
type LoginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
	CaptchaFields
}

// 登录限速的入口 scope：账号维度的失败计数按入口隔离（见 loginlimit 包），
// 普通入口上的爆破不会把管理员锁在管理员入口外，反之亦然。
const (
	loginScopeUser  = "login"
	loginScopeAdmin = "admin"
)

// Login 登录普通用户。
//
// 管理员凭证在此入口被显式拒绝（前端收到 ADMIN_ENTRY_REQUIRED 后引导跳转
// 管理员专用入口）。两类账号共用一个入口曾让管理员的爆破面与普通用户完全
// 重合；拆开后管理员入口得以叠加更强的防护（强制验证码、独立限速、更短的
// 会话有效期）而不拖累普通用户的体验。
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	ip := c.ClientIP()
	// 限速先于验证码与密码：被锁的请求在这里结束，不消耗 bcrypt 的算力。
	if !h.allowLogin(c, loginScopeUser, req.Identifier, ip) {
		return
	}
	// 先验验证码再验密码，否则接口仍可被直接拿来撞库。
	if err := h.captcha().Verify(service.CaptchaSceneLogin, req.CaptchaID, req.CaptchaCode); err != nil {
		respond(c, nil, err)
		return
	}
	user, err := h.users().Login(req.Identifier, req.Password)
	if err != nil {
		h.loginTracker.RecordFailure(loginScopeUser, req.Identifier, ip)
		respond(c, nil, err)
		return
	}
	if user.IsAdmin() {
		// 管理员凭证出现在普通入口按失败计数：不这样做，攻击者可以把
		// 这里当作「密码是否正确」的免费预言机无限试探。
		h.loginTracker.RecordFailure(loginScopeUser, req.Identifier, ip)
		Fail(c, http.StatusForbidden, httpx.CodeAdminEntryRequired, "管理员请使用专用入口登录")
		return
	}
	h.loginTracker.RecordSuccess(loginScopeUser, req.Identifier)
	// 密码正确但站点开启了登录邮箱验证码：不签发会话，走票据二次校验。
	if h.loginEmailChallenge(c, user.ID) {
		return
	}
	if err := h.issueSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"user": user})
}

// AdminLogin 是管理员专用登录入口（POST /api/admin/login）。
//
// 与普通入口的差异：
//   - 验证码强制校验（VerifyForced，不看站点开关）；
//   - 限速按独立的 scope 计数，阈值相同；
//   - 会话有效期更短（auth.AdminTokenTTL）。
func (h *Handler) AdminLogin(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	ip := c.ClientIP()
	if !h.allowLogin(c, loginScopeAdmin, req.Identifier, ip) {
		return
	}
	if err := h.captcha().VerifyForced(req.CaptchaID, req.CaptchaCode); err != nil {
		respond(c, nil, err)
		return
	}
	user, err := h.users().Login(req.Identifier, req.Password)
	if err != nil {
		h.loginTracker.RecordFailure(loginScopeAdmin, req.Identifier, ip)
		respond(c, nil, err)
		return
	}
	if !user.IsAdmin() {
		// 普通用户凭证出现在管理员入口同样按失败计数 —— 不给「密码
		// 是否正确」提供无限次的免费试探。
		h.loginTracker.RecordFailure(loginScopeAdmin, req.Identifier, ip)
		Forbidden(c, "需要管理员权限")
		return
	}
	h.loginTracker.RecordSuccess(loginScopeAdmin, req.Identifier)
	if err := h.issueAdminSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"user": user})
}

// allowLogin 检查登录限速；被拒时写回 429 与 Retry-After，返回 false。
//
// 提示不区分账号锁定还是来源锁定，避免向探测方暴露具体维度。
func (h *Handler) allowLogin(c *gin.Context, scope, identifier, ip string) bool {
	if ok, wait := h.loginTracker.Check(scope, identifier, ip); !ok {
		minutes := int(math.Ceil(wait.Minutes()))
		if minutes < 1 {
			minutes = 1
		}
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		httpx.Fail(c, http.StatusTooManyRequests, httpx.CodeTooManyRequests,
			fmt.Sprintf("尝试过于频繁，请约 %d 分钟后再试", minutes))
		return false
	}
	return true
}

// Logout 登出：吊销当前 token 并清除登录态 cookie。
//
// 只删 cookie 不够 —— token 本身在到期前依旧有效，拷贝走的人还能继续用。
// 这里把它按 jti 记入吊销表，RequireAuth 会在剩余有效期内持续拒绝它。
func (h *Handler) Logout(c *gin.Context) {
	if token, err := c.Cookie(auth.CookieToken); err == nil && token != "" {
		if claims, err := auth.ParseToken(h.rt.JWTSecret(), token); err == nil && claims.ExpiresAt != nil {
			h.revoker.Revoke(claims.ID, claims.ExpiresAt.Time)
		}
	}
	h.clearCookie(c, auth.CookieToken, true)
	h.clearCookie(c, auth.CookieCSRF, false)
	noContent(c)
}

// Me 返回当前登录用户。
func (h *Handler) Me(c *gin.Context) {
	OK(c, gin.H{"user": httpx.CurrentUser(c)})
}

// UpdateEmailRequest 是修改邮箱的入参。
type UpdateEmailRequest struct {
	Password string `json:"password"`
	Email    string `json:"email"`
}

// UpdateEmail 修改当前用户邮箱。
func (h *Handler) UpdateEmail(c *gin.Context) {
	var req UpdateEmailRequest
	if !bindJSON(c, &req) {
		return
	}
	err := h.users().ChangeEmail(httpx.CurrentUserID(c), req.Password, req.Email)
	if err != nil {
		respond(c, nil, err)
		return
	}
	user, err := h.users().Get(httpx.CurrentUserID(c))
	respond(c, gin.H{"user": user}, err)
}

// UpdatePasswordRequest 是修改密码的入参。
type UpdatePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// UpdatePassword 修改当前用户密码，成功后重新签发凭证。
func (h *Handler) UpdatePassword(c *gin.Context) {
	var req UpdatePasswordRequest
	if !bindJSON(c, &req) {
		return
	}
	userID := httpx.CurrentUserID(c)
	if err := h.users().ChangePassword(userID, req.OldPassword, req.NewPassword); err != nil {
		respond(c, nil, err)
		return
	}
	// 改密后重签 token，让旧凭证的剩余有效期不再关联新密码状态。
	user, err := h.users().Get(userID)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if err := h.issueSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// issueSession 签发普通用户会话（JWT + CSRF cookie），有效期 auth.TokenTTL。
func (h *Handler) issueSession(c *gin.Context, user *model.User) error {
	return h.issueSessionFor(c, user, auth.TokenTTL)
}

// issueAdminSession 签发管理员会话，有效期 auth.AdminTokenTTL（更短）。
func (h *Handler) issueAdminSession(c *gin.Context, user *model.User) error {
	return h.issueSessionFor(c, user, auth.AdminTokenTTL)
}

// issueSessionFor 签发指定有效期的会话并写入 cookie。
func (h *Handler) issueSessionFor(c *gin.Context, user *model.User, ttl time.Duration) error {
	token, _, err := auth.GenerateTokenWithTTL(h.rt.JWTSecret(), user.ID, user.Role, ttl)
	if err != nil {
		return err
	}
	csrf, err := auth.GenerateCSRFToken()
	if err != nil {
		return err
	}
	maxAge := int(ttl.Seconds())
	secure := h.secureCookie(c)

	// token 是 httpOnly：前端 JS 读不到，XSS 无法直接取走凭证。
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.CookieToken, token, maxAge, "/", "", secure, true)
	// CSRF token 必须能被 JS 读到，以便 axios 拦截器复制到请求头。
	c.SetCookie(auth.CookieCSRF, csrf, maxAge, "/", "", secure, false)
	return nil
}

// clearCookie 立即过期指定 cookie。
func (h *Handler) clearCookie(c *gin.Context, name string, httpOnly bool) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(name, "", -1, "/", "", h.secureCookie(c), httpOnly)
}

// secureCookie 判断是否应给 cookie 加 Secure 属性。
//
// 只在确认走 HTTPS 时加：本地 HTTP 开发环境若加了 Secure，浏览器会直接丢弃
// cookie，导致登录不上。反向代理场景依赖 X-Forwarded-Proto。
func (h *Handler) secureCookie(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}
