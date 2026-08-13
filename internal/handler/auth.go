package handler

import (
	"net/http"
	"strings"

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
	if err := h.issueSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"ok": true, "user": user})
}

// Register 注册普通用户。
func (h *Handler) Register(c *gin.Context) {
	// 用独立的请求结构体接收，绝不直接绑定到 model.User，
	// 否则客户端可传 role=admin 自我提权。
	var req service.RegisterRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.users().Register(req)
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
}

// Login 登录。管理员与普通用户共用此入口，前端按返回的 role 决定落地页。
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.users().Login(req.Identifier, req.Password)
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

// Logout 清除登录态 cookie。
func (h *Handler) Logout(c *gin.Context) {
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

// issueSession 签发 JWT 与 CSRF token 并写入 cookie。
func (h *Handler) issueSession(c *gin.Context, user *model.User) error {
	token, _, err := auth.GenerateToken(h.rt.JWTSecret(), user.ID, user.Role)
	if err != nil {
		return err
	}
	csrf, err := auth.GenerateCSRFToken()
	if err != nil {
		return err
	}
	maxAge := int(auth.TokenTTL.Seconds())
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
