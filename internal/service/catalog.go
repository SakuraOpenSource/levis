package service

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// CatalogService 提供商品分组与商品的读取。
type CatalogService struct {
	db *gorm.DB
}

// NewCatalogService 构造 CatalogService。
func NewCatalogService(db *gorm.DB) *CatalogService {
	return &CatalogService{db: db}
}

// Tree 返回两级分组树，并把上架商品挂到所属分组下。
//
// 结构形如「香港（大类）→ HK1 / HK2（小类）→ 商品」。整体一次性读出后在内存
// 组装，避免 N+1 查询。
func (s *CatalogService) Tree() ([]model.ProductCategory, error) {
	var categories []model.ProductCategory
	if err := s.db.Order("sort ASC, id ASC").Find(&categories).Error; err != nil {
		return nil, err
	}
	var products []model.Product
	if err := s.db.Where("status = ?", model.ProductActive).
		Order("sort ASC, id ASC").Find(&products).Error; err != nil {
		return nil, err
	}

	byCategory := make(map[uint][]model.Product, len(categories))
	for _, p := range products {
		byCategory[p.CategoryID] = append(byCategory[p.CategoryID], p)
	}

	// 先建索引，再按 ParentID 挂载，这样不依赖记录的返回顺序。
	index := make(map[uint]*model.ProductCategory, len(categories))
	for i := range categories {
		categories[i].Products = byCategory[categories[i].ID]
		index[categories[i].ID] = &categories[i]
	}

	var roots []model.ProductCategory
	for i := range categories {
		node := &categories[i]
		if node.ParentID == nil {
			continue
		}
		if parent, ok := index[*node.ParentID]; ok {
			parent.Children = append(parent.Children, *node)
		}
	}
	for i := range categories {
		if categories[i].ParentID == nil {
			roots = append(roots, categories[i])
		}
	}
	return roots, nil
}

// Products 返回上架商品，category_id 为 0 时不过滤分组。
func (s *CatalogService) Products(categoryID uint) ([]model.Product, error) {
	query := s.db.Where("status = ?", model.ProductActive)
	if categoryID != 0 {
		query = query.Where("category_id = ?", categoryID)
	}
	var items []model.Product
	if err := query.Order("sort ASC, id ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Product 按 ID 读取上架商品。
func (s *CatalogService) Product(id uint) (*model.Product, error) {
	var item model.Product
	err := s.db.First(&item, "id = ? AND status = ?", id, model.ProductActive).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("商品不存在或已下架")
		}
		return nil, err
	}
	return &item, nil
}

// CategoryInput 是分组的创建/更新入参。
type CategoryInput struct {
	ParentID    *uint  `json:"parent_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Sort        int    `json:"sort"`
}

// ProductInput 是商品的创建/更新入参。
type ProductInput struct {
	CategoryID  uint           `json:"category_id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Specs       model.SpecList `json:"specs"`
	PriceCents  int64          `json:"price_cents"`
	BillingCyc  string         `json:"billing_cycle"`
	Stock       int            `json:"stock"`
	Status      string         `json:"status"`
	Sort        int            `json:"sort"`
}

// slugPattern 之外的字符会被替换成短横线。
func normalizeSlug(raw, fallback string) string {
	source := strings.TrimSpace(strings.ToLower(raw))
	if source == "" {
		source = strings.TrimSpace(strings.ToLower(fallback))
	}
	var b strings.Builder
	lastDash := false
	for _, r := range source {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			// 中文等非 ASCII 字符无法直接进 slug，统一转成短横线；
			// 若结果为空则由调用方回退到自动编号。
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
