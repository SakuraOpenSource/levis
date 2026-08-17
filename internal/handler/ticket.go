package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/httpx"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// Tickets 分页返回当前用户的工单。
func (h *Handler) Tickets(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.tickets().List(httpx.CurrentUserID(c), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// CreateTicket 创建工单。请求体为 multipart：subject、body、files[]。
func (h *Handler) CreateTicket(c *gin.Context) {
	files, ok := formFiles(c, "files")
	if !ok {
		return
	}
	ticket, err := h.tickets().Create(httpx.CurrentUser(c), service.TicketCreateRequest{
		Subject: c.PostForm("subject"),
		Body:    c.PostForm("body"),
		Files:   files,
	})
	respond(c, ticket, err)
}

// Ticket 返回当前用户的工单详情。
func (h *Handler) Ticket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	ticket, err := h.tickets().Get(id, httpx.CurrentUserID(c), false)
	respond(c, ticket, err)
}

// ReplyTicket 回复自己的工单。
func (h *Handler) ReplyTicket(c *gin.Context) {
	h.replyTicket(c, false)
}

// CloseTicket 关闭自己的工单。
func (h *Handler) CloseTicket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.tickets().Close(id, httpx.CurrentUserID(c), false); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// TicketAttachment 下发自己工单里的附件。
func (h *Handler) TicketAttachment(c *gin.Context) {
	h.ticketAttachment(c, false)
}

// AdminTickets 分页返回全部工单，可按 status 过滤。
func (h *Handler) AdminTickets(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.tickets().ListAll(c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminTicket 返回任意工单的详情。
func (h *Handler) AdminTicket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	ticket, err := h.tickets().Get(id, httpx.CurrentUserID(c), true)
	respond(c, ticket, err)
}

// AdminReplyTicket 以客服身份回复工单。
func (h *Handler) AdminReplyTicket(c *gin.Context) {
	h.replyTicket(c, true)
}

// AdminCloseTicket 关闭任意工单。
func (h *Handler) AdminCloseTicket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.tickets().Close(id, httpx.CurrentUserID(c), true); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// AdminReopenTicket 重新开启已关闭的工单。
func (h *Handler) AdminReopenTicket(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.tickets().Reopen(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}

// AdminTicketAttachment 下发任意工单里的附件。
func (h *Handler) AdminTicketAttachment(c *gin.Context) {
	h.ticketAttachment(c, true)
}

// replyTicket 是用户侧与管理侧回复的共用实现。
func (h *Handler) replyTicket(c *gin.Context, isStaff bool) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	files, ok := formFiles(c, "files")
	if !ok {
		return
	}
	reply, err := h.tickets().Reply(id, httpx.CurrentUser(c), isStaff, c.PostForm("body"), files)
	respond(c, reply, err)
}

// ticketAttachment 是附件下发的共用实现。
func (h *Handler) ticketAttachment(c *gin.Context, isStaff bool) {
	ticketID, ok := IDParam(c, "id")
	if !ok {
		return
	}
	attachmentID, ok := IDParam(c, "aid")
	if !ok {
		return
	}
	item, err := h.tickets().Attachment(ticketID, attachmentID, httpx.CurrentUserID(c), isStaff)
	if err != nil {
		respond(c, nil, err)
		return
	}
	file, err := h.storage.Open(item.StoredPath)
	if err != nil {
		NotFound(c, "附件文件不存在")
		return
	}
	// 一律按下载处理，不管是什么类型：内联展示等于让用户上传的内容在本站
	// 域下执行。
	sendFile(c, file, item.MimeType, item.FileName, "attachment")
}
