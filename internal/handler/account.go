package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
)

// Wallet 返回钱包概览（余额、未付账单、在用服务数）。
func (h *Handler) Wallet(c *gin.Context) {
	overview, err := h.wallet().Overview(httpx.CurrentUserID(c))
	respond(c, overview, err)
}

// Transactions 分页返回余额流水。
func (h *Handler) Transactions(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.wallet().Transactions(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// RechargeRequest 是充值入参。
type RechargeRequest struct {
	AmountCents int64 `json:"amount_cents"`
}

// Recharge 为当前用户充值（假充值，等待接入真实支付渠道）。
func (h *Handler) Recharge(c *gin.Context) {
	var req RechargeRequest
	if !bindJSON(c, &req) {
		return
	}
	record, err := h.wallet().Recharge(httpx.CurrentUserID(c), req.AmountCents)
	respond(c, record, err)
}

// Services 分页返回已购服务。
func (h *Handler) Services(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.billing().Services(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// Service 返回单个已购服务。
func (h *Handler) Service(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.billing().Service(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

// RenewService 为已购服务续费一个周期。
func (h *Handler) RenewService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	result, err := h.billing().Renew(httpx.CurrentUserID(c), id)
	respond(c, result, err)
}

// ServicePowerRequest 是电源操作入参。
type ServicePowerRequest struct {
	// Action 取值：boot / shutdown / reboot / reinstall。
	Action string `json:"action"`
	OS     string `json:"os"`
}

// ServicePower 对上游对接的服务执行电源操作（开机/关机/重启/重装系统）。
func (h *Handler) ServicePower(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req ServicePowerRequest
	if !bindJSON(c, &req) {
		return
	}
	err := h.billing().Power(httpx.CurrentUserID(c), id, req.Action, req.OS)
	respond(c, gin.H{"message": "操作已提交"}, err)
}

// ServiceUpstream 返回上游主机详情与支持的操作列表。
func (h *Handler) ServiceUpstream(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	info, err := h.billing().UpstreamInfo(httpx.CurrentUserID(c), id)
	respond(c, info, err)
}

// ServiceOS 返回上游主机可用的重装系统列表。
func (h *Handler) ServiceOS(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	items, err := h.billing().ListOS(httpx.CurrentUserID(c), id)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"items": items})
}

// Invoices 分页返回账单。
func (h *Handler) Invoices(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.billing().Invoices(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// Invoice 返回单个账单（含明细）。
func (h *Handler) Invoice(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.billing().Invoice(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}
