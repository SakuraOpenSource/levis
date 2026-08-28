package model

import "time"

// 实名认证状态。
const (
	KYCPending  = "pending"
	KYCApproved = "approved"
	KYCRejected = "rejected"
)

// ValidKYCStatus 报告 s 是否为受支持的实名认证状态。
func ValidKYCStatus(s string) bool {
	switch s {
	case KYCPending, KYCApproved, KYCRejected:
		return true
	}
	return false
}

// Verification 是用户的实名认证记录。
//
// UserID 唯一：一人一条，重新提交是覆盖既有记录而不是追加新行 ——
// 否则审核列表里会堆满同一个人的历史提交，管理员得自己找哪条是最新的。
//
// 注意本表含个人敏感信息（姓名、身份证号、证件照）。当前实现为明文存储，
// 防护口径是：data 目录 0700、照片文件 0600、用户侧接口把号码打码、
// 完整号码只在管理员审核详情里出现。
type Verification struct {
	Base
	UserID   uint   `gorm:"uniqueIndex;not null" json:"user_id"`
	RealName string `gorm:"size:64;not null" json:"real_name"`
	// IDNumber 建索引是为了查重（同一号码不能被两个账号通过认证），
	// 但不设唯一约束 —— 被驳回的记录不应占着号码。
	IDNumber string `gorm:"index;size:32;not null" json:"id_number"`
	// 证件照路径同 TicketAttachment.StoredPath，绝不下发。
	FrontPath    string     `gorm:"size:255;not null" json:"-"`
	BackPath     string     `gorm:"size:255;not null" json:"-"`
	Status       string     `gorm:"size:16;not null;default:pending" json:"status"`
	RejectReason string     `gorm:"size:255" json:"reject_reason"`
	ReviewedBy   uint       `json:"reviewed_by"`
	ReviewedAt   *time.Time `json:"reviewed_at"`
	SubmittedAt  time.Time  `json:"submitted_at"`
	// PluginID 与 CertifyID 仅在第三方认证流程中使用。人工上传认证保持为空。
	PluginID  string `gorm:"size:64" json:"plugin_id,omitempty"`
	CertifyID string `gorm:"size:128;index" json:"certify_id,omitempty"`
	// Username 是审核列表展示用的关联字段，不落库。
	Username string `gorm:"-" json:"username,omitempty"`
}

// MaskIDNumber 把身份证号中间部分替换为星号，只留前 6 位与后 4 位。
//
// 前 6 位是地区码、后 4 位含校验位，足够用户确认「填的是这张证」，
// 又不足以拼出完整号码。
func MaskIDNumber(id string) string {
	runes := []rune(id)
	if len(runes) <= 10 {
		// 长度不足时不做部分暴露，整串打码。
		return repeatRune('*', len(runes))
	}
	return string(runes[:6]) + repeatRune('*', len(runes)-10) + string(runes[len(runes)-4:])
}

// repeatRune 返回由 n 个 r 组成的字符串。
func repeatRune(r rune, n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]rune, n)
	for i := range buf {
		buf[i] = r
	}
	return string(buf)
}
