package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Verification 返回当前用户的实名认证状态；未提交过时 record 为 null。
func (h *Handler) Verification(c *gin.Context) {
	record, err := h.kyc().Mine(httpx.CurrentUserID(c))
	respond(c, gin.H{"record": record}, err)
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
func (h *Handler) AdminVerification(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	record, err := h.kyc().Get(id)
	respond(c, record, err)
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
