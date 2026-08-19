package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

type PaymentCreateRequest struct {
	Purpose     string `json:"purpose"`
	TargetID    uint   `json:"target_id"`
	PluginID    string `json:"plugin_id"`
	AmountCents int64  `json:"amount_cents"`
}

func (h *Handler) PaymentMethods(c *gin.Context) {
	items, err := h.payments().Methods()
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"items": items})
}

func (h *Handler) CreatePayment(c *gin.Context) {
	var req PaymentCreateRequest
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.payments().Create(c.Request.Context(), httpx.CurrentUserID(c), c.ClientIP(), service.PaymentCreateInput{Purpose: req.Purpose, TargetID: req.TargetID, PluginID: req.PluginID, AmountCents: req.AmountCents})
	respond(c, item, err)
}

func (h *Handler) Payment(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.payments().Get(httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

func (h *Handler) QueryPayment(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.payments().Query(c.Request.Context(), httpx.CurrentUserID(c), id)
	respond(c, item, err)
}

// PaymentNotify handles gateway callback notifications from payment plugins.
func (h *Handler) PaymentNotify(c *gin.Context) {
	pluginID := c.Param("plugin")
	if pluginID == "" {
		BadRequest(c, "缺少插件标识")
		return
	}
	if h.plugins == nil {
		Fail(c, 503, "PLUGIN_UNAVAILABLE", "插件系统未启用")
		return
	}

	raw := make(map[string]string)
	if c.Request.Method == "GET" {
		for key, values := range c.Request.URL.Query() {
			if len(values) > 0 {
				raw[key] = values[0]
			}
		}
	} else {
		if err := c.Request.ParseForm(); err != nil {
			BadRequest(c, "无法解析回调参数")
			return
		}
		for key, values := range c.Request.PostForm {
			if len(values) > 0 {
				raw[key] = values[0]
			}
		}
	}

	reply, err := h.plugins.VerifyPaymentCallback(c.Request.Context(), pluginID, &pb.VerifyPaymentCallbackRequest{Raw: raw})
	if err != nil {
		respond(c, nil, err)
		return
	}

	if reply.GetState() != pb.PaymentState_PAYMENT_STATE_PAID {
		c.String(200, "fail")
		return
	}

	if reply.GetCurrency() != "" && reply.GetCurrency() != "CNY" {
		c.String(200, "fail")
		return
	}

	if err := h.payments().FinalizeCallback(c.Request.Context(), pluginID, reply.GetExternalId(), reply.GetPaidAmountCents(), reply.GetGatewayRef()); err != nil {
		c.String(200, "fail")
		return
	}

	c.String(200, "success")
}
