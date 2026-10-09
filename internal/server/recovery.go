package server

import (
	"errors"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"runtime/debug"
)

// Gin 1.12 treats ErrAbortHandler as a broken pipe and swallows it, producing a
// normal HTTP EOF for truncated downloads. net/http must receive this sentinel.
func recoveryPreservingAbort() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(error); ok && errors.Is(e, http.ErrAbortHandler) {
					panic(http.ErrAbortHandler)
				}
				log.Printf("HTTP handler panic: %v\n%s", r, debug.Stack())
				if c.Writer.Written() {
					panic(http.ErrAbortHandler)
				}
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}
