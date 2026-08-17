package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// APIKeys 返回当前用户的 Key 列表与可选权限位。
//
// 附上 scopes 清单，前端的勾选项就不必再硬编码一份 —— 两处各自维护迟早不一致。
func (h *Handler) APIKeys(c *gin.Context) {
	items, err := h.apiKeys().List(httpx.CurrentUserID(c))
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"items": items, "scopes": model.AllScopes()})
}

// CreateAPIKey 创建一把 Key。需先通过实名认证。
//
// 响应里的 secret 是明文，且是它在系统里唯一一次露面 —— 库里只有 SHA-256。
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req service.APIKeyCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	userID := httpx.CurrentUserID(c)

	approved, err := h.kyc().IsApproved(userID)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if !approved {
		httpx.Forbidden(c, "请先完成实名认证后再创建 API Key")
		return
	}

	created, err := h.apiKeys().Create(userID, req)
	respond(c, created, err)
}

// RevokeAPIKey 吊销一把 Key。
func (h *Handler) RevokeAPIKey(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.apiKeys().Revoke(id, httpx.CurrentUserID(c)); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}
