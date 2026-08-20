package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/storage"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// AdminService 提供管理后台的用户、分组与商品管理。
type AdminService struct {
	db     *gorm.DB
	wallet *WalletService
	// store 用于删除用户时清理其上传的附件与证件照。
	store *storage.Store
	// plugins 用于把上游服务的暂停/恢复/删除同步到上游。可为 nil。
	plugins *plugin.Manager
}

// NewAdminService 构造 AdminService。
func NewAdminService(db *gorm.DB, wallet *WalletService, store *storage.Store, plugins *plugin.Manager) *AdminService {
	return &AdminService{db: db, wallet: wallet, store: store, plugins: plugins}
}

// ---------- 用户管理 ----------

// Users 分页返回用户，keyword 可按用户名或邮箱模糊匹配。
func (s *AdminService) Users(keyword string, offset, limit int) ([]model.User, int64, error) {
	query := s.db.Model(&model.User{})
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		like := "%" + keyword + "%"
		query = query.Where("username LIKE ? OR email LIKE ?", like, like)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.User
	if err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// CreateUserRequest 是管理员创建用户的入参。
type CreateUserRequest struct {
	Username     string `json:"username"`
	Email        string `json:"email"`
	Password     string `json:"password"`
	Role         string `json:"role"`
	BalanceCents int64  `json:"balance_cents"`
}

// CreateUser 由管理员创建用户，可指定角色与初始余额。
func (s *AdminService) CreateUser(req CreateUserRequest) (*model.User, error) {
	req.Username = strings.TrimSpace(req.Username)
	if err := ValidateUsername(req.Username); err != nil {
		return nil, err
	}
	email, err := ValidateEmail(req.Email)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}
	role := req.Role
	if role == "" {
		role = model.RoleUser
	}
	if role != model.RoleUser && role != model.RoleAdmin {
		return nil, ErrBadRequest("无效的角色")
	}
	if req.BalanceCents < 0 {
		return nil, ErrBadRequest("初始余额不能为负")
	}

	var count int64
	if err := s.db.Model(&model.User{}).
		Where("username = ? OR email = ?", req.Username, email).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrConflict("用户名或邮箱已被使用")
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	user := model.User{
		Username:     req.Username,
		Email:        email,
		PasswordHash: hash,
		Role:         role,
		Status:       model.UserActive,
	}

	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrConflict("用户名或邮箱已被使用")
			}
			return err
		}
		// 初始余额也要留下流水，保证余额与流水始终对得上。
		if req.BalanceCents > 0 {
			_, err := s.wallet.adjustBalance(tx, user.ID, req.BalanceCents,
				model.TxAdjust, "admin", 0, "管理员设置初始余额")
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	user.BalanceCents = req.BalanceCents
	return &user, nil
}

// UpdateUserRequest 是管理员更新用户的入参。指针字段为 nil 表示不修改。
type UpdateUserRequest struct {
	Username     *string `json:"username"`
	Email        *string `json:"email"`
	Password     *string `json:"password"`
	Role         *string `json:"role"`
	Status       *string `json:"status"`
	BalanceCents *int64  `json:"balance_cents"`
}

// UpdateUser 更新用户资料。余额修改走流水，不直接改字段。
//
// operatorID 是执行操作的管理员，用于阻止其自我降权或自我禁用 —— 否则可能
// 出现系统中没有任何可用管理员的死局。
func (s *AdminService) UpdateUser(operatorID, userID uint, req UpdateUserRequest) (*model.User, error) {
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("用户不存在")
		}
		return nil, err
	}

	updates := map[string]any{}

	if req.Username != nil {
		name := strings.TrimSpace(*req.Username)
		if err := ValidateUsername(name); err != nil {
			return nil, err
		}
		if name != user.Username {
			var count int64
			if err := s.db.Model(&model.User{}).
				Where("username = ? AND id <> ?", name, userID).Count(&count).Error; err != nil {
				return nil, err
			}
			if count > 0 {
				return nil, ErrConflict("用户名已被使用")
			}
			updates["username"] = name
		}
	}

	if req.Email != nil {
		email, err := ValidateEmail(*req.Email)
		if err != nil {
			return nil, err
		}
		if email != user.Email {
			var count int64
			if err := s.db.Model(&model.User{}).
				Where("email = ? AND id <> ?", email, userID).Count(&count).Error; err != nil {
				return nil, err
			}
			if count > 0 {
				return nil, ErrConflict("邮箱已被使用")
			}
			updates["email"] = email
		}
	}

	if req.Password != nil {
		if err := ValidatePassword(*req.Password); err != nil {
			return nil, err
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			return nil, err
		}
		updates["password_hash"] = hash
	}

	if req.Role != nil {
		role := *req.Role
		if role != model.RoleUser && role != model.RoleAdmin {
			return nil, ErrBadRequest("无效的角色")
		}
		if userID == operatorID && role != model.RoleAdmin {
			return nil, ErrBadRequest("不能移除自己的管理员权限")
		}
		if user.IsAdmin() && role != model.RoleAdmin {
			if err := s.ensureAnotherAdmin(userID); err != nil {
				return nil, err
			}
		}
		updates["role"] = role
	}

	if req.Status != nil {
		status := *req.Status
		if status != model.UserActive && status != model.UserDisabled {
			return nil, ErrBadRequest("无效的状态")
		}
		if userID == operatorID && status != model.UserActive {
			return nil, ErrBadRequest("不能禁用自己的账号")
		}
		updates["status"] = status
	}

	// 余额单独处理：目标值与当前值的差额记为一笔 adjust 流水。
	var delta int64
	if req.BalanceCents != nil {
		if *req.BalanceCents < 0 {
			return nil, ErrBadRequest("余额不能为负")
		}
		delta = *req.BalanceCents - user.BalanceCents
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		if len(updates) > 0 {
			if err := tx.Model(&model.User{}).Where("id = ?", userID).Updates(updates).Error; err != nil {
				return err
			}
		}
		if delta != 0 {
			note := fmt.Sprintf("管理员调整余额（%+d 分）", delta)
			if _, err := s.wallet.adjustBalance(tx, userID, delta,
				model.TxAdjust, "admin", operatorID, note); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := s.db.First(&user, userID).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// DeleteUser 删除用户。
func (s *AdminService) DeleteUser(operatorID, userID uint) error {
	if operatorID == userID {
		return ErrBadRequest("不能删除自己的账号")
	}
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("用户不存在")
		}
		return err
	}
	if user.IsAdmin() {
		if err := s.ensureAnotherAdmin(userID); err != nil {
			return err
		}
	}

	// 用户的订单、账单、服务、流水、工单与实名材料一并清理，避免留下孤儿数据。
	//
	// 磁盘文件不能在事务里删：事务回滚而文件已 unlink，就成了「行还在、文件
	// 没了」，那是用户能看见的错误。所以事务内只收集路径，提交成功后再删。
	// 反向的残留（事务失败留下无人引用的文件）无害。
	var filePaths []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		tickets := NewTicketService(tx, s.store)
		attachmentPaths, err := tickets.UserAttachmentPaths(tx, userID)
		if err != nil {
			return err
		}
		photoPaths, err := NewKYCService(tx, s.store).UserPhotoPaths(tx, userID)
		if err != nil {
			return err
		}
		filePaths = append(append(filePaths, attachmentPaths...), photoPaths...)

		if err := tickets.DeleteUserTickets(tx, userID); err != nil {
			return err
		}
		if err := NewAPIKeyService(tx).DeleteUserKeys(tx, userID); err != nil {
			return err
		}

		var orderIDs []uint
		if err := tx.Model(&model.Order{}).Where("user_id = ?", userID).
			Pluck("id", &orderIDs).Error; err != nil {
			return err
		}
		if len(orderIDs) > 0 {
			if err := tx.Where("order_id IN ?", orderIDs).Delete(&model.OrderItem{}).Error; err != nil {
				return err
			}
		}
		var invoiceIDs []uint
		if err := tx.Model(&model.Invoice{}).Where("user_id = ?", userID).
			Pluck("id", &invoiceIDs).Error; err != nil {
			return err
		}
		if len(invoiceIDs) > 0 {
			if err := tx.Where("invoice_id IN ?", invoiceIDs).Delete(&model.InvoiceItem{}).Error; err != nil {
				return err
			}
		}
		for _, target := range []any{
			&model.CartItem{}, &model.Order{}, &model.Invoice{},
			&model.Service{}, &model.Transaction{}, &model.Verification{},
		} {
			if err := tx.Where("user_id = ?", userID).Delete(target).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&model.User{}, userID).Error
	})
	if err != nil {
		return err
	}
	// 文件删不掉只是留了垃圾，不该让「用户已删除」这个结果失败。
	s.store.RemoveAll(filePaths)
	return nil
}

// ensureAnotherAdmin 确认除 excludeID 外还存在其他启用中的管理员。
func (s *AdminService) ensureAnotherAdmin(excludeID uint) error {
	var count int64
	err := s.db.Model(&model.User{}).
		Where("role = ? AND status = ? AND id <> ?", model.RoleAdmin, model.UserActive, excludeID).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrBadRequest("系统必须保留至少一名管理员")
	}
	return nil
}

// ---------- 分组管理 ----------

// Categories 返回全部分组（平铺，按排序值）。
func (s *AdminService) Categories() ([]model.ProductCategory, error) {
	var items []model.ProductCategory
	if err := s.db.Order("sort ASC, id ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// CreateCategory 创建分组。
func (s *AdminService) CreateCategory(in CategoryInput) (*model.ProductCategory, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, ErrBadRequest("分组名称不能为空")
	}
	if err := s.validateParent(in.ParentID, 0); err != nil {
		return nil, err
	}
	slug, err := s.uniqueSlug(in.Slug, in.Name, 0)
	if err != nil {
		return nil, err
	}

	item := model.ProductCategory{
		ParentID:    in.ParentID,
		Name:        in.Name,
		Slug:        slug,
		Description: strings.TrimSpace(in.Description),
		Sort:        in.Sort,
	}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// UpdateCategory 更新分组。
func (s *AdminService) UpdateCategory(id uint, in CategoryInput) (*model.ProductCategory, error) {
	var item model.ProductCategory
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("分组不存在")
		}
		return nil, err
	}

	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, ErrBadRequest("分组名称不能为空")
	}
	if err := s.validateParent(in.ParentID, id); err != nil {
		return nil, err
	}
	slug, err := s.uniqueSlug(in.Slug, in.Name, id)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{
		"parent_id":   in.ParentID,
		"name":        in.Name,
		"slug":        slug,
		"description": strings.TrimSpace(in.Description),
		"sort":        in.Sort,
	}
	if err := s.db.Model(&item).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteCategory 删除分组。存在子分组或商品时拒绝。
func (s *AdminService) DeleteCategory(id uint) error {
	var children int64
	if err := s.db.Model(&model.ProductCategory{}).
		Where("parent_id = ?", id).Count(&children).Error; err != nil {
		return err
	}
	if children > 0 {
		return ErrConflict("该分组下还有子分组，请先删除子分组")
	}
	var products int64
	if err := s.db.Model(&model.Product{}).
		Where("category_id = ?", id).Count(&products).Error; err != nil {
		return err
	}
	if products > 0 {
		return ErrConflict("该分组下还有商品，请先移除或删除商品")
	}

	result := s.db.Delete(&model.ProductCategory{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("分组不存在")
	}
	return nil
}

// validateParent 校验父分组：必须存在、不能是自己、且父级本身不能再有父级
// （限制为两级结构）。
func (s *AdminService) validateParent(parentID *uint, selfID uint) error {
	if parentID == nil {
		return nil
	}
	if *parentID == 0 {
		return ErrBadRequest("无效的父分组")
	}
	if selfID != 0 && *parentID == selfID {
		return ErrBadRequest("分组不能以自己作为父分组")
	}
	var parent model.ProductCategory
	if err := s.db.First(&parent, *parentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBadRequest("父分组不存在")
		}
		return err
	}
	if parent.ParentID != nil {
		return ErrBadRequest("分组层级最多两级")
	}
	// 自己若已有子分组，就不能再挂到别人下面，否则会变成三级。
	if selfID != 0 {
		var children int64
		if err := s.db.Model(&model.ProductCategory{}).
			Where("parent_id = ?", selfID).Count(&children).Error; err != nil {
			return err
		}
		if children > 0 {
			return ErrBadRequest("该分组下已有子分组，不能再设置父分组")
		}
	}
	return nil
}

// uniqueSlug 生成不与他人冲突的 slug。
//
// 纯中文名称经 normalizeSlug 后会变成空串（无法音译），此时用名称的短哈希
// 兜底，得到形如 cat-3f2a1b 的稳定标识。若退回固定前缀，多个中文分组就会
// 挤在 category、category-2、category-3 这样毫无信息量的序列上。
func (s *AdminService) uniqueSlug(raw, fallback string, selfID uint) (string, error) {
	base := normalizeSlug(raw, fallback)
	if base == "" {
		sum := sha256.Sum256([]byte(fallback))
		base = "cat-" + hex.EncodeToString(sum[:3])
	}
	candidate := base
	for i := 2; ; i++ {
		var count int64
		query := s.db.Model(&model.ProductCategory{}).Where("slug = ?", candidate)
		if selfID != 0 {
			query = query.Where("id <> ?", selfID)
		}
		if err := query.Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		if i > 1000 {
			return "", ErrConflict("无法生成唯一的分组标识")
		}
	}
}

// ---------- 商品管理 ----------

// Products 分页返回全部商品（含隐藏商品）。
func (s *AdminService) Products(categoryID uint, offset, limit int) ([]model.Product, int64, error) {
	query := s.db.Model(&model.Product{})
	if categoryID != 0 {
		query = query.Where("category_id = ?", categoryID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Product
	err := query.Order("sort ASC, id ASC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// CreateProduct 创建商品。
func (s *AdminService) CreateProduct(in ProductInput) (*model.Product, error) {
	if err := s.validateProduct(&in); err != nil {
		return nil, err
	}
	item := model.Product{
		CategoryID:        in.CategoryID,
		Name:              in.Name,
		Description:       strings.TrimSpace(in.Description),
		Specs:             in.Specs,
		PriceCents:        in.PriceCents,
		BillingCyc:        in.BillingCyc,
		Stock:             in.Stock,
		Status:            in.Status,
		Sort:              in.Sort,
		UpstreamPluginID:  in.UpstreamPluginID,
		UpstreamProductID: in.UpstreamProductID,
	}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// UpdateProduct 更新商品。
func (s *AdminService) UpdateProduct(id uint, in ProductInput) (*model.Product, error) {
	var item model.Product
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("商品不存在")
		}
		return nil, err
	}
	if err := s.validateProduct(&in); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"category_id":         in.CategoryID,
		"name":                in.Name,
		"description":         strings.TrimSpace(in.Description),
		"specs":               in.Specs,
		"price_cents":         in.PriceCents,
		"billing_cycle":       in.BillingCyc,
		"stock":               in.Stock,
		"status":              in.Status,
		"sort":                in.Sort,
		"upstream_plugin_id":  in.UpstreamPluginID,
		"upstream_product_id": in.UpstreamProductID,
	}
	if err := s.db.Model(&item).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteProduct 删除商品。已有服务实例引用时拒绝，保留历史数据完整性。
func (s *AdminService) DeleteProduct(id uint) error {
	var services int64
	if err := s.db.Model(&model.Service{}).
		Where("product_id = ?", id).Count(&services).Error; err != nil {
		return err
	}
	if services > 0 {
		return ErrConflict("该商品已有用户购买，建议改为隐藏而不是删除")
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		// 购物车里的引用可以安全清掉，不涉及历史账目。
		if err := tx.Where("product_id = ?", id).Delete(&model.CartItem{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&model.Product{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound("商品不存在")
		}
		return nil
	})
}

// maxSpecs 限制单个商品的规格行数，避免前端表单被滥用撑爆卡片。
const maxSpecs = 20

// validateProduct 校验并补齐商品入参。
func (s *AdminService) validateProduct(in *ProductInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return ErrBadRequest("商品名称不能为空")
	}
	if in.PriceCents < 0 {
		return ErrBadRequest("价格不能为负")
	}
	if err := normalizeSpecs(&in.Specs); err != nil {
		return err
	}
	if in.BillingCyc == "" {
		in.BillingCyc = model.CycleMonthly
	}
	if !model.ValidCycle(in.BillingCyc) {
		return ErrBadRequest("无效的计费周期")
	}
	if in.Status == "" {
		in.Status = model.ProductActive
	}
	if in.Status != model.ProductActive && in.Status != model.ProductHidden {
		return ErrBadRequest("无效的商品状态")
	}
	if in.CategoryID == 0 {
		return ErrBadRequest("请选择所属分组")
	}

	var category model.ProductCategory
	if err := s.db.First(&category, in.CategoryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBadRequest("所属分组不存在")
		}
		return err
	}
	return nil
}

// normalizeSpecs 清理规格列表：去空白、丢弃空行、限制条数。
//
// 前端的规格表单常留下空白行，直接入库会在卡片上渲染出空规格，
// 因此在这里统一丢掉，而不是要求前端保证干净。
func normalizeSpecs(specs *model.SpecList) error {
	if len(*specs) == 0 {
		*specs = nil
		return nil
	}
	out := make(model.SpecList, 0, len(*specs))
	for _, spec := range *specs {
		label := strings.TrimSpace(spec.Label)
		value := strings.TrimSpace(spec.Value)
		if label == "" && value == "" {
			continue
		}
		if label == "" || value == "" {
			return ErrBadRequest("规格的名称与内容都不能为空")
		}
		if len([]rune(label)) > 32 || len([]rune(value)) > 64 {
			return ErrBadRequest("规格内容过长")
		}
		out = append(out, model.Spec{Label: label, Value: value})
	}
	if len(out) > maxSpecs {
		return ErrBadRequest("规格最多 %d 条", maxSpecs)
	}
	if len(out) == 0 {
		out = nil
	}
	*specs = out
	return nil
}

// ---------- 服务管理 ----------

// UserServices 分页返回某用户的已购服务。
func (s *AdminService) UserServices(userID uint, offset, limit int) ([]model.Service, int64, error) {
	var (
		items []model.Service
		total int64
	)
	if err := s.db.Model(&model.Service{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := s.db.Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// SetServiceStatus 修改服务状态。仅允许「使用中」与「暂停」之间切换，
// 已终止的服务不在这里复活。上游服务会同步暂停/恢复到上游。
func (s *AdminService) SetServiceStatus(serviceID uint, status string) (*model.Service, error) {
	if status != model.ServiceActive && status != model.ServiceSuspended {
		return nil, ErrBadRequest("无效的服务状态")
	}
	var item model.Service
	if err := s.db.First(&item, serviceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	if item.Status == status {
		return &item, nil
	}

	// 上游服务：先把状态变更同步到上游，失败则本地不变更。
	if item.UpstreamPluginID != "" && item.UpstreamHostID != "" {
		action := pb.HostAction_HOST_ACTION_UNSUSPEND
		if status == model.ServiceSuspended {
			action = pb.HostAction_HOST_ACTION_SUSPEND
		}
		if err := s.manageUpstream(&item, action); err != nil {
			return nil, err
		}
	}

	if err := s.db.Model(&item).Update("status", status).Error; err != nil {
		return nil, err
	}
	item.Status = status
	return &item, nil
}

// DeleteService 删除用户的服务。账单明细仍要保留历史金额，因此先把它们对
// 本服务的引用置空，再删除服务本身，避免留下悬空的 service_id。
// 上游服务会先向上游发起删除（终止），失败则本地不删除。
func (s *AdminService) DeleteService(serviceID uint) error {
	var item model.Service
	if err := s.db.First(&item, serviceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("服务不存在")
		}
		return err
	}

	// 上游服务：先向上游终止，失败则本地保留，避免两边不一致。
	if item.UpstreamPluginID != "" && item.UpstreamHostID != "" {
		if err := s.manageUpstream(&item, pb.HostAction_HOST_ACTION_TERMINATE); err != nil {
			return err
		}
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.InvoiceItem{}).
			Where("service_id = ?", serviceID).
			Update("service_id", nil).Error; err != nil {
			return err
		}
		result := tx.Delete(&model.Service{}, serviceID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound("服务不存在")
		}
		return nil
	})
}

// manageUpstream 对上游服务实例执行管理动作，失败返回业务错误。
func (s *AdminService) manageUpstream(svc *model.Service, action pb.HostAction) error {
	if s.plugins == nil {
		return ErrBadRequest("上游插件不可用，无法操作该服务")
	}
	reply, err := s.plugins.ManageHost(context.Background(), svc.UpstreamPluginID, &pb.ManageHostRequest{
		HostId: svc.UpstreamHostID,
		Action: action,
	})
	if err != nil {
		return ErrBadRequest("上游操作失败: %v", err)
	}
	if !reply.GetSuccess() {
		return ErrBadRequest("上游操作失败")
	}
	return nil
}

// ---------- 财务：支付方式 ----------

type PaymentMethodInput struct {
	Name     string            `json:"name"`
	PluginID string            `json:"plugin_id"`
	Config   map[string]string `json:"config"`
	Enabled  *bool             `json:"enabled"`
	Sort     int               `json:"sort_order"`
}

type PaymentPluginField struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Hint         string   `json:"hint"`
	Type         string   `json:"type"`
	Required     bool     `json:"required"`
	Secret       bool     `json:"secret"`
	DefaultValue string   `json:"default_value"`
	Options      []Option `json:"options,omitempty"`
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type PaymentPluginInfo struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Config []PaymentPluginField `json:"config"`
}

func paymentPluginFields(fields []*pb.ConfigField) []PaymentPluginField {
	out := make([]PaymentPluginField, 0, len(fields))
	for _, f := range fields {
		item := PaymentPluginField{Key: f.GetKey(), Label: f.GetLabel(), Hint: f.GetHint(), Type: fieldTypeName(f.GetType()), Required: f.GetRequired(), Secret: f.GetSecret(), DefaultValue: f.GetDefaultValue()}
		if len(f.GetOptions()) > 0 {
			for _, o := range f.GetOptions() {
				item.Options = append(item.Options, Option{Value: o.GetValue(), Label: o.GetLabel()})
			}
		}
		out = append(out, item)
	}
	return out
}

// PaymentPlugins 返回可用的支付插件及其按方式配置 schema。
func (s *AdminService) PaymentPlugins() ([]PaymentPluginInfo, error) {
	if s.plugins == nil {
		return []PaymentPluginInfo{}, nil
	}
	ids := s.plugins.PaymentPlugins()
	out := make([]PaymentPluginInfo, 0, len(ids))
	for _, id := range ids {
		inst, err := s.plugins.Get(id)
		if err != nil {
			continue
		}
		m := inst.Manifest()
		if m == nil {
			out = append(out, PaymentPluginInfo{ID: id, Name: inst.Snapshot().Name, Config: []PaymentPluginField{}})
			continue
		}
		fields := m.GetPaymentConfig()
		if len(fields) == 0 {
			// 兼容旧插件：若 payment_config 为空则回落到全局 config（epay 旧版）
			fields = m.GetConfig()
		}
		out = append(out, PaymentPluginInfo{ID: id, Name: m.GetName(), Config: paymentPluginFields(fields)})
	}
	return out, nil
}

func (s *AdminService) PaymentMethods() ([]model.PaymentMethod, error) {
	var items []model.PaymentMethod
	if err := s.db.Order("sort_order ASC, id ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func (s *AdminService) CreatePaymentMethod(in PaymentMethodInput) (*model.PaymentMethod, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, ErrBadRequest("支付方式名称不能为空")
	}
	if len(name) > 64 {
		return nil, ErrBadRequest("名称过长")
	}
	pluginID := strings.TrimSpace(in.PluginID)
	if pluginID == "" {
		return nil, ErrBadRequest("请选择支付接口")
	}
	if s.plugins != nil {
		found := false
		for _, id := range s.plugins.PaymentPlugins() {
			if id == pluginID {
				found = true
				break
			}
		}
		if !found {
			return nil, ErrBadRequest("所选支付接口不可用")
		}
	}
	configJSON := "{}"
	if in.Config != nil {
		trimmed := map[string]string{}
		for k, v := range in.Config {
			trimmed[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		b, _ := json.Marshal(trimmed)
		configJSON = string(b)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	item := &model.PaymentMethod{Name: name, PluginID: pluginID, Config: configJSON, Enabled: enabled, SortOrder: in.Sort}
	if err := s.db.Create(item).Error; err != nil {
		return nil, err
	}
	return item, nil
}

func (s *AdminService) UpdatePaymentMethod(id uint, in PaymentMethodInput) (*model.PaymentMethod, error) {
	var item model.PaymentMethod
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("支付方式不存在")
		}
		return nil, err
	}
	updates := map[string]any{}
	if in.Name != "" {
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return nil, ErrBadRequest("支付方式名称不能为空")
		}
		updates["name"] = name
	}
	if in.PluginID != "" {
		pluginID := strings.TrimSpace(in.PluginID)
		if s.plugins != nil {
			found := false
			for _, pid := range s.plugins.PaymentPlugins() {
				if pid == pluginID {
					found = true
					break
				}
			}
			if !found {
				return nil, ErrBadRequest("所选支付接口不可用")
			}
		}
		updates["plugin_id"] = pluginID
	}
	if in.Config != nil {
		trimmed := map[string]string{}
		for k, v := range in.Config {
			trimmed[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		b, _ := json.Marshal(trimmed)
		updates["config"] = string(b)
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	updates["sort_order"] = in.Sort
	if len(updates) > 0 {
		if err := s.db.Model(&item).Updates(updates).Error; err != nil {
			return nil, err
		}
	}
	if err := s.db.First(&item, id).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *AdminService) DeletePaymentMethod(id uint) error {
	result := s.db.Delete(&model.PaymentMethod{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("支付方式不存在")
	}
	return nil
}

// ---------- 管理员手动服务 ----------

type AdminCreateServiceRequest struct {
	ProductID    uint   `json:"product_id"`
	Quantity     int    `json:"quantity"`
	BillingCycle string `json:"billing_cycle"`
	Provision    bool   `json:"provision"`
}

func (s *AdminService) CreateServiceForUser(userID uint, req AdminCreateServiceRequest) (*model.Service, error) {
	if req.ProductID == 0 {
		return nil, ErrBadRequest("请选择商品")
	}
	qty := req.Quantity
	if qty < 1 {
		qty = 1
	}
	if qty > 100 {
		return nil, ErrBadRequest("数量过大")
	}
	var product model.Product
	if err := s.db.First(&product, req.ProductID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("商品不存在")
		}
		return nil, err
	}
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("用户不存在")
		}
		return nil, err
	}
	cycle := req.BillingCycle
	if cycle == "" {
		cycle = product.BillingCyc
	}
	if !model.ValidCycle(cycle) {
		cycle = product.BillingCyc
	}
	services := make([]*model.Service, 0, qty)
	var last *model.Service
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < qty; i++ {
			svc := &model.Service{
				UserID:     userID,
				ProductID:  product.ID,
				OrderID:    0,
				Name:       product.Name,
				Status:     model.ServiceActive,
				BillingCyc: cycle,
				PriceCents: product.PriceCents,
			}
			if next := model.AdvanceCycle(time.Now().UTC(), cycle); !next.IsZero() {
				svc.NextDueAt = &next
				svc.ExpiresAt = &next
			}
			if req.Provision && product.UpstreamPluginID != "" && product.UpstreamProductID != "" {
				if s.plugins == nil {
					return ErrBadRequest("上游插件不可用，无法开通")
				}
				// 调用上游开通
				hostID, expiry, err := s.provisionUpstreamForAdmin(tx, &user, &product, cycle)
				if err != nil {
					return err
				}
				svc.UpstreamPluginID = product.UpstreamPluginID
				svc.UpstreamHostID = hostID
				if expiry != nil {
					svc.NextDueAt = expiry
					svc.ExpiresAt = expiry
				}
			}
			if err := tx.Create(svc).Error; err != nil {
				return err
			}
			services = append(services, svc)
			last = svc
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(services) == 1 {
		return services[0], nil
	}
	return last, nil
}

func (s *AdminService) provisionUpstreamForAdmin(tx *gorm.DB, user *model.User, product *model.Product, cycle string) (string, *time.Time, error) {
	reply, err := s.plugins.CreateOrder(context.Background(), product.UpstreamPluginID, &pb.CreateOrderRequest{
		ProductId:    product.UpstreamProductID,
		BillingCycle: cycle,
		Quantity:     1,
		ClientEmail:  user.Email,
		Remark:       fmt.Sprintf("admin-create user=%d", user.ID),
	})
	if err != nil {
		return "", nil, ErrBadRequest("上游开通失败: %v", err)
	}
	hostID := reply.GetUpstreamOrderId()
	if hostID == "" {
		return "", nil, ErrBadRequest("上游开通失败: 未返回服务实例 ID")
	}
	var expiry *time.Time
	if host, err := s.plugins.GetHost(context.Background(), product.UpstreamPluginID, &pb.GetHostRequest{HostId: hostID}); err == nil {
		if e := host.GetHost().GetExpiry(); e != "" {
			if t, err := time.Parse(time.RFC3339, e); err == nil {
				expiry = &t
			}
		}
	}
	return hostID, expiry, nil
}

type AdminBindServiceRequest struct {
	UpstreamPluginID string `json:"upstream_plugin_id"`
	UpstreamHostID   string `json:"upstream_host_id"`
}

func (s *AdminService) BindServiceUpstream(serviceID uint, req AdminBindServiceRequest) (*model.Service, error) {
	var svc model.Service
	if err := s.db.First(&svc, serviceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	pluginID := strings.TrimSpace(req.UpstreamPluginID)
	hostID := strings.TrimSpace(req.UpstreamHostID)
	// 解绑：两者都为空表示清除绑定
	if pluginID == "" && hostID == "" {
		if err := s.db.Model(&svc).Updates(map[string]any{"upstream_plugin_id": "", "upstream_host_id": ""}).Error; err != nil {
			return nil, err
		}
		svc.UpstreamPluginID = ""
		svc.UpstreamHostID = ""
		return &svc, nil
	}
	if pluginID == "" || hostID == "" {
		return nil, ErrBadRequest("请同时提供上游插件和主机 ID，或都留空以解绑")
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("上游插件不可用")
	}
	inst, err := s.plugins.Get(pluginID)
	if err != nil || !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT) {
		return nil, ErrBadRequest("上游插件不可用或不支持对接")
	}
	if inst.Client() == nil {
		return nil, ErrBadRequest("上游插件未运行")
	}
	// 验证上游主机存在
	if _, err := s.plugins.GetHost(context.Background(), pluginID, &pb.GetHostRequest{HostId: hostID}); err != nil {
		return nil, ErrBadRequest("上游主机不存在: %v", err)
	}
	if err := s.db.Model(&svc).Updates(map[string]any{"upstream_plugin_id": pluginID, "upstream_host_id": hostID}).Error; err != nil {
		return nil, err
	}
	svc.UpstreamPluginID = pluginID
	svc.UpstreamHostID = hostID
	return &svc, nil
}

// ---------- 概览 ----------

// Stats 是管理后台的概览数据。
type Stats struct {
	UserCount    int64 `json:"user_count"`
	ProductCount int64 `json:"product_count"`
	OrderCount   int64 `json:"order_count"`
	ServiceCount int64 `json:"service_count"`
	RevenueCents int64 `json:"revenue_cents"`
}

// Stats 汇总管理后台首屏数据。
func (s *AdminService) Stats() (*Stats, error) {
	var out Stats
	counts := []struct {
		model any
		into  *int64
	}{
		{&model.User{}, &out.UserCount},
		{&model.Product{}, &out.ProductCount},
		{&model.Order{}, &out.OrderCount},
		{&model.Service{}, &out.ServiceCount},
	}
	for _, item := range counts {
		if err := s.db.Model(item.model).Count(item.into).Error; err != nil {
			return nil, err
		}
	}
	err := s.db.Model(&model.Order{}).Where("status = ?", model.OrderPaid).
		Select("COALESCE(SUM(total_cents), 0)").Scan(&out.RevenueCents).Error
	if err != nil {
		return nil, err
	}
	return &out, nil
}
