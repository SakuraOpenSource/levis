package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// API Key 的权限位。写权限隐含其操作对象的读权限
// （能下单的 Key 自然要能看商品列表）。
const (
	ScopeBalanceRead  = "balance:read"  // 查余额与流水
	ScopeOrderWrite   = "order:write"   // 看商品、下单、支付
	ScopeServiceWrite = "service:write" // 看服务、续费
)

// AllScopes 返回全部可选权限位，供校验与前端展示使用。
func AllScopes() []string {
	return []string{ScopeBalanceRead, ScopeOrderWrite, ScopeServiceWrite}
}

// ValidScope 报告 s 是否为受支持的权限位。
func ValidScope(s string) bool {
	switch s {
	case ScopeBalanceRead, ScopeOrderWrite, ScopeServiceWrite:
		return true
	}
	return false
}

// API Key 状态。
const (
	APIKeyActive  = "active"
	APIKeyRevoked = "revoked"
)

// ScopeList 是权限位列表，以 JSON 文本存入单列。
//
// 与 SpecList 同一套做法：三种数据库共用一份定义，不依赖 MySQL/PG 的
// JSON 类型，也不必为权限位另开一张关联表 —— 它只随 Key 整体读写。
type ScopeList []string

// Value 实现 driver.Valuer，写库时序列化为 JSON 文本。
func (s ScopeList) Value() (driver.Value, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal([]string(s))
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// Scan 实现 sql.Scanner，读库时从 JSON 文本还原。
func (s *ScopeList) Scan(src any) error {
	if src == nil {
		*s = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("model: 无法把 %T 解析为 ScopeList", src)
	}
	if len(raw) == 0 {
		*s = nil
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return errors.New("model: 权限字段不是合法的 JSON 数组")
	}
	*s = out
	return nil
}

// Has 报告列表中是否包含 scope。
func (s ScopeList) Has(scope string) bool {
	for _, item := range s {
		if item == scope {
			return true
		}
	}
	return false
}

// APIKey 是用户创建的接口凭证。
//
// KeyHash 存 SHA-256 而非 bcrypt：明文是 128 位密码学随机串，没有字典攻击面，
// 不需要慢 KDF；而 bcrypt 无法建索引，每次调用都得全表扫描逐行比对。
// SHA-256 的 hex 摘要可以直接走唯一索引一次命中。
//
// 明文只在创建响应里出现一次，此后系统里任何地方都取不回。
type APIKey struct {
	Base
	UserID uint   `gorm:"index;not null" json:"user_id"`
	Name   string `gorm:"size:64;not null" json:"name"`
	// Prefix 是明文的前若干位，用于在列表里辨认「这是哪一把」。
	Prefix     string     `gorm:"size:16;not null" json:"prefix"`
	KeyHash    string     `gorm:"uniqueIndex;size:64;not null" json:"-"`
	Scopes     ScopeList  `gorm:"type:text" json:"scopes"`
	Status     string     `gorm:"size:16;not null;default:active" json:"status"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// Usable 报告该 Key 当前是否可用（启用中且未过期）。
func (k APIKey) Usable(now time.Time) bool {
	if k.Status != APIKeyActive {
		return false
	}
	return k.ExpiresAt == nil || k.ExpiresAt.After(now)
}
