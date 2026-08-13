// Package handler 实现 HTTP 接口层。handler 保持轻薄：绑定参数、调用
// service、返回响应；业务规则一律放在 internal/service。
//
// 响应构件本身放在 internal/httpx（middleware 也要用，放这里会形成 import 环）。
// 下面的别名让 handler 内的调用点保持简洁。
package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// Page 是分页响应结构。
type Page = httpx.Page

// 响应构件别名。
var (
	Fail       = httpx.Fail
	BadRequest = httpx.BadRequest
	NotFound   = httpx.NotFound
	Conflict   = httpx.Conflict
	Internal   = httpx.Internal
	OK         = httpx.OK
	Pagination = httpx.Pagination
	IDParam    = httpx.IDParam
)

// CodeNotFound 供路由层构造 404 响应使用。
const CodeNotFound = httpx.CodeNotFound

// bindJSON 解析请求体，失败时直接写回 400。
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		BadRequest(c, "请求参数格式不正确")
		return false
	}
	return true
}

// noContent 返回 204。
func noContent(c *gin.Context) {
	httpx.NoContent(c)
}
