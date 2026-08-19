package model

import "time"

// 插件专用权限位。
//
// 与用户 API Key 的 scope 分开成一套：用户的权限位（balance:read 之类）都是
// 「操作我自己的资源」，插件的权限位是「以系统身份操作任意用户的资源」，
// 两者混在一个命名空间里迟早会有人把 wallet:credit 勾给某个用户的 Key。
//
// 需要明白这些 scope 的边界：它约束的是「能做哪类操作」，不是「能操作哪些
// 用户」。拿到 PluginScopeWalletCredit 的插件可以给任意用户加任意金额。
const (
	PluginScopeWalletCredit = "wallet:credit" // 给用户加余额（支付到账）
	PluginScopeUserRead     = "user:read"     // 读用户名与邮箱（发信要用）
	PluginScopeOrderRead    = "order:read"    // 读订单与账单
)

// AllPluginScopes 返回全部插件权限位，供校验与管理界面勾选使用。
func AllPluginScopes() []string {
	return []string{PluginScopeWalletCredit, PluginScopeUserRead, PluginScopeOrderRead}
}

// ValidPluginScope 报告 s 是否为受支持的插件权限位。
func ValidPluginScope(s string) bool {
	switch s {
	case PluginScopeWalletCredit, PluginScopeUserRead, PluginScopeOrderRead:
		return true
	}
	return false
}

// PluginKey 是签发给插件、用于回调主程序的凭证。
//
// 不复用 APIKey 表而是另开一张：APIKey 上的 UserID 是 not null 且业务上要求
// 归属一个通过实名的用户，插件是系统组件，没有归属用户可填。复用的是机制
// （SHA-256 摘要 + 唯一索引 + scope 列表），不是表。
//
// 生命周期与插件进程绑定：每次启动签发一把新的，旧的立即吊销，进程停止时
// 全部吊销。明文只经环境变量交给子进程，不落库、不进日志、不回传 API。
type PluginKey struct {
	Base
	PluginID string `gorm:"index;size:64;not null" json:"plugin_id"`
	KeyHash  string `gorm:"uniqueIndex;size:64;not null" json:"-"`
	// Scopes 取 manifest 声明与管理员勾选的交集，未勾选的一律 403。
	Scopes     ScopeList  `gorm:"type:text" json:"scopes"`
	Status     string     `gorm:"size:16;not null;default:active" json:"status"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// Usable 报告该 Key 当前是否可用。
//
// 插件 Key 没有过期时间：它随进程存活，进程一停就吊销，比设一个固定期限
// 更贴合实际生命周期。
func (k PluginKey) Usable() bool { return k.Status == APIKeyActive }

// PluginState 是管理员对某个插件的决定：是否启用、授予哪些权限。
//
// 与「插件自己声明需要什么」严格分开。manifest 里的 required_scopes 只是插件
// 的申请，授权是管理员在后台勾选的结果，存在这里。签发凭证时按这里的记录来，
// 不看 manifest —— 否则插件改一行代码就能给自己扩权。
//
// 另一个必须落库的理由：插件在首次启动前还没握手过，manifest 是空的，那时也
// 得知道该签发什么权限的凭证。
type PluginState struct {
	Base
	PluginID string `gorm:"uniqueIndex;size:64;not null" json:"plugin_id"`
	// Enabled 是管理员的意图，主程序重启后据此恢复运行中的插件。
	Enabled bool      `gorm:"not null;default:false" json:"enabled"`
	Scopes  ScopeList `gorm:"type:text" json:"scopes"`
}

// PluginSetting 是单个插件的一项配置。
//
// 没有塞进 Setting 表用 plugin:<id>:<field> 拼键：Setting.Key 是 size:64 的
// 主键，光「plugin:」加上 48 字符的插件 ID 就占掉 56，留给字段名的只剩 8 个
// 字符。加宽那个主键要在三种方言上改一次主键定义，代价比另开一张表大得多。
//
// 独立成表还顺带解决了两件事：按插件整体删除配置是一条语句；插件的密钥不会
// 混在站点设置里被一并读出去。
type PluginSetting struct {
	Base
	PluginID string `gorm:"uniqueIndex:idx_plugin_setting_key;size:64;not null" json:"plugin_id"`
	// Key 是 manifest 里 ConfigField.key 的原值。
	Key string `gorm:"uniqueIndex:idx_plugin_setting_key;size:64;not null" json:"key"`
	// Value 不限长：证书、私钥这类配置动辄几 KB。
	//
	// 这里存的是明文 —— 与 KYC 身份证号同样的处境，原因也相同：加密落盘需要
	// 独立的密钥管理，复用 JWTSecret 会把认证密钥与数据密钥绑死，轮换前者就
	// 会毁掉后者能解开的全部数据。当前防护是 data/ 0700、配置文件 0600、
	// 以及 API 永不回传 secret 字段的值。
	Value string `gorm:"type:text" json:"value"`
}

// ExternalPayment 是主程序创建的外部支付意图。回调只能结算已创建且金额匹配的意图。
const (
	ExternalPaymentPurposeRecharge = "recharge"
	ExternalPaymentPurposeOrder    = "order"
	ExternalPaymentPurposeInvoice  = "invoice"
	ExternalPaymentPurposeRenewal  = "renewal"
	ExternalPaymentPending    = "pending"
	ExternalPaymentProcessing = "processing"
	ExternalPaymentPaid       = "paid"
	ExternalPaymentFailed     = "failed"
	PluginPaymentPaid              = ExternalPaymentPaid
)

type ExternalPayment struct {
	Base
	PluginID        string     `gorm:"index:idx_external_payment_plugin_ext,unique;size:64;not null" json:"plugin_id"`
	ExternalID      string     `gorm:"index:idx_external_payment_plugin_ext,unique;size:128;not null" json:"external_id"`
	UserID          uint       `gorm:"index;not null" json:"user_id"`
	Purpose         string     `gorm:"size:16;not null" json:"purpose"`
	TargetID        uint       `gorm:"index" json:"target_id"`
	AmountCents     int64      `gorm:"not null" json:"amount_cents"`
	Currency        string     `gorm:"size:8;not null;default:CNY" json:"currency"`
	Subject         string     `gorm:"size:255;not null" json:"subject"`
	ReturnURL       string     `gorm:"size:1024" json:"return_url"`
	PayURL          string     `gorm:"size:2048" json:"pay_url"`
	GatewayRef      string     `gorm:"size:128" json:"gateway_ref"`
	PaidAmountCents int64      `json:"paid_amount_cents"`
	Status          string     `gorm:"size:16;index;not null;default:pending" json:"status"`
	FailureReason   string     `gorm:"size:255" json:"failure_reason"`
	PaidAt          *time.Time `json:"paid_at"`
}

// PluginPayment 记录插件报上来的每一笔到账，唯一索引即幂等键。
//
// 支付渠道一定会重复回调 —— 超时重试、人工补发都会触发。靠数据库的唯一
// 索引拦重复，不靠「先查再写」：那中间有竞态窗口，两个并发回调都能查到
// 「不存在」然后各加一次钱。
type PluginPayment struct {
	Base
	// 唯一索引建在 (plugin_id, external_id) 上而不是单独的 external_id 上：
	// external_id 由主程序生成，但两个插件各自的命名空间不该互相干扰。
	PluginID   string `gorm:"uniqueIndex:idx_plugin_payment_ext;size:64;not null" json:"plugin_id"`
	ExternalID string `gorm:"uniqueIndex:idx_plugin_payment_ext;size:128;not null" json:"external_id"`
	UserID     uint   `gorm:"index;not null" json:"user_id"`
	// AmountCents 是实际到账金额，用于幂等重放时原样回传。
	AmountCents int64 `gorm:"not null" json:"amount_cents"`
	// GatewayRef 是渠道侧的订单号，落库备查对账。
	GatewayRef string `gorm:"size:128" json:"gateway_ref"`
	Status     string `gorm:"size:16;not null;default:paid" json:"status"`
	// TransactionID 指向这笔到账产生的流水，便于从支付记录追到余额变动。
	TransactionID uint `gorm:"index" json:"transaction_id"`
}
