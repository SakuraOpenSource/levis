package server

import (
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoveryPropagatesStreamingAbort(t *testing.T) {
	_, h, _, _ := installedWithUsers(t)
	h.(*gin.Engine).GET("/test-stream-abort", func(c *gin.Context) { c.Writer.Write([]byte("prefix")); c.Writer.Flush(); panic(http.ErrAbortHandler) })
	srv := httptest.NewServer(h)
	defer srv.Close()
	r, e := srv.Client().Get(srv.URL + "/test-stream-abort")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	_, e = io.ReadAll(r.Body)
	if e == nil {
		t.Fatal("truncated download looked successful")
	}
}
