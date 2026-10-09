package service

import (
	"fmt"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockFinancialWriter takes SQLite's writer reservation before any reads.
// SQLite ignores FOR UPDATE; upgrading a read transaction later can otherwise
// fail with SQLITE_BUSY instead of serializing the competing money paths.
func lockFinancialWriter(tx *gorm.DB, userID uint) error {
	if tx.Dialector.Name() != "sqlite" {
		return nil
	}
	return tx.Model(&model.User{}).Where("id = ?", userID).
		UpdateColumn("id", gorm.Expr("id")).Error
}

func paymentTargetKey(purpose string, id uint) string {
	return fmt.Sprintf("%s:%d", purpose, id)
}

type canonicalPaymentTarget struct {
	Key     string
	Order   *model.Order
	Service *model.Service
	Invoice *model.Invoice
	// quoteExpiry is the service expiry the preflight quoted against; the
	// reservation recheck must reject a window that moved in between.
	quoteExpiry time.Time
	// invoiceQuoteExpiry is the expiry seen when the invoice preflight ran.
	invoiceQuoteExpiry time.Time
}

// loadPaymentTarget resolves invoice aliases before locking the shared root.
// Root -> invoice -> intent is the lock order on MySQL/Postgres. SQLite reserves
// its writer first, because its dialect intentionally omits FOR UPDATE.
func loadPaymentTarget(tx *gorm.DB, userID uint, purpose string, targetID uint, lock bool) (*canonicalPaymentTarget, error) {
	if purpose == model.ExternalPaymentPurposeRecharge {
		return &canonicalPaymentTarget{}, nil
	}
	if lock {
		if err := lockFinancialWriter(tx, userID); err != nil {
			return nil, err
		}
	}
	read := tx
	if lock {
		read = tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	target := &canonicalPaymentTarget{Key: paymentTargetKey(purpose, targetID)}
	rootPurpose, rootID := purpose, targetID
	if purpose == model.ExternalPaymentPurposeInvoice {
		var invoice model.Invoice
		// The routing read takes no row lock: every caller locks the root first.
		if err := tx.Preload("Items").First(&invoice, "id = ? AND user_id = ?", targetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "账单")
		}
		target.Invoice = &invoice
		if invoice.OrderID != nil && *invoice.OrderID != 0 {
			rootPurpose, rootID = model.ExternalPaymentPurposeOrder, *invoice.OrderID
		} else if invoice.ServiceID != nil && *invoice.ServiceID != 0 {
			rootPurpose, rootID = model.ExternalPaymentPurposeRenewal, *invoice.ServiceID
		}
	}
	switch rootPurpose {
	case model.ExternalPaymentPurposeOrder:
		var order model.Order
		if err := read.First(&order, "id = ? AND user_id = ?", rootID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "订单")
		}
		target.Order, target.Key = &order, paymentTargetKey(rootPurpose, rootID)
	case model.ExternalPaymentPurposeRenewal:
		var svc model.Service
		if err := read.First(&svc, "id = ? AND user_id = ?", rootID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "服务")
		}
		target.Service, target.Key = &svc, paymentTargetKey(rootPurpose, rootID)
	case model.ExternalPaymentPurposeInvoice:
	default:
		return nil, ErrBadRequest("不支持的支付用途")
	}
	if target.Invoice != nil && lock {
		var current model.Invoice
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Items").First(&current, "id = ? AND user_id = ?", targetID, userID).Error; err != nil {
			return nil, paymentTargetError(err, "账单")
		}
		if !sameOptionalID(current.OrderID, target.Invoice.OrderID) || !sameOptionalID(current.ServiceID, target.Invoice.ServiceID) {
			return nil, ErrConflict("账单归属已变更")
		}
		target.Invoice = &current
	}
	if target.Invoice != nil && isTrafficInvoice(target.Invoice) {
		target.Key = paymentTargetKey(model.ExternalPaymentPurposeInvoice, target.Invoice.ID)
	}
	return target, nil
}

func sameOptionalID(a, b *uint) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func rejectPaymentIntentTx(tx *gorm.DB, userID uint, target *canonicalPaymentTarget, exceptID uint) error {
	if target.Key == "" {
		return nil
	}
	var invoices []model.Invoice
	var aliasPurpose string
	var aliasID uint
	switch {
	case target.Order != nil:
		aliasPurpose, aliasID = model.ExternalPaymentPurposeOrder, target.Order.ID
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", aliasID).Find(&invoices).Error; err != nil {
			return err
		}
	case target.Service != nil && (target.Invoice == nil || !isTrafficInvoice(target.Invoice)):
		aliasPurpose, aliasID = model.ExternalPaymentPurposeRenewal, target.Service.ID
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Items").Where("service_id = ?", aliasID).Find(&invoices).Error; err != nil {
			return err
		}
		filtered := invoices[:0]
		for _, invoice := range invoices {
			if !isTrafficInvoice(&invoice) && (invoice.OrderID == nil || *invoice.OrderID == 0) {
				filtered = append(filtered, invoice)
			}
		}
		invoices = filtered
	default:
		aliasPurpose, aliasID = model.ExternalPaymentPurposeInvoice, target.Invoice.ID
	}
	ids := make([]uint, 0, len(invoices))
	for _, invoice := range invoices {
		ids = append(ids, invoice.ID)
	}
	// Include legacy NULL-key intents by their authoritative associations. A
	// locking read sees newly committed rows under MySQL REPEATABLE READ.
	query := tx.Where("user_id = ? AND status IN ? AND id <> ?", userID,
		[]string{model.ExternalPaymentPending, model.ExternalPaymentProcessing}, exceptID).
		Where("active_target_key = ? OR (purpose = ? AND target_id = ?) OR (purpose = ? AND target_id IN ?)",
			target.Key, aliasPurpose, aliasID, model.ExternalPaymentPurposeInvoice, ids)
	var live []model.ExternalPayment
	if err := query.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&live).Error; err != nil {
		return err
	}
	if len(live) != 0 {
		return ErrConflict("该订单已有进行中的支付，请先完成或取消后再发起新的支付")
	}
	return nil
}

func (target *canonicalPaymentTarget) payableAmount(purpose string) (int64, error) {
	renewalQuoteExpiry := target.quoteExpiry
	invoiceQuoteExpiry := target.invoiceQuoteExpiry
	if target.Invoice != nil {
		if target.Invoice.Status != model.InvoiceUnpaid {
			return 0, ErrConflict("账单当前无需支付")
		}
		if target.Order != nil && target.Order.TotalCents != target.Invoice.TotalCents {
			return 0, ErrConflict("账单与订单金额不一致")
		}
		// A renewal/traffic invoice charges a service period or quota; its
		// amount was fixed when the invoice row was created, but the service
		// behind it may have changed since. Re-derive and compare (audit
		// MONEY-07): refuse to collect a stale amount for a moved window.
		if target.Service != nil && !isTrafficInvoice(target.Invoice) {
			if target.Service.Status != model.ServiceActive && !(target.Service.Status == model.ServiceSuspended && (target.Service.SuspendReason == "traffic" || target.Service.SuspendReason == "expired")) {
				return 0, ErrConflict("该服务当前不可续费")
			}
			if target.Service.ChangePendingID != nil {
				return 0, ErrConflict("该服务有进行中的规格更新，请等待其完成后再续费")
			}
			if target.Service.PriceCents != target.Invoice.TotalCents {
				return 0, ErrConflict("服务价格已变更，请重新开具账单")
			}
			if target.Service.ExpiresAt == nil || !target.Service.ExpiresAt.Equal(invoiceQuoteExpiry) {
				return 0, ErrConflict("服务到期时间已变更，请重新开具账单")
			}
		}
	}
	if target.Order != nil && target.Order.Status != model.OrderPending {
		return 0, ErrConflict("订单当前不可支付")
	}
	switch purpose {
	case model.ExternalPaymentPurposeInvoice:
		return target.Invoice.TotalCents, nil
	case model.ExternalPaymentPurposeOrder:
		return target.Order.TotalCents, nil
	case model.ExternalPaymentPurposeRenewal:
		if target.Service.Status != model.ServiceActive || target.Service.BillingCyc == model.CycleOneTime {
			return 0, ErrConflict("该服务当前不可续费")
		}
		if target.Service.ChangePendingID != nil {
			return 0, ErrConflict("该服务有进行中的规格更新，请等待其完成后再续费")
		}
		if target.Service.ExpiresAt == nil || !target.Service.ExpiresAt.Equal(renewalQuoteExpiry) {
			return 0, ErrConflict("服务到期时间已变更，请刷新后重试")
		}
		return target.Service.PriceCents, nil
	}
	return 0, ErrBadRequest("不支持的支付用途")
}

// lockOrderPaymentTx is the common lease for an order and every invoice alias.
func lockOrderPaymentTx(tx *gorm.DB, userID, orderID uint) error {
	_, err := loadPaymentTarget(tx, userID, model.ExternalPaymentPurposeOrder, orderID, true)
	return err
}

func rejectOrderPaymentIntentTx(tx *gorm.DB, userID, orderID, exceptID uint) error {
	target := &canonicalPaymentTarget{Key: paymentTargetKey(model.ExternalPaymentPurposeOrder, orderID), Order: &model.Order{Base: model.Base{ID: orderID}}}
	return rejectPaymentIntentTx(tx, userID, target, exceptID)
}
