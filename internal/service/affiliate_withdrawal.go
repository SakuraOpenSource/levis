package service

import (
	"errors"
	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"strings"
	"unicode/utf8"
)

type AffiliateWithdrawalInput struct {
	AmountCents int64  `json:"amount_cents"`
	Account     string `json:"account"`
	Remark      string `json:"remark"`
}
type AffiliateReviewInput struct {
	Action string `json:"action"`
	Remark string `json:"remark"`
}

func (s *AffiliateService) Withdraw(userID uint, in AffiliateWithdrawalInput) (*model.AffiliateWithdrawal, error) {
	in.Account = strings.TrimSpace(in.Account)
	in.Remark = strings.TrimSpace(in.Remark)
	settings, err := s.Settings()
	if err != nil {
		return nil, err
	}
	if !settings.Enabled {
		return nil, ErrConflict("推广计划未开放")
	}
	if in.AmountCents < settings.MinWithdrawalCents || in.AmountCents > 100_000_000 || in.Account == "" || utf8.RuneCountInString(in.Account) > 255 || utf8.RuneCountInString(in.Remark) > 500 {
		return nil, ErrBadRequest("提现金额、账户或备注无效")
	}
	var out model.AffiliateWithdrawal
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var a model.Affiliate
		if err := tx.Where("user_id = ?", userID).First(&a).Error; err != nil {
			return ErrNotFound("请先加入推广计划")
		}
		claim := tx.Model(&model.Affiliate{}).Where("id = ? AND balance_cents >= ?", a.ID, in.AmountCents).Updates(map[string]any{"balance_cents": gorm.Expr("balance_cents - ?", in.AmountCents), "reserved_cents": gorm.Expr("reserved_cents + ?", in.AmountCents)})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return ErrBadRequest("可提现余额不足")
		}
		out = model.AffiliateWithdrawal{UserID: userID, AffiliateID: a.ID, AmountCents: in.AmountCents, Account: in.Account, Remark: in.Remark, Status: "pending"}
		return tx.Create(&out).Error
	})
	return &out, err
}

// Review transfers an approved reservation into the Levis wallet, not a claimed
// external payout. Refund debt blocks payout until rejected/resolved.
func (s *AffiliateService) Review(reviewerID, id uint, in AffiliateReviewInput) (*model.AffiliateWithdrawal, error) {
	if in.Action != "approve" && in.Action != "reject" {
		return nil, ErrBadRequest("审核操作无效")
	}
	if utf8.RuneCountInString(in.Remark) > 500 {
		return nil, ErrBadRequest("审核备注过长")
	}
	var out model.AffiliateWithdrawal
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var admin model.User
		if err := tx.First(&admin, "id = ? AND role = ? AND status = ?", reviewerID, model.RoleAdmin, model.UserActive).Error; err != nil {
			return ErrForbidden("需要管理员权限")
		}
		if err := tx.First(&out, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound("提现申请不存在")
			}
			return err
		}
		status := "rejected"
		if in.Action == "approve" {
			status = "approved"
		}
		claim := tx.Model(&model.AffiliateWithdrawal{}).Where("id = ? AND status = ?", id, "pending").Updates(map[string]any{"status": status, "review_remark": strings.TrimSpace(in.Remark), "reviewed_by": reviewerID})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return ErrConflict("申请已审核")
		}
		account := tx.Model(&model.Affiliate{}).Where("id = ? AND reserved_cents >= ?", out.AffiliateID, out.AmountCents)
		updates := map[string]any{"reserved_cents": gorm.Expr("reserved_cents - ?", out.AmountCents)}
		if in.Action == "approve" {
			account = account.Where("balance_cents >= 0")
		} else {
			updates["balance_cents"] = gorm.Expr("balance_cents + ?", out.AmountCents)
		}
		claim = account.Updates(updates)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return ErrConflict("佣金已撤销或预留不足，请驳回申请")
		}
		if in.Action == "approve" {
			if _, err := NewWalletService(tx).adjustBalance(tx, out.UserID, out.AmountCents, model.TxAdjust, "affiliate_withdrawal", out.ID, "推广佣金转入 Levis 钱包"); err != nil {
				return err
			}
		}
		out.Status, out.ReviewRemark, out.ReviewedBy = status, strings.TrimSpace(in.Remark), &reviewerID
		return nil
	})
	return &out, err
}
func (s *AffiliateService) Commissions(userID uint, offset, limit int) ([]model.AffiliateCommission, int64, error) {
	items := []model.AffiliateCommission{}
	var total int64
	q := s.db.Model(&model.AffiliateCommission{}).Where("affiliate_id IN (?)", s.db.Model(&model.Affiliate{}).Select("id").Where("user_id = ?", userID))
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	return items, total, err
}
func (s *AffiliateService) Withdrawals(userID uint, status string, offset, limit int) ([]model.AffiliateWithdrawal, int64, error) {
	items := []model.AffiliateWithdrawal{}
	var total int64
	q := s.db.Model(&model.AffiliateWithdrawal{})
	if userID != 0 {
		q = q.Where("user_id = ?", userID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	return items, total, err
}
