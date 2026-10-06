package handler

import (
	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/gin-gonic/gin"
)

func (h *Handler) ServiceAutoRenew(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		AutoRenew *bool `json:"auto_renew"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.AutoRenew == nil {
		BadRequest(c, "缺少 auto_renew")
		return
	}
	out, err := h.billing().SetAutoRenew(httpx.CurrentUserID(c), id, *req.AutoRenew)
	respond(c, out, err)
}
