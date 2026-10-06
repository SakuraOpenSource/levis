package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AffiliateSettings struct {
	Enabled            bool  `json:"enabled"`
	RateBPS            int   `json:"rate_bps"`
	MinWithdrawalCents int64 `json:"min_withdrawal_cents"`
}
type AffiliateSummary struct {
	Code             string            `json:"code"`
	ReferralCount    int64             `json:"referral_count"`
	BalanceCents     int64             `json:"balance_cents"`
	PendingCents     int64             `json:"pending_cents"`
	TotalEarnedCents int64             `json:"total_earned_cents"`
	Settings         AffiliateSettings `json:"settings"`
}
type AffiliateService struct{ db *gorm.DB }

func NewAffiliateService(db *gorm.DB) *AffiliateService { return &AffiliateService{db: db} }
func affiliateSettings(db *gorm.DB) (AffiliateSettings, error) {
	out := AffiliateSettings{RateBPS: 500, MinWithdrawalCents: 1000}
	var row model.Setting
	err := db.Where(map[string]any{"key": model.SettingAffiliate}).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(row.Value), &out)
	return out, err
}
func (s *AffiliateService) Settings() (AffiliateSettings, error) { return affiliateSettings(s.db) }
func (s *AffiliateService) UpdateSettings(in AffiliateSettings) (AffiliateSettings, error) {
	if in.RateBPS < 0 || in.RateBPS > 10000 || in.MinWithdrawalCents < 1 || in.MinWithdrawalCents > 100_000_000 {
		return in, ErrBadRequest("推广费率需为 0-10000 基点，提现门槛需为 1-100000000 分")
	}
	b, err := json.Marshal(in)
	if err != nil {
		return in, err
	}
	err = s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).Create(&model.Setting{Key: model.SettingAffiliate, Value: string(b)}).Error
	return in, err
}
func (s *AffiliateService) Summary(userID uint) (*AffiliateSummary, error) {
	settings, err := s.Settings()
	if err != nil {
		return nil, err
	}
	out := &AffiliateSummary{Settings: settings}
	var a model.Affiliate
	if err = s.db.Where("user_id = ?", userID).First(&a).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	out.Code, out.BalanceCents, out.PendingCents, out.TotalEarnedCents = a.Code, a.BalanceCents, a.ReservedCents, a.EarnedCents
	err = s.db.Model(&model.AffiliateReferral{}).Where("affiliate_id = ?", a.ID).Count(&out.ReferralCount).Error
	return out, err
}
func (s *AffiliateService) Join(userID uint) (*AffiliateSummary, error) {
	settings, err := s.Settings()
	if err != nil {
		return nil, err
	}
	if !settings.Enabled {
		return nil, ErrConflict("推广计划未开放")
	}
	var u model.User
	if err = s.db.First(&u, "id = ? AND status = ?", userID, model.UserActive).Error; err != nil {
		return nil, ErrNotFound("用户不存在")
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	a := model.Affiliate{UserID: userID, Code: hex.EncodeToString(random[:])}
	if err = s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoNothing: true}).Create(&a).Error; err != nil {
		return nil, err
	}
	return s.Summary(userID)
}

// accrueAffiliateTx is called only after the order payment CAS, in its transaction.
// The unique order ledger entry and atomic account increment commit together.
func accrueAffiliateTx(tx *gorm.DB, order *model.Order) error {
	if order.Status != model.OrderPaid || order.PaidAt == nil || order.TotalCents <= 0 {
		return nil
	}
	settings, err := affiliateSettings(tx)
	if err != nil {
		return err
	}
	if !settings.Enabled || settings.RateBPS == 0 {
		return nil
	}
	var ref model.AffiliateReferral
	if err = tx.Where("user_id = ?", order.UserID).First(&ref).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var a model.Affiliate
	if err = tx.First(&a, ref.AffiliateID).Error; err != nil {
		return err
	}
	if a.UserID == order.UserID {
		return nil
	}
	amount, err := mulDivCents(order.TotalCents, int64(settings.RateBPS), 10000)
	if err != nil {
		return err
	}
	if amount == 0 {
		return nil
	}
	row := model.AffiliateCommission{AffiliateID: a.ID, UserID: order.UserID, OrderID: order.ID, PaidCents: order.TotalCents, AmountCents: amount, RateBPS: settings.RateBPS}
	res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "order_id"}}, DoNothing: true}).Create(&row)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	return tx.Model(&model.Affiliate{}).Where("id = ?", a.ID).Updates(map[string]any{"balance_cents": gorm.Expr("balance_cents + ?", amount), "earned_cents": gorm.Expr("earned_cents + ?", amount)}).Error
}

// reverseAffiliateTx runs only inside the successfully completed refund transaction.
// Cumulative proportional reversal avoids rounding drift across partial refunds.
func reverseAffiliateTx(tx *gorm.DB, refund *model.RefundRequest) error {
	if refund.OrderID == 0 || refund.AmountCents <= 0 {
		return nil
	}
	var c model.AffiliateCommission
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", refund.OrderID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var refunded int64
	if err = tx.Model(&model.RefundRequest{}).Where("order_id = ? AND status = ?", refund.OrderID, model.RefundCompleted).Select("COALESCE(SUM(amount_cents),0)").Scan(&refunded).Error; err != nil {
		return err
	}
	if refunded > c.PaidCents {
		refunded = c.PaidCents
	}
	target, err := mulDivCents(c.AmountCents, refunded, c.PaidCents)
	if err != nil {
		return err
	}
	delta := target - c.ReversedCents
	if delta <= 0 {
		return nil
	}
	entry := model.AffiliateReversal{RefundID: refund.ID, CommissionID: c.ID, AmountCents: delta}
	res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "refund_id"}}, DoNothing: true}).Create(&entry)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	res = tx.Model(&model.AffiliateCommission{}).Where("id = ? AND reversed_cents = ?", c.ID, c.ReversedCents).Update("reversed_cents", target)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrConflict("佣金已变更，请重试退款")
	}
	return tx.Model(&model.Affiliate{}).Where("id = ?", c.AffiliateID).Updates(map[string]any{"balance_cents": gorm.Expr("balance_cents - ?", delta), "earned_cents": gorm.Expr("earned_cents - ?", delta)}).Error
}

// Only invoked in the registration transaction: never an update of an existing attribution.
func attachAffiliateReferralTx(tx *gorm.DB, user *model.User, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil
	}
	settings, err := affiliateSettings(tx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return ErrBadRequest("推广计划未开放")
	}
	var a model.Affiliate
	if err = tx.Where("code = ?", code).First(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBadRequest("推广码无效")
		}
		return err
	}
	var owner model.User
	if err = tx.First(&owner, "id = ? AND status = ?", a.UserID, model.UserActive).Error; err != nil {
		return ErrBadRequest("推广码无效")
	}
	if owner.ID == user.ID || strings.EqualFold(owner.Email, user.Email) || strings.EqualFold(owner.Username, user.Username) {
		return ErrBadRequest("不能使用自己的推广码")
	}
	return tx.Create(&model.AffiliateReferral{UserID: user.ID, AffiliateID: a.ID}).Error
}
