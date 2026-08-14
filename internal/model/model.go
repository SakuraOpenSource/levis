// Package model 定义 Levis 的数据库模型。
//
// 金额约定：所有金额字段一律使用 int64 存储最小货币单位（分），字段名带
// _cents 后缀。绝不使用浮点类型存储金额。
package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Base 是所有模型共用的主键与时间戳。
type Base struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// 用户角色。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// 用户状态。
const (
	UserActive   = "active"
	UserDisabled = "disabled"
)

// 计费周期。
const (
	CycleOneTime      = "onetime"
	CycleMonthly      = "monthly"
	CycleQuarterly    = "quarterly"
	CycleSemiannually = "semiannually"
	CycleAnnually     = "annually"
	CycleBiennially   = "biennially"
	CycleTriennially  = "triennially"
)

// ValidCycle 报告 c 是否为受支持的计费周期。
func ValidCycle(c string) bool {
	switch c {
	case CycleOneTime, CycleMonthly, CycleQuarterly, CycleSemiannually,
		CycleAnnually, CycleBiennially, CycleTriennially:
		return true
	}
	return false
}

// AdvanceCycle 返回从 from 起经过一个 c 周期后的时间。
// 一次性付费无续费周期，返回零值时间。
func AdvanceCycle(from time.Time, c string) time.Time {
	switch c {
	case CycleMonthly:
		return from.AddDate(0, 1, 0)
	case CycleQuarterly:
		return from.AddDate(0, 3, 0)
	case CycleSemiannually:
		return from.AddDate(0, 6, 0)
	case CycleAnnually:
		return from.AddDate(1, 0, 0)
	case CycleBiennially:
		return from.AddDate(2, 0, 0)
	case CycleTriennially:
		return from.AddDate(3, 0, 0)
	}
	return time.Time{}
}

// Setting 是站点级配置的键值存储（站点名称、简介等）。
type Setting struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `json:"value"`
}

// 站点设置的键名。
const (
	SettingSiteName        = "site_name"
	SettingSiteDescription = "site_description"
)

// User 是系统用户。普通用户与管理员共用此表，由 Role 区分。
type User struct {
	Base
	Username string `gorm:"uniqueIndex;size:64;not null" json:"username"`
	Email    string `gorm:"uniqueIndex;size:255;not null" json:"email"`
	// PasswordHash 带 json:"-"，确保 bcrypt 哈希永不进入 API 响应。
	PasswordHash string `gorm:"size:255;not null" json:"-"`
	Role         string `gorm:"size:16;not null;default:user" json:"role"`
	BalanceCents int64  `gorm:"not null;default:0" json:"balance_cents"`
	Status       string `gorm:"size:16;not null;default:active" json:"status"`
}

// IsAdmin 报告用户是否为管理员。
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// ProductCategory 是商品分组，通过 ParentID 自引用形成两级结构
// （如「香港」大类下挂 HK1、HK2 小类）。
type ProductCategory struct {
	Base
	ParentID    *uint             `gorm:"index" json:"parent_id"`
	Name        string            `gorm:"size:128;not null" json:"name"`
	Slug        string            `gorm:"uniqueIndex;size:128;not null" json:"slug"`
	Description string            `json:"description"`
	Sort        int               `gorm:"not null;default:0" json:"sort"`
	Children    []ProductCategory `gorm:"-" json:"children,omitempty"`
	Products    []Product         `gorm:"-" json:"products,omitempty"`
}

// 商品状态。
const (
	ProductActive = "active"
	ProductHidden = "hidden"
)

// Spec 是商品的一条规格，如 {Label: "CPU", Value: "4 核"}。
//
// 刻意不做成 CPU、内存这类固定字段：不同商品要展示的维度差别很大
// （云服务器看 CPU/内存，虚拟主机看空间/数据库数），键值对最灵活。
type Spec struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// SpecList 是规格列表，以 JSON 文本存入单列。
//
// 这样三种数据库共用一份定义，不依赖 MySQL/PG 的 JSON 类型，
// 也不必为规格另开一张表 —— 规格只随商品整体读写，没有独立查询需求。
type SpecList []Spec

// Value 实现 driver.Valuer，写库时序列化为 JSON 文本。
func (s SpecList) Value() (driver.Value, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal([]Spec(s))
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// Scan 实现 sql.Scanner，读库时从 JSON 文本还原。
//
// 兼容 NULL 与空串：历史行在加上本字段前没有值，迁移后读出来是空的。
func (s *SpecList) Scan(src any) error {
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
		return fmt.Errorf("model: 无法把 %T 解析为 SpecList", src)
	}
	if len(raw) == 0 {
		*s = nil
		return nil
	}
	var out []Spec
	if err := json.Unmarshal(raw, &out); err != nil {
		return errors.New("model: 规格字段不是合法的 JSON 数组")
	}
	*s = out
	return nil
}

// Product 是可购买的商品。
type Product struct {
	Base
	CategoryID  uint   `gorm:"index;not null" json:"category_id"`
	Name        string `gorm:"size:128;not null" json:"name"`
	Description string `json:"description"`
	// Specs 是展示用的规格行（CPU、内存、带宽…），存为 JSON 文本。
	Specs      SpecList `gorm:"type:text" json:"specs"`
	PriceCents int64    `gorm:"not null;default:0" json:"price_cents"`
	BillingCyc string   `gorm:"column:billing_cycle;size:16;not null;default:monthly" json:"billing_cycle"`
	// Stock 为负数表示库存不限。
	Stock  int    `gorm:"not null;default:-1" json:"stock"`
	Status string `gorm:"size:16;not null;default:active" json:"status"`
	Sort   int    `gorm:"not null;default:0" json:"sort"`
}

// CartItem 是购物车条目。同一用户、同一商品、同一计费周期唯一。
type CartItem struct {
	Base
	UserID     uint     `gorm:"uniqueIndex:idx_cart_unique;not null" json:"user_id"`
	ProductID  uint     `gorm:"uniqueIndex:idx_cart_unique;not null" json:"product_id"`
	BillingCyc string   `gorm:"column:billing_cycle;uniqueIndex:idx_cart_unique;size:16;not null" json:"billing_cycle"`
	Quantity   int      `gorm:"not null;default:1" json:"quantity"`
	Product    *Product `gorm:"foreignKey:ProductID" json:"product,omitempty"`
}

// 订单状态。
const (
	OrderPending   = "pending"
	OrderPaid      = "paid"
	OrderCancelled = "cancelled"
)

// Order 是一次下单。
type Order struct {
	Base
	OrderNo    string      `gorm:"uniqueIndex;size:32;not null" json:"order_no"`
	UserID     uint        `gorm:"index;not null" json:"user_id"`
	Status     string      `gorm:"size:16;not null;default:pending" json:"status"`
	TotalCents int64       `gorm:"not null;default:0" json:"total_cents"`
	PaidAt     *time.Time  `json:"paid_at"`
	Items      []OrderItem `gorm:"foreignKey:OrderID" json:"items,omitempty"`
}

// OrderItem 是订单明细。ProductName 与 PriceCents 是刻意冗余的快照字段：
// 商品日后改价或下架，历史订单仍须显示成交时的原值。
type OrderItem struct {
	Base
	OrderID     uint   `gorm:"index;not null" json:"order_id"`
	ProductID   uint   `gorm:"index;not null" json:"product_id"`
	ProductName string `gorm:"size:128;not null" json:"product_name"`
	PriceCents  int64  `gorm:"not null;default:0" json:"price_cents"`
	Quantity    int    `gorm:"not null;default:1" json:"quantity"`
	BillingCyc  string `gorm:"column:billing_cycle;size:16;not null" json:"billing_cycle"`
}

// 服务状态。
const (
	ServicePending    = "pending"
	ServiceActive     = "active"
	ServiceSuspended  = "suspended"
	ServiceTerminated = "terminated"
)

// Service 是用户已购买并开通的服务实例。
type Service struct {
	Base
	UserID     uint       `gorm:"index;not null" json:"user_id"`
	ProductID  uint       `gorm:"index;not null" json:"product_id"`
	OrderID    uint       `gorm:"index;not null" json:"order_id"`
	Name       string     `gorm:"size:128;not null" json:"name"`
	Status     string     `gorm:"size:16;not null;default:pending" json:"status"`
	BillingCyc string     `gorm:"column:billing_cycle;size:16;not null" json:"billing_cycle"`
	PriceCents int64      `gorm:"not null;default:0" json:"price_cents"`
	NextDueAt  *time.Time `json:"next_due_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// 账单状态。
const (
	InvoiceUnpaid    = "unpaid"
	InvoicePaid      = "paid"
	InvoiceCancelled = "cancelled"
)

// Invoice 是账单。
type Invoice struct {
	Base
	InvoiceNo  string        `gorm:"uniqueIndex;size:32;not null" json:"invoice_no"`
	UserID     uint          `gorm:"index;not null" json:"user_id"`
	OrderID    *uint         `gorm:"index" json:"order_id"`
	Status     string        `gorm:"size:16;not null;default:unpaid" json:"status"`
	TotalCents int64         `gorm:"not null;default:0" json:"total_cents"`
	DueAt      *time.Time    `json:"due_at"`
	PaidAt     *time.Time    `json:"paid_at"`
	Items      []InvoiceItem `gorm:"foreignKey:InvoiceID" json:"items,omitempty"`
}

// InvoiceItem 是账单明细。
type InvoiceItem struct {
	Base
	InvoiceID   uint   `gorm:"index;not null" json:"invoice_id"`
	ServiceID   *uint  `gorm:"index" json:"service_id"`
	Description string `gorm:"size:255;not null" json:"description"`
	AmountCents int64  `gorm:"not null;default:0" json:"amount_cents"`
}

// 流水类型。
const (
	TxRecharge = "recharge"
	TxPayment  = "payment"
	TxRefund   = "refund"
	TxAdjust   = "adjust"
)

// Transaction 是余额变动流水。AmountCents 带符号：正为入账，负为出账。
// BalanceAfterCents 记录变动后的余额，便于对账。
type Transaction struct {
	Base
	UserID            uint   `gorm:"index;not null" json:"user_id"`
	Type              string `gorm:"size:16;not null" json:"type"`
	AmountCents       int64  `gorm:"not null" json:"amount_cents"`
	BalanceAfterCents int64  `gorm:"not null" json:"balance_after_cents"`
	RefType           string `gorm:"size:32" json:"ref_type"`
	RefID             uint   `json:"ref_id"`
	Note              string `gorm:"size:255" json:"note"`
}

// AllModels 返回需要迁移的全部模型，供 AutoMigrate 使用。
func AllModels() []any {
	return []any{
		&Setting{},
		&User{},
		&ProductCategory{},
		&Product{},
		&CartItem{},
		&Order{},
		&OrderItem{},
		&Service{},
		&Invoice{},
		&InvoiceItem{},
		&Transaction{},
	}
}
