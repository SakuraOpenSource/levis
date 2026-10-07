package handler

import (
	"context"
	"github.com/gin-gonic/gin"
	"time"
)

// AdminInterfaceSecurityGroups 返回接口提供的安全组商品预设选项。
func (h *Handler) AdminInterfaceSecurityGroups(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if len(c.Request.URL.Query()) != 0 {
		BadRequest(c, "安全组目录不接受查询参数")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	out, err := h.hostFeatures().InterfaceSecurityGroups(ctx, id)
	respond(c, out, err)
}
