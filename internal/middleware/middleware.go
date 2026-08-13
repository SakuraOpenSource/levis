// Package middleware 提供安装态检查、认证、鉴权与 CSRF 防护。
package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// RequireInstalled 拦截未安装状态下对业务接口的访问。
//
// 程序可以在数据库尚不存在时启动（等待用户走安装流程），此时业务接口没有
// 可用的数据库句柄，必须挡在最外层，否则会解引用 nil。
func RequireInstalled(rt *runtime.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !rt.Installed() {
			httpx.Fail(c, http.StatusServiceUnavailable, httpx.CodeNotInstalled, "程序尚未完成安装")
			return
		}
		c.Next()
	}
}

// RequireAuth 校验 token cookie 并把用户实体放入 context。
//
// 这里每次请求都查库，而不是只信 JWT 里的 role：用户被禁用或降权后应立即
// 失效，不能等到 token 过期。
func RequireAuth(rt *runtime.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(auth.CookieToken)
		if err != nil || token == "" {
			httpx.Unauthorized(c, "请先登录")
			return
		}
		claims, err := auth.ParseToken(rt.JWTSecret(), token)
		if err != nil {
			httpx.Unauthorized(c, "登录已过期，请重新登录")
			return
		}

		var user model.User
		if err := rt.DB().First(&user, claims.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				httpx.Unauthorized(c, "账号不存在")
				return
			}
			httpx.Internal(c, "")
			return
		}
		if user.Status != model.UserActive {
			httpx.Forbidden(c, "账号已被禁用")
			return
		}

		httpx.SetUser(c, &user)
		c.Next()
	}
}

// RequireAdmin 要求当前用户是管理员。必须挂在 RequireAuth 之后。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := httpx.CurrentUser(c)
		if user == nil {
			httpx.Unauthorized(c, "请先登录")
			return
		}
		if !user.IsAdmin() {
			httpx.Forbidden(c, "需要管理员权限")
			return
		}
		c.Next()
	}
}

// CSRF 对所有写操作校验双提交令牌，并在安全请求上播种令牌。
//
// token 存在 SameSite=Lax 的 cookie 里，已能挡掉大多数跨站请求；双提交是
// 纵深防御：攻击者能诱导浏览器带上 cookie，但读不到 cookie 值来填请求头。
//
// 播种是必要的一步：全新访客手里没有任何 cookie，若不先给一个令牌，
// 连安装、登录、注册这三个「第一次写操作」都会被自己挡在门外。前端启动时
// 必然会发 GET /api/bootstrap，正好借这一次请求把令牌发下去。
func CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if cookie, err := c.Cookie(auth.CookieCSRF); err != nil || cookie == "" {
				issueCSRFCookie(c)
			}
			c.Next()
			return
		}
		cookie, err := c.Cookie(auth.CookieCSRF)
		if err != nil || !auth.SecureCompare(cookie, c.GetHeader(auth.HeaderCSRF)) {
			httpx.Forbidden(c, "CSRF 校验失败，请刷新页面重试")
			return
		}
		c.Next()
	}
}

// issueCSRFCookie 下发一个新的 CSRF 令牌。
//
// 该 cookie 不是 httpOnly —— 前端 axios 拦截器需要读取它并复制到请求头，
// 这正是双提交模式的工作方式。
func issueCSRFCookie(c *gin.Context) {
	token, err := auth.GenerateCSRFToken()
	if err != nil {
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.CookieCSRF, token, int(auth.TokenTTL.Seconds()), "/", "", secureRequest(c), false)
}

// secureRequest 判断当前请求是否走 HTTPS。
//
// 只在确认是 HTTPS 时给 cookie 加 Secure：本地 HTTP 开发环境若加了 Secure，
// 浏览器会直接丢弃 cookie。反向代理场景依赖 X-Forwarded-Proto。
func secureRequest(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}
