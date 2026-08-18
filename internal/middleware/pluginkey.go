package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// ctxPluginID 是 gin.Context 中存放当前插件 ID 的键名。
const ctxPluginID = "levis_plugin_id"

// ctxPluginScopes 是 gin.Context 中存放插件权限位的键名。
//
// 与用户 Key 的 ctxScopes 分开存：两套权限位的语义完全不同（一套是「操作我
// 自己的资源」，一套是「以系统身份操作任意用户的资源」），共用一个键会让
// RequireScope 与 RequirePluginScope 互相误判。
const ctxPluginScopes = "levis_plugin_scopes"

// RequirePluginKey 校验插件凭证，并把插件 ID 与权限位放入 context。
//
// 与 RequireAPIKey 一样**完全不看 Cookie**：带着有效登录态但不带插件凭证的
// 请求一律 401。这条边界尤其要紧 —— 这组接口能给任意用户加余额，若它继承了
// 浏览器的隐式凭证，一个诱导管理员点开的页面就能凭空造钱。
//
// 也不设置 httpx.SetUser：插件不是某个用户，下游要操作哪个用户由请求体里的
// user_id 指定并逐个校验，不存在「当前用户」这个概念。
func RequirePluginKey(rt *runtime.Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		secret := extractAPIKey(c)
		if secret == "" {
			httpx.Unauthorized(c, "请在 Authorization 头中提供插件凭证")
			return
		}

		keys := service.NewPluginKeyService(rt.DB())
		key, err := keys.Authenticate(secret)
		if err != nil {
			if bizErr, ok := service.AsError(err); ok {
				httpx.Fail(c, bizErr.Status, bizErr.Code, bizErr.Message)
				return
			}
			httpx.Internal(c, "")
			return
		}

		keys.TouchLastUsed(key)
		c.Set(ctxPluginID, key.PluginID)
		c.Set(ctxPluginScopes, key.Scopes)
		c.Next()
	}
}

// RequirePluginScope 要求当前插件凭证具备指定权限位。
// 必须挂在 RequirePluginKey 之后。
func RequirePluginScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, ok := c.Get(ctxPluginScopes)
		if !ok {
			httpx.Unauthorized(c, "请在 Authorization 头中提供插件凭证")
			return
		}
		scopes, _ := value.(model.ScopeList)
		if !scopes.Has(scope) {
			httpx.Forbidden(c, "该插件未获授权 "+scope+" 权限")
			return
		}
		c.Next()
	}
}

// CurrentPluginID 返回当前请求的插件 ID，未经插件认证时返回空串。
func CurrentPluginID(c *gin.Context) string {
	value, ok := c.Get(ctxPluginID)
	if !ok {
		return ""
	}
	id, _ := value.(string)
	return id
}
