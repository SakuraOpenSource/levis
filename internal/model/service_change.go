package model

// ServiceChange is the durable wallet reservation and provider operation ledger.
type ServiceChange struct {
	Base
	UserID            uint      `gorm:"index;not null" json:"user_id"`
	ServiceID         uint      `gorm:"uniqueIndex:change_key;index;not null" json:"service_id"`
	ProductID         uint      `json:"product_id"`
	PreviousProductID uint      `json:"previous_product_id"`
	IdempotencyKey    string    `gorm:"uniqueIndex:change_key;size:128;not null" json:"idempotency_key"`
	OperationID       string    `gorm:"uniqueIndex;size:128;not null" json:"operation_id"`
	Status            string    `gorm:"index;size:16;not null" json:"status"`
	ChargeCents       int64     `json:"charge_cents"`
	CreditCents       int64     `json:"credit_cents"`
	PriceCents        int64     `json:"price_cents"`
	Options           OptionMap `gorm:"type:text" json:"options"`
	PreviousResources OptionMap `gorm:"type:text" json:"-"`
	Error             string    `gorm:"size:500" json:"error"`
}
