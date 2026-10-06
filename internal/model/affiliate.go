package model

// Affiliate balances are independent of the wallet. A refund after payout can
// create debt (negative available balance), which future commissions offset.
type Affiliate struct {
	Base
	UserID        uint   `gorm:"uniqueIndex;not null" json:"user_id"`
	Code          string `gorm:"uniqueIndex;size:32;not null" json:"code"`
	BalanceCents  int64  `gorm:"not null;default:0" json:"balance_cents"`
	ReservedCents int64  `gorm:"not null;default:0" json:"pending_cents"`
	EarnedCents   int64  `gorm:"not null;default:0" json:"total_earned_cents"`
}
type AffiliateReferral struct {
	Base
	UserID      uint `gorm:"uniqueIndex;not null" json:"user_id"`
	AffiliateID uint `gorm:"index;not null" json:"affiliate_id"`
}
type AffiliateCommission struct {
	Base
	AffiliateID   uint  `gorm:"index;not null" json:"affiliate_id"`
	UserID        uint  `gorm:"index;not null" json:"user_id"`
	OrderID       uint  `gorm:"uniqueIndex;not null" json:"order_id"`
	PaidCents     int64 `gorm:"not null" json:"paid_cents"`
	AmountCents   int64 `gorm:"not null" json:"amount_cents"`
	ReversedCents int64 `gorm:"not null;default:0" json:"reversed_cents"`
	RateBPS       int   `gorm:"not null" json:"rate_bps"`
}
type AffiliateReversal struct {
	Base
	RefundID     uint  `gorm:"uniqueIndex;not null" json:"refund_id"`
	CommissionID uint  `gorm:"index;not null" json:"commission_id"`
	AmountCents  int64 `gorm:"not null" json:"amount_cents"`
}
type AffiliateWithdrawal struct {
	Base
	UserID       uint   `gorm:"index;not null" json:"user_id"`
	AffiliateID  uint   `gorm:"index;not null" json:"affiliate_id"`
	AmountCents  int64  `gorm:"not null" json:"amount_cents"`
	Account      string `gorm:"size:255;not null" json:"account"`
	Remark       string `gorm:"size:500" json:"remark"`
	Status       string `gorm:"index;size:16;not null" json:"status"`
	ReviewRemark string `gorm:"size:500" json:"review_remark"`
	ReviewedBy   *uint  `json:"reviewed_by"`
}

const SettingAffiliate = "affiliate_settings"
