package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// AdminEmailSettings 返回邮件配置（密码不回显）。
func (h *Handler) AdminEmailSettings(c *gin.Context) {
	OK(c, h.email().EmailSettings())
}

// AdminUpdateEmailSettingsRequest 是保存邮件配置的入参。
// SMTPPassword 为空表示保留原密码。
type AdminUpdateEmailSettingsRequest struct {
	service.EmailSettings
	SMTPPassword string `json:"smtp_password"`
}

// AdminUpdateEmailSettings 保存 SMTP 与邮箱验证码开关。
func (h *Handler) AdminUpdateEmailSettings(c *gin.Context) {
	var req AdminUpdateEmailSettingsRequest
	if !bindJSON(c, &req) {
		return
	}
	out, err := h.email().SaveEmailSettings(req.EmailSettings, req.SMTPPassword)
	respond(c, out, err)
}

// AdminEmailTestRequest 是发送测试邮件的入参。
type AdminEmailTestRequest struct {
	Email string `json:"email"`
}

// AdminEmailTest 发一封测试邮件验证 SMTP 配置。
func (h *Handler) AdminEmailTest(c *gin.Context) {
	var req AdminEmailTestRequest
	if !bindJSON(c, &req) {
		return
	}
	respond(c, gin.H{"ok": true}, h.email().SendTestMail(req.Email))
}

// EmailCodeRequest 是发送邮箱验证码的入参。
type EmailCodeRequest struct {
	Scene string `json:"scene"`
	Email string `json:"email"`
}

// EmailCode 给注册场景发送邮箱验证码。
//
// 登录场景不发独立验证码：登录码在密码校验通过后按票据签发，避免给
// 撞库者一个「这个邮箱存在」的探测口。
func (h *Handler) EmailCode(c *gin.Context) {
	var req EmailCodeRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Scene != service.EmailSceneRegister {
		BadRequest(c, "不支持的验证码场景")
		return
	}
	siteName, _ := h.settings().Site()
	if err := h.email().SendEmailCode(req.Scene, req.Email, siteName); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

// RegisterRequest 追加邮箱验证码字段。
type emailRegisterRequest struct {
	RegisterRequest
	EmailCode string `json:"email_code"`
}

// RegisterWithEmailCode 包装注册：开启邮箱验证码时先验码再建号。
func (h *Handler) registerVerifyEmail(c *gin.Context, req emailRegisterRequest) bool {
	if !h.email().EmailCodeEnabled(service.EmailSceneRegister) {
		return true
	}
	if err := h.email().VerifyEmailCode(service.EmailSceneRegister, req.RegisterRequest.Email, req.EmailCode); err != nil {
		respond(c, nil, err)
		return false
	}
	return true
}

// LoginEmailCodeRequest 是登录二次邮箱校验的入参。
type LoginEmailCodeRequest struct {
	Ticket string `json:"ticket"`
	Code   string `json:"code"`
}

// LoginEmailCode 校验登录票据与邮箱验证码，签发会话。
func (h *Handler) LoginEmailCode(c *gin.Context) {
	var req LoginEmailCodeRequest
	if !bindJSON(c, &req) {
		return
	}
	userID, err := h.email().VerifyLoginTicket(req.Ticket, req.Code)
	if err != nil {
		respond(c, nil, err)
		return
	}
	user, err := h.users().Get(userID)
	if err != nil {
		respond(c, nil, err)
		return
	}
	if err := h.issueSession(c, user); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"user": user})
}

// loginEmailChallenge 处理开启邮箱验证码后的登录第二步。
// 返回 true 表示已响应（要求验证码）；false 表示未开启，走原登录流程。
func (h *Handler) loginEmailChallenge(c *gin.Context, userID uint) bool {
	if !h.email().EmailCodeEnabled(service.EmailSceneLogin) {
		return false
	}
	user, err := h.users().Get(userID)
	if err != nil {
		respond(c, nil, err)
		return true
	}
	siteName, _ := h.settings().Site()
	ticket, masked, err := h.email().IssueLoginTicket(user, siteName)
	if err != nil {
		respond(c, nil, err)
		return true
	}
	OK(c, gin.H{"need_email_code": true, "ticket": ticket, "masked_email": masked})
	return true
}

// emailFlags 返回给前端的邮箱验证码开关（挂在 bootstrap）。
func (h *Handler) emailFlags() gin.H {
	return gin.H{
		"register": h.email().EmailCodeEnabled(service.EmailSceneRegister),
		"login":    h.email().EmailCodeEnabled(service.EmailSceneLogin),
	}
}
