// Package httpx 提供 HTTP 层的公共构件：统一响应格式、分页解析与登录态存取。
//
// 这是一个叶子包，只依赖 gin 与 model。handler 与 middleware 都需要这些构件，
// 若放在其中任一方就会形成 import 环，因此单独抽出。
package httpx

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// 错误码。前端据此判断具体错误类型，message 仅用于展示。
const (
	CodeBadRequest   = "BAD_REQUEST"
	CodeUnauthorized = "UNAUTHORIZED"
	CodeForbidden    = "FORBIDDEN"
	CodeNotFound     = "NOT_FOUND"
	CodeConflict     = "CONFLICT"
	CodeNotInstalled = "NOT_INSTALLED"
	CodeInternal     = "INTERNAL"
)

// ErrorBody 是统一的错误响应结构。
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Fail 以指定状态码返回错误，并终止后续 handler。
func Fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorBody{Code: code, Message: message})
}

// BadRequest 返回 400。
func BadRequest(c *gin.Context, message string) {
	Fail(c, http.StatusBadRequest, CodeBadRequest, message)
}

// Unauthorized 返回 401。
func Unauthorized(c *gin.Context, message string) {
	Fail(c, http.StatusUnauthorized, CodeUnauthorized, message)
}

// Forbidden 返回 403。
func Forbidden(c *gin.Context, message string) {
	Fail(c, http.StatusForbidden, CodeForbidden, message)
}

// NotFound 返回 404。
func NotFound(c *gin.Context, message string) {
	Fail(c, http.StatusNotFound, CodeNotFound, message)
}

// Conflict 返回 409。
func Conflict(c *gin.Context, message string) {
	Fail(c, http.StatusConflict, CodeConflict, message)
}

// Internal 返回 500。内部错误细节不外泄，只返回一句通用提示。
func Internal(c *gin.Context, message string) {
	if message == "" {
		message = "服务器内部错误"
	}
	Fail(c, http.StatusInternalServerError, CodeInternal, message)
}

// OK 返回 200 与数据体。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, data)
}

// NoContent 返回 204。
func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Page 是分页响应结构。
type Page struct {
	Items    any   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// 分页参数边界。
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Pagination 从查询串解析 page 与 page_size，并做边界收敛。
// 返回值可直接用于 GORM 的 Offset/Limit。
func Pagination(c *gin.Context) (page, pageSize, offset int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(c.Query("page_size"))
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return page, pageSize, (page - 1) * pageSize
}

// IDParam 解析路径参数中的数字 ID，失败时已写回 400。
func IDParam(c *gin.Context, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || id == 0 {
		BadRequest(c, "无效的 ID")
		return 0, false
	}
	return uint(id), true
}

// QueryUint 解析查询串中的无符号整数，缺失或非法时返回 0。
func QueryUint(c *gin.Context, name string) uint {
	value, err := strconv.ParseUint(c.Query(name), 10, 64)
	if err != nil {
		return 0
	}
	return uint(value)
}

// gin.Context 中的键名。
const (
	ctxUser   = "levis_user"
	ctxUserID = "levis_user_id"
)

// SetUser 把已认证用户写入请求上下文。
func SetUser(c *gin.Context, user *model.User) {
	c.Set(ctxUser, user)
	c.Set(ctxUserID, user.ID)
}

// CurrentUser 返回当前登录用户；未登录时返回 nil。
func CurrentUser(c *gin.Context) *model.User {
	value, ok := c.Get(ctxUser)
	if !ok {
		return nil
	}
	user, _ := value.(*model.User)
	return user
}

// CurrentUserID 返回当前登录用户 ID；未登录时返回 0。
func CurrentUserID(c *gin.Context) uint {
	value, ok := c.Get(ctxUserID)
	if !ok {
		return 0
	}
	id, _ := value.(uint)
	return id
}
