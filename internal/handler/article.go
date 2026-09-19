package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/SakuraOpenSource/levis/internal/service"
)

// Articles 返回全部已发布文章的索引（不含正文），公开可读。
func (h *Handler) Articles(c *gin.Context) {
	items, err := h.articles().ListPublished()
	respond(c, items, err)
}

// Article 按 slug 返回已发布的知识库文章，公开可读。
func (h *Handler) Article(c *gin.Context) {
	item, err := h.articles().GetPublished(c.Param("slug"))
	respond(c, item, err)
}

// ArticleByID 按 ID 返回已发布的知识库文章，公开可读。
// 购买页按商品 agreement_article_id 解析协议标题与 slug 用。
func (h *Handler) ArticleByID(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.articles().GetPublishedByID(id)
	respond(c, item, err)
}

// AdminArticles 分页返回文章，支持 status 过滤。
func (h *Handler) AdminArticles(c *gin.Context) {
	page, pageSize, offset := Pagination(c)
	items, total, err := h.articles().AdminList(c.Query("status"), offset, pageSize)
	if err != nil {
		respond(c, nil, err)
		return
	}
	OK(c, Page{Items: items, Total: total, Page: page, PageSize: pageSize})
}

// AdminArticle 返回单篇文章（含草稿）。
func (h *Handler) AdminArticle(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	item, err := h.articles().AdminGet(id)
	respond(c, item, err)
}

// AdminCreateArticle 创建文章。
func (h *Handler) AdminCreateArticle(c *gin.Context) {
	var req service.ArticleInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.articles().Create(req)
	respond(c, item, err)
}

// AdminUpdateArticle 更新文章。
func (h *Handler) AdminUpdateArticle(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	var req service.ArticleInput
	if !bindJSON(c, &req) {
		return
	}
	item, err := h.articles().Update(id, req)
	respond(c, item, err)
}

// AdminDeleteArticle 删除文章。
func (h *Handler) AdminDeleteArticle(c *gin.Context) {
	id, ok := IDParam(c, "id")
	if !ok {
		return
	}
	if err := h.articles().Delete(id); err != nil {
		respond(c, nil, err)
		return
	}
	noContent(c)
}
