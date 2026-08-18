package service

import (
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/storage"
)

// 工单内容长度与附件数量上限。
const (
	MaxTicketSubjectLen = 200
	MaxTicketBodyLen    = 10000
	MaxAttachments      = 5
	// MaxAttachmentBytes 是单个附件的上限（20 MiB）。
	MaxAttachmentBytes = 20 << 20
)

// attachmentCategory 是附件在 uploads 下的分类目录名。
const attachmentCategory = "tickets"

// TicketService 处理工单与回复。
type TicketService struct {
	db    *gorm.DB
	store *storage.Store
}

// NewTicketService 构造 TicketService。
func NewTicketService(db *gorm.DB, store *storage.Store) *TicketService {
	return &TicketService{db: db, store: store}
}

// Upload 是一个待保存的上传文件。
//
// Open 而不是 io.Reader：附件可能被重试或跳过，延迟到真正要写盘时再打开，
// 免得为一批文件同时占着句柄。
type Upload struct {
	FileName string
	Size     int64
	Open     func() (io.ReadCloser, error)
}

// TicketCreateRequest 是建单入参。
type TicketCreateRequest struct {
	Subject string
	Body    string
	Files   []Upload
}

// Create 创建工单及其首条回复。
//
// 建单等于「一张工单 + 第一条回复」：这样首帖与后续回复共用同一套展示与
// 附件处理代码。附件先落盘再入库，事务失败时由调用方清理已落盘的文件。
func (s *TicketService) Create(user *model.User, req TicketCreateRequest) (*model.Ticket, error) {
	subject := strings.TrimSpace(req.Subject)
	if err := validateSubject(subject); err != nil {
		return nil, err
	}
	body, err := validateBody(req.Body)
	if err != nil {
		return nil, err
	}
	if len(req.Files) > MaxAttachments {
		return nil, ErrBadRequest("单次最多上传 %d 个附件", MaxAttachments)
	}

	var (
		ticket model.Ticket
		saved  []string
	)
	err = s.db.Transaction(func(tx *gorm.DB) error {
		no, err := serialNo("TKT")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		ticket = model.Ticket{
			TicketNo:    no,
			UserID:      user.ID,
			Subject:     subject,
			Status:      model.TicketOpen,
			LastReplyAt: &now,
		}
		if err := tx.Create(&ticket).Error; err != nil {
			return err
		}
		reply, err := s.createReply(tx, &ticket, user, body, req.Files, &saved)
		if err != nil {
			return err
		}
		ticket.Replies = []model.TicketReply{*reply}
		return nil
	})
	if err != nil {
		// 事务回滚了，已落盘的附件成了无人引用的孤儿，就地清掉。
		s.store.RemoveAll(saved)
		return nil, err
	}
	return &ticket, nil
}

// Reply 追加一条回复，并返回所属工单。
//
// isStaff 决定状态流向：客服回复置为 answered（等用户看），用户回复置回
// open（等客服处理）。已关闭的工单一律拒绝回复。
//
// 一并返回工单是为了让调用方能发通知邮件：那需要工单号、主题与工单的归属用户，
// 而事务里本来就把工单读出来了，再查一遍是白费一次查询。
func (s *TicketService) Reply(
	ticketID uint, user *model.User, isStaff bool, body string, files []Upload,
) (*model.TicketReply, *model.Ticket, error) {
	text, err := validateBody(body)
	if err != nil {
		return nil, nil, err
	}
	if len(files) > MaxAttachments {
		return nil, nil, ErrBadRequest("单次最多上传 %d 个附件", MaxAttachments)
	}

	var (
		reply  *model.TicketReply
		ticket *model.Ticket
		saved  []string
	)
	err = s.db.Transaction(func(tx *gorm.DB) error {
		found, err := s.lockTicket(tx, ticketID, user.ID, isStaff)
		if err != nil {
			return err
		}
		if found.Status == model.TicketClosed {
			return ErrConflict("工单已关闭，请重新开启后再回复")
		}
		reply, err = s.createReply(tx, found, user, text, files, &saved)
		if err != nil {
			return err
		}
		ticket = found
		return nil
	})
	if err != nil {
		s.store.RemoveAll(saved)
		return nil, nil, err
	}
	return reply, ticket, nil
}

// createReply 在事务内写入一条回复及其附件，并更新工单状态与最后回复时间。
//
// 落盘成功的附件路径会追加到 saved，供调用方在事务回滚后清理。
func (s *TicketService) createReply(
	tx *gorm.DB, ticket *model.Ticket, user *model.User, body string, files []Upload, saved *[]string,
) (*model.TicketReply, error) {
	isStaff := user.IsAdmin()
	reply := model.TicketReply{
		TicketID: ticket.ID,
		UserID:   user.ID,
		IsStaff:  isStaff,
		// 快照用户名：作者日后改名或删号，历史对话仍显示当时的身份。
		AuthorName: user.Username,
		Body:       body,
	}
	if err := tx.Create(&reply).Error; err != nil {
		return nil, err
	}

	attachments, err := s.saveAttachments(ticket.ID, reply.ID, files, saved)
	if err != nil {
		return nil, err
	}
	if len(attachments) > 0 {
		if err := tx.Create(&attachments).Error; err != nil {
			return nil, err
		}
		reply.Attachments = attachments
	}

	now := time.Now().UTC()
	status := model.TicketOpen
	if isStaff {
		status = model.TicketAnswered
	}
	err = tx.Model(&model.Ticket{}).Where("id = ?", ticket.ID).
		Updates(map[string]any{"status": status, "last_reply_at": now}).Error
	if err != nil {
		return nil, err
	}
	ticket.Status = status
	ticket.LastReplyAt = &now
	return &reply, nil
}

// Close 关闭工单。用户与管理员都可以关。
func (s *TicketService) Close(ticketID, userID uint, isStaff bool) error {
	return s.setStatus(ticketID, userID, isStaff, model.TicketClosed, "工单已经是关闭状态")
}

// Reopen 重新开启工单。仅管理员可用。
func (s *TicketService) Reopen(ticketID uint) error {
	result := s.db.Model(&model.Ticket{}).
		Where("id = ? AND status = ?", ticketID, model.TicketClosed).
		Update("status", model.TicketOpen)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// 工单不存在与「本就没关闭」都走这里；后者更常见，据此给提示。
		var count int64
		if err := s.db.Model(&model.Ticket{}).Where("id = ?", ticketID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound("工单不存在")
		}
		return ErrConflict("工单未关闭，无需重新开启")
	}
	return nil
}

// setStatus 修改工单状态，非管理员只能改自己的工单。
func (s *TicketService) setStatus(ticketID, userID uint, isStaff bool, status, conflictMsg string) error {
	query := s.db.Model(&model.Ticket{}).Where("id = ?", ticketID)
	if !isStaff {
		query = query.Where("user_id = ?", userID)
	}
	result := query.Where("status <> ?", status).Update("status", status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// 区分「不存在/越权」与「状态已是目标值」。前者一律报 404：
		// 403 等于确认这个 ID 存在，把资源存在性也泄露了。
		exists := s.db.Model(&model.Ticket{}).Where("id = ?", ticketID)
		if !isStaff {
			exists = exists.Where("user_id = ?", userID)
		}
		var count int64
		if err := exists.Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrNotFound("工单不存在")
		}
		return ErrConflict("%s", conflictMsg)
	}
	return nil
}

// lockTicket 读取工单并校验归属。非管理员访问他人工单时报 404。
func (s *TicketService) lockTicket(tx *gorm.DB, ticketID, userID uint, isStaff bool) (*model.Ticket, error) {
	query := tx.Where("id = ?", ticketID)
	if !isStaff {
		query = query.Where("user_id = ?", userID)
	}
	var ticket model.Ticket
	if err := query.First(&ticket).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("工单不存在")
		}
		return nil, err
	}
	return &ticket, nil
}

// List 分页返回用户自己的工单。
func (s *TicketService) List(userID uint, offset, limit int) ([]model.Ticket, int64, error) {
	return s.list(s.db.Where("user_id = ?", userID), offset, limit, false)
}

// ListAll 分页返回全部工单，可按状态过滤。供管理端使用。
func (s *TicketService) ListAll(status string, offset, limit int) ([]model.Ticket, int64, error) {
	query := s.db.Session(&gorm.Session{})
	if status != "" {
		if !model.ValidTicketStatus(status) {
			return nil, 0, ErrBadRequest("无效的工单状态")
		}
		query = query.Where("status = ?", status)
	}
	return s.list(query, offset, limit, true)
}

// list 是分页查询的共用实现。withUser 为真时附上提交人用户名。
func (s *TicketService) list(query *gorm.DB, offset, limit int, withUser bool) ([]model.Ticket, int64, error) {
	var total int64
	if err := query.Session(&gorm.Session{}).Model(&model.Ticket{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var items []model.Ticket
	// 按最后回复时间倒序：有新动静的工单浮到最前。NULL 兜底到 id。
	err := query.Session(&gorm.Session{}).Model(&model.Ticket{}).
		Order("last_reply_at DESC, id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	if withUser && len(items) > 0 {
		if err := s.attachUsernames(items); err != nil {
			return nil, 0, err
		}
	}
	return items, total, nil
}

// attachUsernames 一次查库补齐提交人用户名，避免逐条 N+1。
func (s *TicketService) attachUsernames(items []model.Ticket) error {
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UserID)
	}
	var users []model.User
	if err := s.db.Select("id", "username").Where("id IN ?", ids).Find(&users).Error; err != nil {
		return err
	}
	names := make(map[uint]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Username
	}
	for i := range items {
		items[i].Username = names[items[i].UserID]
	}
	return nil
}

// Get 读取工单详情（含回复与附件）。非管理员只能读自己的。
func (s *TicketService) Get(ticketID, userID uint, isStaff bool) (*model.Ticket, error) {
	query := s.db.
		Preload("Replies", func(db *gorm.DB) *gorm.DB { return db.Order("id ASC") }).
		Preload("Replies.Attachments").
		Where("id = ?", ticketID)
	if !isStaff {
		query = query.Where("user_id = ?", userID)
	}

	var ticket model.Ticket
	if err := query.First(&ticket).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("工单不存在")
		}
		return nil, err
	}
	if isStaff {
		var user model.User
		if err := s.db.Select("id", "username").First(&user, ticket.UserID).Error; err == nil {
			ticket.Username = user.Username
		}
	}
	return &ticket, nil
}

// Attachment 读取附件元数据并校验归属。
func (s *TicketService) Attachment(ticketID, attachmentID, userID uint, isStaff bool) (*model.TicketAttachment, error) {
	// 先确认工单可见，再取附件：附件 ID 全局唯一，若不绑定工单校验，
	// 换个 ticket_id 就能拿到别人的文件。
	if _, err := s.lockTicket(s.db, ticketID, userID, isStaff); err != nil {
		return nil, err
	}
	var item model.TicketAttachment
	err := s.db.First(&item, "id = ? AND ticket_id = ?", attachmentID, ticketID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("附件不存在")
		}
		return nil, err
	}
	return &item, nil
}

// UserAttachmentPaths 返回用户全部附件的磁盘路径，供删号时清理文件。
func (s *TicketService) UserAttachmentPaths(tx *gorm.DB, userID uint) ([]string, error) {
	var ticketIDs []uint
	if err := tx.Model(&model.Ticket{}).Where("user_id = ?", userID).
		Pluck("id", &ticketIDs).Error; err != nil {
		return nil, err
	}
	if len(ticketIDs) == 0 {
		return nil, nil
	}
	var paths []string
	err := tx.Model(&model.TicketAttachment{}).Where("ticket_id IN ?", ticketIDs).
		Pluck("stored_path", &paths).Error
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// DeleteUserTickets 在事务内删除用户的工单、回复与附件记录。
//
// 只删数据库行，磁盘文件由调用方在事务提交后清理 —— 事务回滚而文件已删，
// 就成了「行还在、文件没了」，那是用户能看见的错误。
func (s *TicketService) DeleteUserTickets(tx *gorm.DB, userID uint) error {
	var ticketIDs []uint
	if err := tx.Model(&model.Ticket{}).Where("user_id = ?", userID).
		Pluck("id", &ticketIDs).Error; err != nil {
		return err
	}
	if len(ticketIDs) == 0 {
		return nil
	}
	if err := tx.Where("ticket_id IN ?", ticketIDs).Delete(&model.TicketAttachment{}).Error; err != nil {
		return err
	}
	if err := tx.Where("ticket_id IN ?", ticketIDs).Delete(&model.TicketReply{}).Error; err != nil {
		return err
	}
	return tx.Where("id IN ?", ticketIDs).Delete(&model.Ticket{}).Error
}

// saveAttachments 把上传文件落盘并构造元数据行。
func (s *TicketService) saveAttachments(
	ticketID, replyID uint, files []Upload, saved *[]string,
) ([]model.TicketAttachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	out := make([]model.TicketAttachment, 0, len(files))
	for _, file := range files {
		if file.Size > MaxAttachmentBytes {
			return nil, ErrBadRequest("附件「%s」超过 %d MiB", file.FileName, MaxAttachmentBytes>>20)
		}
		reader, err := file.Open()
		if err != nil {
			return nil, ErrBadRequest("无法读取附件「%s」", file.FileName)
		}
		path, size, mime, err := s.store.Save(attachmentCategory, reader, MaxAttachmentBytes)
		reader.Close()
		if err != nil {
			if errors.Is(err, storage.ErrTooLarge) {
				return nil, ErrBadRequest("附件「%s」超过 %d MiB", file.FileName, MaxAttachmentBytes>>20)
			}
			if errors.Is(err, storage.ErrEmpty) {
				return nil, ErrBadRequest("附件「%s」内容为空", file.FileName)
			}
			return nil, err
		}
		*saved = append(*saved, path)
		out = append(out, model.TicketAttachment{
			ReplyID:    replyID,
			TicketID:   ticketID,
			FileName:   safeFileName(file.FileName),
			MimeType:   mime,
			SizeBytes:  size,
			StoredPath: path,
		})
	}
	return out, nil
}

// safeFileName 清理原始文件名，只用于展示与下载时的建议名。
//
// 落盘路径与它无关（Save 自己生成随机名），这里只是不让路径分隔符和控制
// 字符出现在 Content-Disposition 头里。
func safeFileName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if utf8.RuneCountInString(name) > 255 {
		runes := []rune(name)
		name = string(runes[:255])
	}
	return name
}

// validateSubject 校验工单主题。
func validateSubject(subject string) error {
	count := utf8.RuneCountInString(subject)
	if count == 0 {
		return ErrBadRequest("请填写工单主题")
	}
	if count > MaxTicketSubjectLen {
		return ErrBadRequest("主题不能超过 %d 个字符", MaxTicketSubjectLen)
	}
	return nil
}

// validateBody 校验正文，返回去除首尾空白后的内容。
func validateBody(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	count := utf8.RuneCountInString(trimmed)
	if count == 0 {
		return "", ErrBadRequest("请填写内容")
	}
	if count > MaxTicketBodyLen {
		return "", ErrBadRequest("内容不能超过 %d 个字符", MaxTicketBodyLen)
	}
	return trimmed, nil
}
