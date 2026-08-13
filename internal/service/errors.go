// Package service 实现 Levis 的业务逻辑。
//
// 约定：service 返回的错误若是可预期的业务错误，一律包装为 *Error，由
// handler 层映射为对应的 HTTP 状态码；其余错误视为内部错误返回 500。
package service

import (
	"errors"
	"fmt"
	"net/http"
)

// Error 是带 HTTP 语义的业务错误。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// newError 构造业务错误。
func newError(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrBadRequest 返回 400 业务错误。
func ErrBadRequest(format string, args ...any) *Error {
	return newError(http.StatusBadRequest, "BAD_REQUEST", format, args...)
}

// ErrNotFound 返回 404 业务错误。
func ErrNotFound(format string, args ...any) *Error {
	return newError(http.StatusNotFound, "NOT_FOUND", format, args...)
}

// ErrConflict 返回 409 业务错误。
func ErrConflict(format string, args ...any) *Error {
	return newError(http.StatusConflict, "CONFLICT", format, args...)
}

// ErrForbidden 返回 403 业务错误。
func ErrForbidden(format string, args ...any) *Error {
	return newError(http.StatusForbidden, "FORBIDDEN", format, args...)
}

// ErrUnauthorized 返回 401 业务错误。
func ErrUnauthorized(format string, args ...any) *Error {
	return newError(http.StatusUnauthorized, "UNAUTHORIZED", format, args...)
}

// AsError 尝试把 err 还原为 *Error。
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
