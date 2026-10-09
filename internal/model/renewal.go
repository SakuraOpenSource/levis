package model

// RenewalEvent is an expiry-cycle idempotency/notification ledger.
type RenewalEvent struct {
	Base
	ServiceID     uint   `gorm:"index;not null" json:"service_id"`
	UserID        uint   `gorm:"index;not null" json:"user_id"`
	Key           string `gorm:"uniqueIndex;size:128;not null" json:"key"`
	Kind          string `gorm:"size:32;not null" json:"kind"`
	InvoiceID     *uint  `json:"invoice_id"`
	Error         string `gorm:"size:500" json:"error"`
	UpstreamState string `gorm:"size:16" json:"upstream_state"`
}

const (
	SettingAutoRenewWindowHours = "auto_renew_window_hours"
	SettingLifecycleDryRun      = "lifecycle_dry_run"
)
