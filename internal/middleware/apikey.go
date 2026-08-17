package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// ctxScopes 是 gin.Context 中存放当前 Key 权限位的键名。
const ctxScopes = "levis_api_scopes"

// headerAPIKey 是 Authorization 之外的备选头，方便某些不便设置 Authorization
// 的客户端（部分 webhook 平台）。
const headerAPIKey = "X-Levis-Api-Key"

// RequireAPIKey 校验 API Key 并把对应用户放入 context。
//
// **完全不看 Cookie。** 带着有效登录态但不带 Key 的请求一律 401 —— 这是
// 「浏览器端」与「机器端」的分界线，模糊掉就等于让开放接口继承了浏览器的
// 隐式凭证，而那条路径上没有 CSRF 防护。
func RequireAPIKey(rt *runtime.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret := extractAPIKey(c)
		if secret == "" {
			httpx.Unauthorized(c, "请在 Authorization 头中提供 API Key")
			return
		}

		keys := service.NewAPIKeyService(rt.DB())
		key, err := keys.Authenticate(secret)
		if err != nil {
			if bizErr, ok := service.AsError(err); ok {
				httpx.Fail(c, bizErr.Status, bizErr.Code, bizErr.Message)
				return
			}
			httpx.Internal(c, "")
			return
		}

		// 与 RequireAuth 同一口径回查用户：账号被停用能立刻生效，
		// 下游 service 拿到的也是同样的 *model.User，无需为 API 另写一套。
		var user model.User
		if err := rt.DB().First(&user, key.UserID).Error; err != nil {
			httpx.Unauthorized(c, "API Key 无效")
			return
		}
		if user.Status != model.UserActive {
			httpx.Forbidden(c, "账号已被禁用")
			return
		}

		keys.TouchLastUsed(key)
		httpx.SetUser(c, &user)
		c.Set(ctxScopes, key.Scopes)
		c.Next()
	}
}

// RequireScope 要求当前 Key 具备指定权限位。必须挂在 RequireAPIKey 之后。
func RequireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, ok := c.Get(ctxScopes)
		if !ok {
			httpx.Unauthorized(c, "请在 Authorization 头中提供 API Key")
			return
		}
		scopes, _ := value.(model.ScopeList)
		if !scopes.Has(scope) {
			httpx.Forbidden(c, "该 API Key 缺少 "+scope+" 权限")
			return
		}
		c.Next()
	}
}

// extractAPIKey 从请求头中取出明文 Key。
func extractAPIKey(c *gin.Context) string {
	if header := c.GetHeader("Authorization"); header != "" {
		// 只认 Bearer：Basic 之类的其它方案在这里没有定义，
		// 静默接受反而会让调用方误以为自己用对了。
		if value, ok := cutPrefixFold(header, "Bearer "); ok {
			return strings.TrimSpace(value)
		}
		return ""
	}
	return strings.TrimSpace(c.GetHeader(headerAPIKey))
}

// cutPrefixFold 按大小写不敏感的方式去掉前缀。
//
// RFC 7235 规定 auth-scheme 大小写不敏感，实际请求里 "bearer" 与 "Bearer"
// 都会遇到。
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}
