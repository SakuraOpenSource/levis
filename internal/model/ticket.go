package model

import "time"

// 工单状态。
//
// answered 与 open 的区分是给列表页用的：客服只需要盯着 open，
// 用户只需要留意 answered，两边都不必逐条点开看最后一条是谁发的。
const (
	TicketOpen     = "open"     // 等客服处理
	TicketAnswered = "answered" // 客服已回复，等用户
	TicketClosed   = "closed"
)

// ValidTicketStatus 报告 s 是否为受支持的工单状态。
func ValidTicketStatus(s string) bool {
	switch s {
	case TicketOpen, TicketAnswered, TicketClosed:
		return true
	}
	return false
}

// Ticket 是一张工单。
//
// 正文不放在本表：建单等于「一张工单 + 第一条回复」，这样首帖与后续回复
// 在展示和附件处理上是同一套代码，不必为首帖另开一条分支。
type Ticket struct {
	Base
	TicketNo string `gorm:"uniqueIndex;size:32;not null" json:"ticket_no"`
	UserID   uint   `gorm:"index;not null" json:"user_id"`
	Subject  string `gorm:"size:200;not null" json:"subject"`
	Status   string `gorm:"size:16;not null;default:open" json:"status"`
	// LastReplyAt 用于列表排序。不用 UpdatedAt：改个状态也会动那个字段，
	// 排序结果会跟着跳。
	LastReplyAt *time.Time    `json:"last_reply_at"`
	Replies     []TicketReply `gorm:"foreignKey:TicketID" json:"replies,omitempty"`
	// Username 是列表页展示用的关联字段，不落库。
	Username string `gorm:"-" json:"username,omitempty"`
}

// TicketReply 是工单中的一条回复。
//
// IsStaff 与 AuthorName 是刻意冗余的快照：作者日后被删号或降权，
// 历史对话仍要正确显示「谁在什么身份下说了这句话」。
type TicketReply struct {
	Base
	TicketID    uint               `gorm:"index;not null" json:"ticket_id"`
	UserID      uint               `gorm:"index;not null" json:"user_id"`
	IsStaff     bool               `gorm:"not null;default:false" json:"is_staff"`
	AuthorName  string             `gorm:"size:64;not null" json:"author_name"`
	Body        string             `gorm:"type:text;not null" json:"body"`
	Attachments []TicketAttachment `gorm:"foreignKey:ReplyID" json:"attachments,omitempty"`
}

// TicketAttachment 是回复附带的文件。
//
// 文件本体落在 data/uploads 下，本表只存元数据。StoredPath 带 json:"-"：
// 磁盘布局属于实现细节，一旦下发就等于把猜路径的活儿交给了客户端。
type TicketAttachment struct {
	Base
	ReplyID uint `gorm:"index;not null" json:"reply_id"`
	// TicketID 是冗余字段：鉴权与删号清理都按工单粒度做，
	// 有了它就不必为每次下载多join一次回复表。
	TicketID uint   `gorm:"index;not null" json:"ticket_id"`
	FileName string `gorm:"size:255;not null" json:"file_name"`
	// MimeType 由服务端嗅探内容得出，不采信客户端声明的 Content-Type。
	MimeType   string `gorm:"size:128;not null" json:"mime_type"`
	SizeBytes  int64  `gorm:"not null;default:0" json:"size_bytes"`
	StoredPath string `gorm:"size:255;not null" json:"-"`
}
