package handler

import (
	"strings"

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
	// Action 取值：boot / shutdown / reboot / hard_boot / hard_stop / hard_restart / reinstall。
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

// ServiceTraffic 返回服务的累计流量与配额进度，供前端进度条使用。
func (h *Handler) ServiceTraffic(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	progress, err := h.billing().TrafficProgress(httpx.CurrentUserID(c), id)
	respond(c, progress, err)
}

// ServiceMetrics 返回上游主机的实时监控数据（CPU、内存、带宽）。
func (h *Handler) ServiceMetrics(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	metrics, err := h.billing().ServiceMetrics(httpx.CurrentUserID(c), id)
	respond(c, metrics, err)
}

// PayInvoice 用余额全额结清一张待付账单（订单账单走开通，续费账单顺延到期）。
func (h *Handler) PayInvoice(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.payments().SettleInvoice(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

// RenewInvoice 为服务创建一张待付续费账单，支付环节走统一收银台。
func (h *Handler) RenewInvoice(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.billing().CreateRenewalInvoice(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

// TrafficInvoiceRequest 是流量包加购入参：extra_gb 为数量，unit 取 GB 或 TB
// （大小写不敏感，缺省 GB；TB 按 1TB=1024GB 换算后校验与计费）。
type TrafficInvoiceRequest struct {
	ExtraGB int    `json:"extra_gb"`
	Unit    string `json:"unit"`
}

// TrafficInvoice 为服务创建一张待付流量包账单，支付环节走统一收银台
// （余额全额 / 余额抵扣 + 外部支付，purpose=invoice），结清后累加配额。
func (h *Handler) TrafficInvoice(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req TrafficInvoiceRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.ExtraGB < 1 {
		BadRequest(c, "加购流量需大于 0")
		return
	}
	extraGB := int64(req.ExtraGB)
	switch unit := strings.ToUpper(strings.TrimSpace(req.Unit)); unit {
	case "", "GB":
	case "TB":
		extraGB *= 1024
	default:
		BadRequest(c, "单位仅支持 GB 或 TB")
		return
	}
	if extraGB > 10240 {
		BadRequest(c, "单次加购流量不能超过 10240 GB（10 TB）")
		return
	}
	item, err := h.billing().CreateTrafficInvoice(httpx.CurrentUserID(c), id, int(extraGB))
	respond(c, item, err)
}

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

// RetryService 重试单个开通失败或待开通服务的上游开通。
func (h *Handler) RetryService(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.orders().RetryProvision(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}
