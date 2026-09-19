package service

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// ArticleService 提供知识库文章的读写，可被商品引用为购买协议。
type ArticleService struct {
	db *gorm.DB
}

// NewArticleService 构造 ArticleService。
func NewArticleService(db *gorm.DB) *ArticleService {
	return &ArticleService{db: db}
}

// ArticleInput 是文章的创建与更新入参。
type ArticleInput struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	ContentMD string `json:"content_md"`
	Status    string `json:"status"`
	SortOrder int    `json:"sort_order"`
}

// GetPublished 按 slug 读取已发布的文章，未发布或不存在一律返回 404，
// 避免通过公开接口探测草稿的存在。
func (s *ArticleService) GetPublished(slug string) (*model.Article, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, ErrNotFound("文章不存在")
	}
	var item model.Article
	err := s.db.First(&item, "slug = ? AND status = ?", slug, model.ArticlePublished).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("文章不存在")
		}
		return nil, err
	}
	return &item, nil
}

// GetPublishedByID 按 ID 读取已发布的文章，未发布或不存在一律返回 404，
// 避免通过公开接口探测草稿的存在。购买页按商品 agreement_article_id 解析用。
func (s *ArticleService) GetPublishedByID(id uint) (*model.Article, error) {
	if id == 0 {
		return nil, ErrNotFound("文章不存在")
	}
	var item model.Article
	err := s.db.First(&item, "id = ? AND status = ?", id, model.ArticlePublished).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("文章不存在")
		}
		return nil, err
	}
	return &item, nil
}

// ListPublished 返回全部已发布文章的索引（不含正文），按管理端排序。
// 供用户中心知识库列表页使用。
func (s *ArticleService) ListPublished() ([]model.Article, error) {
	var items []model.Article
	err := s.db.Model(&model.Article{}).
		Select("id, slug, title, status, sort_order, created_at, updated_at").
		Where("status = ?", model.ArticlePublished).
		Order("sort_order ASC, id ASC").
		Find(&items).Error
	if err != nil {
		return nil, err
	}
	return items, nil
}

// AdminList 分页返回文章，status 为空时不过滤。
func (s *ArticleService) AdminList(status string, offset, limit int) ([]model.Article, int64, error) {
	query := s.db.Model(&model.Article{})
	if status == model.ArticleDraft || status == model.ArticlePublished {
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Article
	if err := query.Order("sort_order ASC, id ASC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminGet 读取单篇文章（含草稿）。
func (s *ArticleService) AdminGet(id uint) (*model.Article, error) {
	var item model.Article
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("文章不存在")
		}
		return nil, err
	}
	return &item, nil
}

// Create 创建文章。
func (s *ArticleService) Create(in ArticleInput) (*model.Article, error) {
	if err := s.validate(&in, 0); err != nil {
		return nil, err
	}
	item := model.Article{
		Slug:      in.Slug,
		Title:     in.Title,
		ContentMD: in.ContentMD,
		Status:    in.Status,
		SortOrder: in.SortOrder,
	}
	if err := s.db.Create(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrConflict("文章标识已存在")
		}
		return nil, err
	}
	return &item, nil
}

// Update 更新文章。
func (s *ArticleService) Update(id uint, in ArticleInput) (*model.Article, error) {
	var item model.Article
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("文章不存在")
		}
		return nil, err
	}
	if err := s.validate(&in, id); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"slug":       in.Slug,
		"title":      in.Title,
		"content_md": in.ContentMD,
		"status":     in.Status,
		"sort_order": in.SortOrder,
	}
	if err := s.db.Model(&item).Updates(updates).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrConflict("文章标识已存在")
		}
		return nil, err
	}
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// Delete 删除文章，仍被商品引用为协议时拒绝。
func (s *ArticleService) Delete(id uint) error {
	var count int64
	if err := s.db.Model(&model.Product{}).Where("agreement_article_id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrConflict("该文章仍被商品引用为购买协议，请先解除引用")
	}
	result := s.db.Delete(&model.Article{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("文章不存在")
	}
	return nil
}

// validateArticle 校验并补齐文章入参。
func (s *ArticleService) validate(in *ArticleInput, selfID uint) error {
	in.Slug = strings.TrimSpace(in.Slug)
	if in.Slug == "" {
		return ErrBadRequest("文章标识不能为空")
	}
	if len([]rune(in.Slug)) > 64 {
		return ErrBadRequest("文章标识过长")
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return ErrBadRequest("文章标题不能为空")
	}
	if len([]rune(in.Title)) > 128 {
		return ErrBadRequest("文章标题过长")
	}
	if in.Status == "" {
		in.Status = model.ArticleDraft
	}
	if in.Status != model.ArticleDraft && in.Status != model.ArticlePublished {
		return ErrBadRequest("无效的文章状态")
	}
	var count int64
	query := s.db.Model(&model.Article{}).Where("slug = ?", in.Slug)
	if selfID != 0 {
		query = query.Where("id <> ?", selfID)
	}
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrConflict("文章标识已存在")
	}
	return nil
}
