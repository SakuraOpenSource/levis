package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// newGinContext builds a minimal gin context backed by a recorder.
func newGinContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, rec
}

// PLG-F06: typed gRPC failures from the plugin boundary must map to honest
// HTTP statuses instead of a blanket 500/409. A blanket 500 hides
// feature-unsupported old plugins, bad payloads, missing upstream resources
// and timeouts from clients and monitoring alike.
func TestRespondMapsGRPCErrorCategories(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"InvalidArgument -> 400", status.Error(codes.InvalidArgument, "bad payload"), http.StatusBadRequest},
		{"NotFound -> 404", status.Error(codes.NotFound, "backup gone"), http.StatusNotFound},
		{"PermissionDenied -> 403", status.Error(codes.PermissionDenied, "not yours"), http.StatusForbidden},
		{"DeadlineExceeded -> 504", status.Error(codes.DeadlineExceeded, "slow"), http.StatusGatewayTimeout},
		{"Unimplemented -> 501", status.Error(codes.Unimplemented, "old plugin"), http.StatusNotImplemented},
		{"Unavailable -> 503", status.Error(codes.Unavailable, "down"), http.StatusServiceUnavailable},
		{"Unauthenticated -> 401", status.Error(codes.Unauthenticated, "no token"), http.StatusUnauthorized},
		{"Internal -> 500", status.Error(codes.Internal, "boom"), http.StatusInternalServerError},
		{"Unknown -> 500", status.Error(codes.Unknown, "mystery"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newGinContext()
			respond(c, nil, tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// PLG-F06: business errors keep their explicit status; plain errors stay 500.
func TestRespondKeepsBusinessAndInternalErrors(t *testing.T) {
	c, rec := newGinContext()
	respond(c, nil, service.ErrBadRequest("x"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("business error status = %d", rec.Code)
	}
	c, rec = newGinContext()
	respond(c, nil, errors.New("plain"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("plain error status = %d", rec.Code)
	}
	c, rec = newGinContext()
	respond(c, map[string]int{"a": 1}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("success status = %d", rec.Code)
	}
}
