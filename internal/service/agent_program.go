package service

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// AgentProgramService 代理加盟：等级按用户当前余额自动判定，折扣挂在
// 商品分组上（小分组配置覆盖大分组），下单定价时生效。
type AgentProgramService struct {
	db *gorm.DB
}

// NewAgentProgramService 构造 AgentProgramService。
func NewAgentProgramService(db *gorm.DB) *AgentProgramService {
	return &AgentProgramService{db: db}
}

// DB 暴露底层连接（管理端全量重建时用）。
func (s *AgentProgramService) DB() *gorm.DB { return s.db }

// Enabled 报告代理加盟是否开启。
func (s *AgentProgramService) Enabled() bool {
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingAgentProgramEnabled).Error; err != nil {
		return false
	}
	return row.Value == "1"
}

// SetEnabled 开关代理加盟。
func (s *AgentProgramService) SetEnabled(enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	row := model.Setting{Key: model.SettingAgentProgramEnabled, Value: value}
	return s.db.Save(&row).Error
}

// TierWithDiscounts 是管理端读写的等级结构（含其分组折扣）。
type TierWithDiscounts struct {
	model.AgentTier
	Discounts []model.AgentTierDiscount `json:"discounts"`
}

// AgentProgramConfig 是代理加盟的整体配置（管理端一次读写）。
type AgentProgramConfig struct {
	Enabled bool                `json:"enabled"`
	Tiers   []TierWithDiscounts `json:"tiers"`
}

// Config 读取完整配置。
func (s *AgentProgramService) Config() (*AgentProgramConfig, error) {
	cfg := &AgentProgramConfig{Enabled: s.Enabled()}
	var tiers []model.AgentTier
	if err := s.db.Order("sort ASC, min_balance_cents ASC, id ASC").Find(&tiers).Error; err != nil {
		return nil, err
	}
	for _, tier := range tiers {
		item := TierWithDiscounts{AgentTier: tier}
		if err := s.db.Where("tier_id = ?", tier.ID).
			Order("id ASC").Find(&item.Discounts).Error; err != nil {
			return nil, err
		}
		cfg.Tiers = append(cfg.Tiers, item)
	}
	return cfg, nil
}

// TierInput 是等级的创建/更新入参。
type TierInput struct {
	Name            string `json:"name"`
	MinBalanceCents int64  `json:"min_balance_cents"`
	Sort            int    `json:"sort"`
}

// SaveTier 创建或更新等级（id 为 0 表示创建）。
func (s *AgentProgramService) SaveTier(id uint, in TierInput) (*model.AgentTier, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, ErrBadRequest("等级名称不能为空")
	}
	if in.MinBalanceCents < 0 {
		return nil, ErrBadRequest("预存门槛不能为负")
	}
	var tier model.AgentTier
	if id != 0 {
		if err := s.db.First(&tier, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound("等级不存在")
			}
			return nil, err
		}
		tier.Name = in.Name
		tier.MinBalanceCents = in.MinBalanceCents
		tier.Sort = in.Sort
		if err := s.db.Save(&tier).Error; err != nil {
			return nil, err
		}
		return &tier, nil
	}
	tier = model.AgentTier{Name: in.Name, MinBalanceCents: in.MinBalanceCents, Sort: in.Sort}
	if err := s.db.Create(&tier).Error; err != nil {
		return nil, err
	}
	return &tier, nil
}

// DeleteTier 删除等级及其折扣。
func (s *AgentProgramService) DeleteTier(id uint) error {
	if err := s.db.Where("tier_id = ?", id).Delete(&model.AgentTierDiscount{}).Error; err != nil {
		return err
	}
	result := s.db.Delete(&model.AgentTier{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("等级不存在")
	}
	return nil
}

// DiscountInput 是等级折扣的设置入参；DiscountPermille 传 1000 或 0 表示删除。
type DiscountInput struct {
	TierID           uint `json:"tier_id"`
	CategoryID       uint `json:"category_id"`
	DiscountPermille int  `json:"discount_permille"`
}

// SetDiscount 设置/更新/删除一条等级折扣。
func (s *AgentProgramService) SetDiscount(in DiscountInput) error {
	var tier model.AgentTier
	if err := s.db.First(&tier, in.TierID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("等级不存在")
		}
		return err
	}
	var category model.ProductCategory
	if err := s.db.First(&category, in.CategoryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound("商品分组不存在")
		}
		return err
	}
	if in.DiscountPermille != 0 && (in.DiscountPermille < 100 || in.DiscountPermille > 1000) {
		return ErrBadRequest("折扣需在 100‰~1000‰ 之间（800 即 8 折）")
	}
	if in.DiscountPermille == 0 || in.DiscountPermille == 1000 {
		return s.db.Where("tier_id = ? AND category_id = ?", in.TierID, in.CategoryID).
			Delete(&model.AgentTierDiscount{}).Error
	}
	row := model.AgentTierDiscount{
		TierID: in.TierID, CategoryID: in.CategoryID, DiscountPermille: in.DiscountPermille,
	}
	return s.db.Save(&row).Error
}

// TierForUser 判定用户的代理等级：管理员审核绑定的等级优先（预授权），
// 未绑定时按当前余额自动判定。返回 (等级, 是否为绑定)。
func (s *AgentProgramService) TierForUser(userID uint) (*model.AgentTier, bool) {
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		return nil, false
	}
	if user.AgentTierID != nil {
		var bound model.AgentTier
		if err := s.db.First(&bound, *user.AgentTierID).Error; err == nil {
			return &bound, true
		}
	}
	return s.TierForBalance(user.BalanceCents), false
}

// TierForBalance 按余额判定等级（达标最高档），无匹配返回 nil。
func (s *AgentProgramService) TierForBalance(balanceCents int64) *model.AgentTier {
	var tiers []model.AgentTier
	if err := s.db.Order("min_balance_cents ASC").Find(&tiers).Error; err != nil {
		return nil
	}
	var hit *model.AgentTier
	for i := range tiers {
		if balanceCents >= tiers[i].MinBalanceCents {
			hit = &tiers[i]
		}
	}
	return hit
}

// DiscountFor 沿分组父链向上找该等级的折扣，最深（最具体）的命中生效。
// 返回 (千分比, 是否命中)。
func (s *AgentProgramService) DiscountFor(tierID uint, categoryID uint) (int, bool) {
	if tierID == 0 || categoryID == 0 {
		return 0, false
	}
	var discounts []model.AgentTierDiscount
	if err := s.db.Where("tier_id = ?", tierID).Find(&discounts).Error; err != nil || len(discounts) == 0 {
		return 0, false
	}
	byCategory := make(map[uint]int, len(discounts))
	for _, d := range discounts {
		byCategory[d.CategoryID] = d.DiscountPermille
	}
	// 沿父链走，记录最深命中（第一次命中即为最深——从商品所在分组向上）。
	cursor := categoryID
	depth := 0
	for {
		if permille, ok := byCategory[cursor]; ok {
			return permille, true
		}
		var node model.ProductCategory
		if err := s.db.First(&node, cursor).Error; err != nil || node.ParentID == nil {
			return 0, false
		}
		cursor = *node.ParentID
		depth++
		if depth > maxCategoryDepth {
			return 0, false
		}
	}
}

// UserSummary 是用户侧的代理加盟信息。
type UserSummary struct {
	Enabled bool             `json:"enabled"`
	Tier    *model.AgentTier `json:"tier"`
	Next    *model.AgentTier `json:"next_tier"`
	// Bound 表示等级来自管理员审核绑定（预授权），而非余额达标。
	Bound bool `json:"bound"`
	// BalanceCents 是用户当前余额，供前端展示升级进度。
	BalanceCents int64 `json:"balance_cents"`
	// Application 是用户最近一次申请（无申请为 null）。
	Application *model.AgentApplication `json:"application"`
	// Discounts 是该等级生效的分组折扣（管理端配置原样展示）。
	Discounts []model.AgentTierDiscount `json:"discounts"`
}

// Summary 返回用户视角的加盟信息（等级、距下一档差额、生效折扣）。
func (s *AgentProgramService) Summary(userID uint) (*UserSummary, error) {
	out := &UserSummary{Enabled: s.Enabled()}
	if !out.Enabled {
		return out, nil
	}
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		return nil, err
	}
	out.BalanceCents = user.BalanceCents
	out.Tier, out.Bound = s.TierForUser(userID)
	var tiers []model.AgentTier
	if err := s.db.Order("min_balance_cents ASC").Find(&tiers).Error; err != nil {
		return nil, err
	}
	for i := range tiers {
		if out.Tier != nil && tiers[i].ID == out.Tier.ID {
			if i+1 < len(tiers) {
				out.Next = &tiers[i+1]
			}
			break
		}
	}
	if out.Tier != nil {
		if err := s.db.Where("tier_id = ?", out.Tier.ID).
			Order("id ASC").Find(&out.Discounts).Error; err != nil {
			return nil, err
		}
	}
	var latest model.AgentApplication
	if err := s.db.Where("user_id = ?", userID).
		Order("id DESC").First(&latest).Error; err == nil {
		out.Application = &latest
	}
	return out, nil
}

// ApplyInput 是代理申请入参。
type ApplyInput struct {
	TierID  uint   `json:"tier_id"`
	Contact string `json:"contact"`
	Remark  string `json:"remark"`
}

// Apply 提交代理申请。已有待审申请时拒绝重复提交。
func (s *AgentProgramService) Apply(userID uint, in ApplyInput) (*model.AgentApplication, error) {
	if !s.Enabled() {
		return nil, ErrBadRequest("代理加盟未开放")
	}
	in.Contact = strings.TrimSpace(in.Contact)
	if in.TierID == 0 {
		return nil, ErrBadRequest("请选择申请的代理等级")
	}
	if in.Contact == "" {
		return nil, ErrBadRequest("请填写联系方式")
	}
	var tier model.AgentTier
	if err := s.db.First(&tier, in.TierID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("申请的等级不存在")
		}
		return nil, err
	}
	var pending int64
	if err := s.db.Model(&model.AgentApplication{}).
		Where("user_id = ? AND status = ?", userID, model.AgentApplyPending).
		Count(&pending).Error; err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, ErrConflict("您已有一条待审核的申请")
	}
	application := model.AgentApplication{
		UserID:  userID,
		TierID:  in.TierID,
		Contact: in.Contact,
		Remark:  strings.TrimSpace(in.Remark),
		Status:  model.AgentApplyPending,
	}
	if err := s.db.Create(&application).Error; err != nil {
		return nil, err
	}
	return &application, nil
}

// ApplicationWithUser 是审核列表的一行。
type ApplicationWithUser struct {
	model.AgentApplication
	Username     string `json:"username"`
	Email        string `json:"email"`
	BalanceCents int64  `json:"balance_cents"`
	TierName     string `json:"tier_name"`
}

// Applications 返回申请列表（可按状态过滤，最新在前）。
func (s *AgentProgramService) Applications(status string) ([]ApplicationWithUser, error) {
	query := s.db.Model(&model.AgentApplication{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var items []model.AgentApplication
	if err := query.Order("id DESC").Limit(200).Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]ApplicationWithUser, 0, len(items))
	for _, item := range items {
		row := ApplicationWithUser{AgentApplication: item}
		var user model.User
		if err := s.db.First(&user, item.UserID).Error; err == nil {
			row.Username = user.Username
			row.Email = user.Email
			row.BalanceCents = user.BalanceCents
		}
		var tier model.AgentTier
		if err := s.db.First(&tier, item.TierID).Error; err == nil {
			row.TierName = tier.Name
		}
		out = append(out, row)
	}
	return out, nil
}

// ReviewInput 是审核入参。
type ReviewInput struct {
	Approve      bool   `json:"approve"`
	ReviewRemark string `json:"review_remark"`
}

// Review 审核代理申请：通过则把申请人绑到申请的等级（预授权，余额不足也生效）。
func (s *AgentProgramService) Review(applicationID uint, reviewerID uint, in ReviewInput) (*model.AgentApplication, error) {
	var application model.AgentApplication
	if err := s.db.First(&application, applicationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("申请不存在")
		}
		return nil, err
	}
	if application.Status != model.AgentApplyPending {
		return nil, ErrConflict("该申请已处理")
	}
	application.Status = model.AgentApplyRejected
	if in.Approve {
		application.Status = model.AgentApplyApproved
	}
	application.ReviewRemark = strings.TrimSpace(in.ReviewRemark)
	application.ReviewedBy = &reviewerID
	if err := s.db.Save(&application).Error; err != nil {
		return nil, err
	}
	if in.Approve {
		if err := s.db.Model(&model.User{}).Where("id = ?", application.UserID).
			Update("agent_tier_id", application.TierID).Error; err != nil {
			return nil, err
		}
	}
	return &application, nil
}
