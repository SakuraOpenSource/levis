package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The gRPC branch must surface plugin-protocol messages, never raw internals:
// a DB/dial/file error converts to codes.Unknown, and its Error() text (SQL
// dialect strings, host:port, file paths) must not reach the client body.
func TestRespondInternalErrorDoesNotLeakRawErrorText(t *testing.T) {
	for _, raw := range []string{
		"sql: no rows in result set (dialect postgres dsn host=10.0.0.9:5432 user=levis)",
		`open C:\secret\deploy\key.pem: The system cannot find the file specified.`,
	} {
		gin.SetMode(gin.TestMode)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		respond(c, nil, errors.New(raw))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("internal error must stay 500, got %d", rec.Code)
		}
		if body := rec.Body.String(); contains(body, raw) {
			t.Fatalf("internal error text leaked to client: %s", body)
		}
	}
}

// A genuine plugin-protocol status keeps its semantic category and message.
func TestRespondGrpcStatusKeepsMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	respond(c, nil, status.Error(codes.Unimplemented, "refund RPC unavailable"))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("typed plugin status must map to 501, got %d", rec.Code)
	}
	if body := rec.Body.String(); !contains(body, "refund RPC unavailable") {
		t.Fatalf("typed plugin message should stay visible, got: %s", body)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
