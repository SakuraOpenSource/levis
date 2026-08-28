package handler

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Verification 返回当前用户的实名认证状态与站点采用的认证模式。
// mode 为 manual 时前端渲染证件照上传表单；为插件 ID 时用 fields 渲染
// 该插件声明的动态表单。配置的插件不可用时回落人工审核，避免死胡同。
func (h *Handler) Verification(c *gin.Context) {
	record, err := h.kyc().Mine(httpx.CurrentUserID(c))
	if err != nil {
		respond(c, nil, err)
		return
	}
	mode, pluginName, fields := h.kycMode()
	respond(c, gin.H{
		"record":      record,
		"mode":        mode,
		"plugin_name": pluginName,
		"fields":      fields,
	}, nil)
}

// kycMode 返回当前实名认证模式与对应插件的字段声明。
// 返回的 mode 是实际生效的模式：配置的插件不可用时回落 manual。
func (h *Handler) kycMode() (mode, pluginName string, fields []service.KYCFieldSchema) {
	mode = h.settings().KYCMode()
	var inst *plugin.Instance
	if h.plugins != nil && mode != service.KYCModeManual {
		inst = h.plugins.KYCPluginByID(mode)
	}
	if inst == nil {
		return service.KYCModeManual, "", nil
	}
	schemas := service.KYCFieldSchemas(inst.Manifest().GetKycFields())
	if len(schemas) == 0 {
		// 声明了 KYC 却不告诉用户填什么，同样回落人工审核。
		return service.KYCModeManual, "", nil
	}
	return mode, inst.Snapshot().Name, schemas
}

// SubmitVerification 提交实名认证。请求体为 multipart：
// real_name、id_number、front、back。
func (h *Handler) SubmitVerification(c *gin.Context) {
	front, ok := formFile(c, "front")
	if !ok {
		return
	}
	back, ok := formFile(c, "back")
	if !ok {
		return
	}
	record, err := h.kyc().Submit(httpx.CurrentUserID(c), service.SubmitRequest{
		RealName: c.PostForm("real_name"),
		IDNumber: c.PostForm("id_number"),
		Front:    front,
		Back:     back,
	})
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 回给自己的响应同样打码：完整号码不该再离开服务端一次。
	record.IDNumber = model.MaskIDNumber(record.IDNumber)
	respond(c, record, nil)
}

// StartExternalVerificationRequest 是发起第三方实名认证的入参。
// values 的键由实名认证插件的字段声明决定，主程序不做结构假设。
type StartExternalVerificationRequest struct {
	Values map[string]string `json:"values"`
}

// StartExternalVerification 通过实名认证插件发起第三方认证。
// 返回认证单号与跳转地址/HTML，由前端引导用户完成认证。
func (h *Handler) StartExternalVerification(c *gin.Context) {
	var req StartExternalVerificationRequest
	if !bindJSON(c, &req) {
		return
	}
	record, reply, err := h.kyc().StartExternal(c.Request.Context(), httpx.CurrentUserID(c), req.Values)
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 回给前端的记录同样打码，完整号码只在发起 RPC 时用过一次。
	record.IDNumber = model.MaskIDNumber(record.IDNumber)
	respond(c, gin.H{
		"record":       record,
		"certify_id":   reply.GetCertifyId(),
		"certify_url":  reply.GetCertifyUrl(),
		"certify_html": reply.GetCertifyHtml(),
		"message":      reply.GetMessage(),
	}, nil)
}

// QueryExternalVerification 查询第三方实名认证结果，并在通过时更新本地状态。
// passed 为 T（通过）/ F（未通过）/ P（处理中）。
func (h *Handler) QueryExternalVerification(c *gin.Context) {
	record, passed, err := h.kyc().QueryExternal(c.Request.Context(), httpx.CurrentUserID(c))
	if err != nil {
		respond(c, nil, err)
		return
	}
	record.IDNumber = model.MaskIDNumber(record.IDNumber)
	respond(c, gin.H{"record": record, "passed": passed}, nil)
}

// VerificationPhoto 下发当前用户自己的证件照。
func (h *Handler) VerificationPhoto(c *gin.Context) {
	file, mime, err := h.kyc().Photo(httpx.CurrentUserID(c), c.Param("side"))
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 证件照要塞进 <img>，只能内联；安全性由 MIME 白名单 + nosniff 兜住。
	sendFile(c, file, mime, "", "inline")
}

// AdminVerifications 分页返回实名记录，可按 status 过滤。
func (h *Handler) AdminVerifications(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.kyc().List(c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminVerification 返回实名记录详情，身份证号完整 —— 审核必须拿它与照片比对。
// 插件模式的记录额外返回 input（用户提交的字段键值），供审核比对。
func (h *Handler) AdminVerification(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	record, err := h.kyc().Get(id)
	if err != nil {
		respond(c, nil, err)
		return
	}
	respond(c, gin.H{"record": record, "input": decodeKYCInput(record.InputJSON)}, nil)
}

// decodeKYCInput 解析第三方认证记录的用户输入；空值或坏 JSON 返回 nil，
// 管理端按 nil 渲染「无提交字段」。
func decodeKYCInput(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return nil
	}
	return input
}

// AdminVerificationPhoto 下发指定记录的证件照，供管理员审核。
func (h *Handler) AdminVerificationPhoto(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	file, mime, err := h.kyc().AdminPhoto(id, c.Param("side"))
	if err != nil {
		respond(c, nil, err)
		return
	}
	sendFile(c, file, mime, "", "inline")
}

// AdminApproveVerification 通过实名认证。
func (h *Handler) AdminApproveVerification(c *gin.Context) {
	h.reviewVerification(c, true, "")
}

// RejectVerificationRequest 是驳回实名认证的入参。
type RejectVerificationRequest struct {
	Reason string `json:"reason"`
}

// AdminRejectVerification 驳回实名认证，须填原因 —— 用户得知道该改什么。
func (h *Handler) AdminRejectVerification(c *gin.Context) {
	var req RejectVerificationRequest
	if !bindJSON(c, &req) {
		return
	}
	h.reviewVerification(c, false, req.Reason)
}

// reviewVerification 是通过与驳回的共用实现。
func (h *Handler) reviewVerification(c *gin.Context, approved bool, reason string) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	record, err := h.kyc().Review(id, httpx.CurrentUserID(c), approved, reason)
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 审核结果通知用户。审核已经落库，发信失败只进日志。
	h.notify.KYCReviewed(record.UserID, approved, record.RejectReason)
	OK(c, record)
}

// AdminKYCSettings 返回实名认证模式设置与可用的实名认证插件选项。
func (h *Handler) AdminKYCSettings(c *gin.Context) {
	type kycPluginOption struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	options := []kycPluginOption{}
	if h.plugins != nil {
		for _, inst := range h.plugins.KYCPlugins() {
			options = append(options, kycPluginOption{ID: inst.ID(), Name: inst.Snapshot().Name})
		}
	}
	OK(c, gin.H{"mode": h.settings().KYCMode(), "plugins": options})
}

// AdminUpdateKYCSettingsRequest 是保存实名认证模式的入参。
type AdminUpdateKYCSettingsRequest struct {
	Mode string `json:"mode"`
}

// AdminUpdateKYCSettings 保存实名认证模式：manual 或当前可用的实名插件 ID。
func (h *Handler) AdminUpdateKYCSettings(c *gin.Context) {
	var req AdminUpdateKYCSettingsRequest
	if !bindJSON(c, &req) {
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != service.KYCModeManual {
		available := false
		if h.plugins != nil {
			for _, inst := range h.plugins.KYCPlugins() {
				if inst.ID() == mode {
					available = true
					break
				}
			}
		}
		if !available {
			Fail(c, 400, "BAD_REQUEST", "实名认证模式只能是人工审核或当前可用的实名插件")
			return
		}
	}
	if err := h.settings().SaveKYCMode(mode); err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, gin.H{"mode": mode})
}
