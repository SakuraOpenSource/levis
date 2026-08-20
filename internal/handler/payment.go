package handler

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func parseMethodConfig(raw string) map[string]string {
	if raw == "" {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]string{}
	}
	if m == nil {
		return map[string]string{}
	}
	return m
}

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

	tryVerify := func(cfg map[string]string) (*pb.VerifyPaymentCallbackReply, error) {
		return h.plugins.VerifyPaymentCallback(c.Request.Context(), pluginID, &pb.VerifyPaymentCallbackRequest{Raw: raw, Config: cfg})
	}
	var reply *pb.VerifyPaymentCallbackReply
	var err error

	// 1) 路由中直接带 method：/payment-notify/:plugin/:method
	if methodStr := c.Param("method"); methodStr != "" {
		if mid, e := strconv.ParseUint(methodStr, 10, 64); e == nil {
			var m model.PaymentMethod
			if e2 := h.db().First(&m, uint(mid)).Error; e2 == nil && m.PluginID == pluginID {
				cfg := parseMethodConfig(m.Config)
				reply, err = tryVerify(cfg)
			}
		}
	}
	// 2) 无 method 路由：尝试通过 external_id 关联的支付方式定位配置
	if reply == nil {
		if outTradeNo := raw["out_trade_no"]; outTradeNo != "" {
			var pay model.ExternalPayment
			if e := h.db().First(&pay, "plugin_id = ? AND external_id = ?", pluginID, outTradeNo).Error; e == nil && pay.PaymentMethodID != nil {
				var m model.PaymentMethod
				if e2 := h.db().First(&m, *pay.PaymentMethodID).Error; e2 == nil {
					cfg := parseMethodConfig(m.Config)
					if r, e3 := tryVerify(cfg); e3 == nil {
						reply, err = r, nil
					} else {
						err = e3
					}
				}
			}
		}
	}
	// 3) 遍历该插件的所有启用支付方式依次尝试
	if reply == nil {
		var methods []model.PaymentMethod
		_ = h.db().Where("plugin_id = ? AND enabled = ?", pluginID, true).Order("id ASC").Find(&methods).Error
		for _, m := range methods {
			cfg := parseMethodConfig(m.Config)
			if r, e := tryVerify(cfg); e == nil {
				reply, err = r, nil
				break
			} else {
				err = e
			}
		}
	}
	// 4) 回退到空配置（兼容全局配置的旧插件）
	if reply == nil {
		reply, err = tryVerify(map[string]string{})
	}
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
