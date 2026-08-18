package service

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
)

// pluginKeyPrefix 是插件凭证的明文前缀。
//
// 与用户 Key 的 lvs_ 区分开：在日志或抓包里一眼能看出这是系统组件的凭证，
// 而不是某个用户的；也避免把插件 Key 误填进用户接口时得到含糊的错误。
const pluginKeyPrefix = "lvsp_"

// PluginKeyService 签发与校验插件回调凭证。
type PluginKeyService struct {
	db *gorm.DB
}

// NewPluginKeyService 构造 PluginKeyService。
func NewPluginKeyService(db *gorm.DB) *PluginKeyService {
	return &PluginKeyService{db: db}
}

// IssueKey 为插件签发一把新凭证，同时吊销它此前持有的全部凭证。
//
// 每次启动换一把，是为了让「插件进程」与「有效凭证」严格一一对应：进程没了
// 凭证就没用了，插件被人拷走二进制也带不走一把长期有效的 Key。
//
// 返回的明文只在这里出现一次，随后经环境变量交给子进程，不落库、不进日志。
// 实现 plugin.KeyIssuer。
func (s *PluginKeyService) IssueKey(pluginID string, scopes []string) (string, error) {
	if err := plugin.ValidID(pluginID); err != nil {
		return "", ErrBadRequest("%v", err)
	}
	list, err := normalizePluginScopes(scopes)
	if err != nil {
		return "", err
	}

	secret, err := generatePluginSecret()
	if err != nil {
		return "", err
	}
	key := model.PluginKey{
		PluginID: pluginID,
		KeyHash:  HashAPIKey(secret),
		Scopes:   list,
		Status:   model.APIKeyActive,
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 先吊销旧的再签新的：反过来会有一瞬间两把都有效。
		if err := revokeKeys(tx, pluginID); err != nil {
			return err
		}
		return tx.Create(&key).Error
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// RevokeKeys 吊销插件的全部凭证。插件停止或从磁盘消失时调用。
// 实现 plugin.KeyIssuer。
func (s *PluginKeyService) RevokeKeys(pluginID string) error {
	return revokeKeys(s.db, pluginID)
}

// revokeKeys 是 IssueKey 与 RevokeKeys 共用的吊销逻辑。
//
// 只置状态不删行：留着记录才能回答「那把 Key 是什么时候签的、最后一次被用于
// 何时」，这是插件行为出问题时事后追查的起点。
func revokeKeys(tx *gorm.DB, pluginID string) error {
	return tx.Model(&model.PluginKey{}).
		Where("plugin_id = ? AND status = ?", pluginID, model.APIKeyActive).
		Update("status", model.APIKeyRevoked).Error
}

// Authenticate 校验明文插件凭证。
func (s *PluginKeyService) Authenticate(secret string) (*model.PluginKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(secret, pluginKeyPrefix) {
		return nil, ErrUnauthorized("插件凭证无效")
	}
	var key model.PluginKey
	err := s.db.First(&key, "key_hash = ?", HashAPIKey(secret)).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnauthorized("插件凭证无效")
		}
		return nil, err
	}
	if !key.Usable() {
		return nil, ErrUnauthorized("插件凭证已吊销")
	}
	return &key, nil
}

// TouchLastUsed 记录一次使用时间，距上次不足一分钟则跳过。
//
// 与 APIKeyService.TouchLastUsed 同一口径：UpdateColumn 绕开 UpdatedAt，
// 写失败只忽略 —— 遥测不该拖垮回调。
func (s *PluginKeyService) TouchLastUsed(key *model.PluginKey) {
	now := time.Now().UTC()
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < lastUsedThrottle {
		return
	}
	s.db.Model(&model.PluginKey{}).Where("id = ?", key.ID).UpdateColumn("last_used_at", now)
	key.LastUsedAt = &now
}

// RevokeAll 吊销全部插件凭证，主程序启动时调用。
//
// 上次进程若是被 kill -9 收走，来不及吊销的凭证会留在库里；那些插件进程早已
// 不在，凭证却仍然有效。启动时统一清一遍，保证「有效凭证」不早于本次进程。
func (s *PluginKeyService) RevokeAll() error {
	return s.db.Model(&model.PluginKey{}).
		Where("status = ?", model.APIKeyActive).
		Update("status", model.APIKeyRevoked).Error
}

// generatePluginSecret 生成形如 lvsp_<32 hex> 的明文凭证。
func generatePluginSecret() (string, error) {
	secret, err := generateSecret()
	if err != nil {
		return "", err
	}
	return pluginKeyPrefix + strings.TrimPrefix(secret, keyPrefix), nil
}

// normalizePluginScopes 去重并校验插件权限位。
//
// 允许为空：一个只发信的插件可以完全不需要回调主程序，强制它勾一项反而是
// 多给权限。
func normalizePluginScopes(scopes []string) (model.ScopeList, error) {
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !model.ValidPluginScope(s) {
			return nil, ErrBadRequest("未知的插件权限项：%s", s)
		}
		seen[s] = true
	}
	if len(seen) == 0 {
		return model.ScopeList{}, nil
	}
	// 按 AllPluginScopes 的顺序输出，库里存的顺序才稳定。
	out := make(model.ScopeList, 0, len(seen))
	for _, s := range model.AllPluginScopes() {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}
