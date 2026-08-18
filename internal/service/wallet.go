package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// WalletService 处理余额与流水。
type WalletService struct {
	db *gorm.DB
}

// NewWalletService 构造 WalletService。
func NewWalletService(db *gorm.DB) *WalletService {
	return &WalletService{db: db}
}

// adjustBalance 在事务内变动用户余额并记录流水。
//
// 这是全系统唯一的余额写入口径，所有资金操作都必须经由此函数。三条硬性要求：
//  1. 必须在调用方的事务中执行（tx 由外部传入），保证与业务写入同生共死；
//  2. 用 SQL 层面的 balance_cents = balance_cents + ? 做自增，不做读-改-写，
//     避免并发下的更新丢失；
//  3. 扣款时把「余额充足」写进 WHERE 条件，靠受影响行数判断结果，从而在
//     单条语句内完成检查与扣减，不留竞态窗口。
func (s *WalletService) adjustBalance(
	tx *gorm.DB, userID uint, deltaCents int64, txType, refType string, refID uint, note string,
) (*model.Transaction, error) {
	if deltaCents == 0 {
		return nil, ErrBadRequest("金额不能为零")
	}

	query := tx.Model(&model.User{}).Where("id = ?", userID)
	if deltaCents < 0 {
		// 扣款：余额不足时此条件不匹配，RowsAffected 为 0。
		query = query.Where("balance_cents >= ?", -deltaCents)
	}
	result := query.UpdateColumn("balance_cents", gorm.Expr("balance_cents + ?", deltaCents))
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		if deltaCents < 0 {
			return nil, ErrBadRequest("余额不足")
		}
		return nil, ErrNotFound("用户不存在")
	}

	// 回读变动后的余额写入流水，便于日后对账。
	var user model.User
	if err := tx.Select("balance_cents").First(&user, userID).Error; err != nil {
		return nil, err
	}

	record := model.Transaction{
		UserID:            userID,
		Type:              txType,
		AmountCents:       deltaCents,
		BalanceAfterCents: user.BalanceCents,
		RefType:           refType,
		RefID:             refID,
		Note:              note,
	}
	if err := tx.Create(&record).Error; err != nil {
		return nil, err
	}
	return &record, nil
}

// Recharge 为用户充值（当前为假充值，等待接入真实支付渠道）。
func (s *WalletService) Recharge(userID uint, amountCents int64) (*model.Transaction, error) {
	if amountCents <= 0 {
		return nil, ErrBadRequest("充值金额必须大于零")
	}
	if amountCents > 100_000_000 {
		return nil, ErrBadRequest("单次充值金额过大")
	}
	var record *model.Transaction
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		record, err = s.adjustBalance(tx, userID, amountCents, model.TxRecharge, "", 0, "账户充值")
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// CreditExternal 按插件报上来的到账给用户加余额，对 (pluginID, externalID) 幂等。
//
// 幂等靠数据库的唯一索引，不靠「先查再写」：支付渠道的重试往往是并发的，
// 先查再写的两个请求都会查到「不存在」，然后各加一次钱。这里的做法是直接
// 插入，让索引冲突来告诉我们「这笔已经处理过了」，然后返回首次的结果 ——
// 幂等接口对重复请求必须回成功，返回 409 会让渠道一直重试到人工介入。
//
// 资金路径不复制：仍然只经 adjustBalance 这一个入口。这个方法存在的唯一
// 理由就是让插件的调用也走那里，而不是把私有方法改成公开。
func (s *WalletService) CreditExternal(
	pluginID, externalID string, userID uint, amountCents int64, gatewayRef string,
) (*model.PluginPayment, error) {
	if amountCents <= 0 {
		return nil, ErrBadRequest("到账金额必须大于零")
	}
	if amountCents > 100_000_000 {
		return nil, ErrBadRequest("单笔到账金额过大")
	}
	if externalID == "" {
		return nil, ErrBadRequest("缺少 external_id")
	}

	record := model.PluginPayment{
		PluginID:    pluginID,
		ExternalID:  externalID,
		UserID:      userID,
		AmountCents: amountCents,
		GatewayRef:  gatewayRef,
		Status:      model.PluginPaymentPaid,
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		note := "插件 " + pluginID + " 支付到账"
		entry, err := s.adjustBalance(tx, userID, amountCents, model.TxRecharge,
			"plugin_payment", record.ID, note)
		if err != nil {
			return err
		}
		// 回填流水 ID，事后能从支付记录直接追到余额变动。
		record.TransactionID = entry.ID
		return tx.Model(&record).UpdateColumn("transaction_id", entry.ID).Error
	})
	if err == nil {
		return &record, nil
	}

	// 唯一索引冲突意味着重复回调，查出首次的记录原样返回。
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, err
	}
	var existing model.PluginPayment
	find := s.db.Where("plugin_id = ? AND external_id = ?", pluginID, externalID).First(&existing)
	if find.Error != nil {
		return nil, find.Error
	}
	return &existing, nil
}

// Overview 是钱包概览。
type Overview struct {
	BalanceCents  int64 `json:"balance_cents"`
	UnpaidCount   int64 `json:"unpaid_invoice_count"`
	UnpaidCents   int64 `json:"unpaid_total_cents"`
	ServiceActive int64 `json:"active_service_count"`
}

// Overview 汇总用户的余额、未付账单与在用服务数。
func (s *WalletService) Overview(userID uint) (*Overview, error) {
	var user model.User
	if err := s.db.Select("balance_cents").First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("用户不存在")
		}
		return nil, err
	}
	out := Overview{BalanceCents: user.BalanceCents}

	if err := s.db.Model(&model.Invoice{}).
		Where("user_id = ? AND status = ?", userID, model.InvoiceUnpaid).
		Count(&out.UnpaidCount).Error; err != nil {
		return nil, err
	}
	// COALESCE 保证没有未付账单时返回 0 而不是 NULL。
	if err := s.db.Model(&model.Invoice{}).
		Where("user_id = ? AND status = ?", userID, model.InvoiceUnpaid).
		Select("COALESCE(SUM(total_cents), 0)").Scan(&out.UnpaidCents).Error; err != nil {
		return nil, err
	}
	if err := s.db.Model(&model.Service{}).
		Where("user_id = ? AND status = ?", userID, model.ServiceActive).
		Count(&out.ServiceActive).Error; err != nil {
		return nil, err
	}
	return &out, nil
}

// Transactions 分页返回用户流水，按时间倒序。
func (s *WalletService) Transactions(userID uint, offset, limit int) ([]model.Transaction, int64, error) {
	var (
		items []model.Transaction
		total int64
	)
	if err := s.db.Model(&model.Transaction{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := s.db.Where("user_id = ?", userID).
		Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
