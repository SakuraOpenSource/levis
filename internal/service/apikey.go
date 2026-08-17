package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// keyPrefix 是明文 Key 的固定前缀，便于在日志或代码里一眼认出这是什么凭证
// （也方便日后接密钥扫描工具）。
const keyPrefix = "lvs_"

// keyBytes 是随机部分的字节数。16 字节 = 128 位熵，穷举不可行。
const keyBytes = 16

// MaxAPIKeys 是每个账号可持有的启用中 Key 数量上限。
const MaxAPIKeys = 10

// lastUsedThrottle 是 LastUsedAt 的最小更新间隔。
//
// 每次调用都写一行等于给热点行加了一次无谓的写放大；「最近使用」精确到分钟
// 完全够用。
const lastUsedThrottle = time.Minute

// APIKeyService 管理用户的接口凭证。
type APIKeyService struct {
	db *gorm.DB
}

// NewAPIKeyService 构造 APIKeyService。
func NewAPIKeyService(db *gorm.DB) *APIKeyService {
	return &APIKeyService{db: db}
}

// APIKeyCreateRequest 是创建 Key 的入参。
type APIKeyCreateRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresIn int      `json:"expires_in_days"` // 0 表示永不过期
}

// APIKeyCreated 是创建结果，Secret 是明文，只在此处出现一次。
type APIKeyCreated struct {
	Key    *model.APIKey `json:"key"`
	Secret string        `json:"secret"`
}

// Create 生成一把新 Key。
//
// 调用方须先确认用户已通过实名认证（KYC）—— 那是策略判断，放在 handler 层
// 与其它前置校验一处，不埋在这里。
func (s *APIKeyService) Create(userID uint, req APIKeyCreateRequest) (*APIKeyCreated, error) {
	name := strings.TrimSpace(req.Name)
	if count := utf8.RuneCountInString(name); count == 0 || count > 64 {
		return nil, ErrBadRequest("请填写 1-64 个字符的名称")
	}
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		return nil, err
	}
	if req.ExpiresIn < 0 || req.ExpiresIn > 3650 {
		return nil, ErrBadRequest("有效期需在 0-3650 天之间")
	}

	var active int64
	err = s.db.Model(&model.APIKey{}).
		Where("user_id = ? AND status = ?", userID, model.APIKeyActive).Count(&active).Error
	if err != nil {
		return nil, err
	}
	if active >= MaxAPIKeys {
		return nil, ErrConflict("最多只能持有 %d 把启用中的 Key，请先吊销不用的", MaxAPIKeys)
	}

	secret, err := generateSecret()
	if err != nil {
		return nil, err
	}
	var expiresAt *time.Time
	if req.ExpiresIn > 0 {
		t := time.Now().UTC().AddDate(0, 0, req.ExpiresIn)
		expiresAt = &t
	}

	key := model.APIKey{
		UserID:    userID,
		Name:      name,
		Prefix:    secret[:len(keyPrefix)+8],
		KeyHash:   HashAPIKey(secret),
		Scopes:    scopes,
		Status:    model.APIKeyActive,
		ExpiresAt: expiresAt,
	}
	if err := s.db.Create(&key).Error; err != nil {
		return nil, err
	}
	return &APIKeyCreated{Key: &key, Secret: secret}, nil
}

// List 返回用户的全部 Key。KeyHash 带 json:"-"，明文根本没存，都不会外泄。
func (s *APIKeyService) List(userID uint) ([]model.APIKey, error) {
	var items []model.APIKey
	err := s.db.Where("user_id = ?", userID).Order("id DESC").Find(&items).Error
	if err != nil {
		return nil, err
	}
	return items, nil
}

// Revoke 吊销一把 Key。
//
// 只置状态不删行：留着记录才能回答「那把 Key 是谁在什么时候建的、最后用于
// 何时」，这是事后追查的起点。
func (s *APIKeyService) Revoke(id, userID uint) error {
	result := s.db.Model(&model.APIKey{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userID, model.APIKeyActive).
		Update("status", model.APIKeyRevoked)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		err := s.db.Model(&model.APIKey{}).
			Where("id = ? AND user_id = ?", id, userID).Count(&count).Error
		if err != nil {
			return err
		}
		if count == 0 {
			// 不属于自己的 Key 一律报 404，不确认它是否存在。
			return ErrNotFound("Key 不存在")
		}
		return ErrConflict("该 Key 已吊销")
	}
	return nil
}

// Authenticate 校验明文 Key，返回对应记录。
//
// 只做「这把 Key 本身有效吗」，用户状态由中间件回查 —— 与 RequireAuth 同一
// 口径，账号被停用能立刻生效。
func (s *APIKeyService) Authenticate(secret string) (*model.APIKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(secret, keyPrefix) {
		return nil, ErrUnauthorized("API Key 无效")
	}
	var key model.APIKey
	// SHA-256 的 hex 摘要走唯一索引一次命中；bcrypt 在这里就得全表扫描了。
	err := s.db.First(&key, "key_hash = ?", HashAPIKey(secret)).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnauthorized("API Key 无效")
		}
		return nil, err
	}
	if !key.Usable(time.Now().UTC()) {
		return nil, ErrUnauthorized("API Key 已吊销或已过期")
	}
	return &key, nil
}

// TouchLastUsed 记录一次使用时间，距上次不足一分钟则跳过。
//
// UpdateColumn 绕开钩子与 UpdatedAt：这是遥测，不该让 Key 记录看起来「被
// 修改过」。写失败只忽略 —— 统计不该拖垮请求。
func (s *APIKeyService) TouchLastUsed(key *model.APIKey) {
	now := time.Now().UTC()
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < lastUsedThrottle {
		return
	}
	s.db.Model(&model.APIKey{}).Where("id = ?", key.ID).UpdateColumn("last_used_at", now)
	key.LastUsedAt = &now
}

// DeleteUserKeys 删除用户的全部 Key。删号时调用。
func (s *APIKeyService) DeleteUserKeys(tx *gorm.DB, userID uint) error {
	return tx.Where("user_id = ?", userID).Delete(&model.APIKey{}).Error
}

// HashAPIKey 返回明文 Key 的 SHA-256 十六进制摘要。
func HashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// generateSecret 生成形如 lvs_<32 hex> 的明文 Key。
func generateSecret() (string, error) {
	buf := make([]byte, keyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return keyPrefix + hex.EncodeToString(buf), nil
}

// normalizeScopes 去重并校验权限位，返回按声明顺序排列的结果。
func normalizeScopes(scopes []string) (model.ScopeList, error) {
	if len(scopes) == 0 {
		return nil, ErrBadRequest("请至少勾选一项权限")
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !model.ValidScope(s) {
			return nil, ErrBadRequest("未知的权限项：%s", s)
		}
		seen[s] = true
	}
	// 按 AllScopes 的顺序输出，前端展示与库里存的顺序才稳定一致。
	out := make(model.ScopeList, 0, len(seen))
	for _, s := range model.AllScopes() {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}
