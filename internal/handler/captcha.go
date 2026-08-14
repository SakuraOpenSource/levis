package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// Captcha 签发一张图形验证码。
//
// 公开接口：登录与注册页在用户尚未登录时就要拿到它。
func (h *Handler) Captcha(c *gin.Context) {
	item, err := h.captcha().Issue()
	respond(c, item, err)
}

// AdminCaptchaSettings 返回验证码配置。
func (h *Handler) AdminCaptchaSettings(c *gin.Context) {
	OK(c, h.settings().Captcha())
}

// AdminUpdateCaptchaSettings 保存验证码配置。
func (h *Handler) AdminUpdateCaptchaSettings(c *gin.Context) {
	var req service.CaptchaConfig
	if !bindJSON(c, &req) {
		return
	}
	cfg, err := h.settings().SaveCaptcha(req)
	respond(c, cfg, err)
}
