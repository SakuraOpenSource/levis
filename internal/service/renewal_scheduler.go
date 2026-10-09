package service

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"log"
	"strconv"
	"time"
)

func (s *LifecycleService) autoRenew(ctx context.Context) {
	if s.dryRun() {
		return
	}
	now := time.Now().UTC()
	window := 24 * time.Hour
	var row model.Setting
	if err := s.db.Where(map[string]any{"key": model.SettingAutoRenewWindowHours}).First(&row).Error; err == nil {
		if hours, err := strconv.Atoi(row.Value); err == nil && hours >= 1 && hours <= 168 {
			window = time.Duration(hours) * time.Hour
		}
	}
	var candidates []model.Service
	if err := s.db.Where("auto_renew = ? AND expires_at IS NOT NULL AND status IN ?", true, []string{model.ServiceActive, model.ServiceSuspended}).Find(&candidates).Error; err != nil {
		log.Printf("自动续费读取失败: %v", err)
		return
	}
	for _, snapshot := range candidates {
		if ctx.Err() != nil {
			return
		}
		if snapshot.ExpiresAt.After(now.Add(window)) {
			continue
		}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			billing := NewBillingService(tx, NewWalletService(tx), nil)
			svc, err := billing.loadRenewableService(tx, snapshot.UserID, snapshot.ID)
			if err != nil {
				return nil
			}
			if !svc.AutoRenew || svc.ExpiresAt == nil || !svc.ExpiresAt.Equal(*snapshot.ExpiresAt) || svc.ExpiresAt.After(now.Add(window)) || svc.BillingCyc == model.CycleOneTime || !model.ValidCycle(svc.BillingCyc) {
				return nil
			}
			var invoices []model.Invoice
			if err = tx.Preload("Items").Where("service_id = ? AND status = ?", svc.ID, model.InvoiceUnpaid).Find(&invoices).Error; err != nil {
				return err
			}
			for i := range invoices {
				if !isTrafficInvoice(&invoices[i]) {
					return nil
				}
			}
			var pending int64
			if err = tx.Model(&model.ExternalPayment{}).Where("purpose = ? AND target_id = ? AND status IN ?", model.ExternalPaymentPurposeRenewal, svc.ID, []string{model.ExternalPaymentPending, model.ExternalPaymentProcessing}).Count(&pending).Error; err != nil {
				return err
			}
			if pending > 0 {
				return nil
			}
			key := fmt.Sprintf("auto-renew/%d/%d", svc.ID, svc.ExpiresAt.UnixNano())
			event := model.RenewalEvent{ServiceID: svc.ID, UserID: svc.UserID, Key: key, Kind: "paid", UpstreamState: "pending"}
			claim := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&event)
			if claim.Error != nil {
				return claim.Error
			}
			if claim.RowsAffected != 1 {
				return nil
			}
			if svc.PriceCents < 0 {
				return ErrBadRequest("续费价格无效")
			}
			if svc.PriceCents > 0 {
				if _, err = billing.wallet.adjustBalance(tx, svc.UserID, -svc.PriceCents, model.TxPayment, "service", svc.ID, "自动续费 "+svc.Name); err != nil {
					return err
				}
			}
			if _, err = billing.applyRenewalTx(tx, svc, svc.UserID, now); err != nil {
				return err
			}
			no, err := serialNo("INV")
			if err != nil {
				return err
			}
			invoice := model.Invoice{InvoiceNo: no, UserID: svc.UserID, ServiceID: &svc.ID, Status: model.InvoicePaid, TotalCents: svc.PriceCents, DueAt: &now, PaidAt: &now}
			if err = tx.Create(&invoice).Error; err != nil {
				return err
			}
			if err = tx.Create(&model.InvoiceItem{InvoiceID: invoice.ID, ServiceID: &svc.ID, Description: "自动续费 " + svc.Name, AmountCents: svc.PriceCents}).Error; err != nil {
				return err
			}
			return tx.Model(&event).Update("invoice_id", invoice.ID).Error
		})
		if err != nil {
			log.Printf("自动续费失败 service=%d: %v", snapshot.ID, err)
			event := model.RenewalEvent{ServiceID: snapshot.ID, UserID: snapshot.UserID, Key: fmt.Sprintf("auto-renew/%d/%d/failed", snapshot.ID, snapshot.ExpiresAt.UnixNano()), Kind: "failed", Error: truncateProvisionError(err.Error())}
			claim := s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&event)
			if claim.Error == nil && claim.RowsAffected == 1 && s.renewNotify != nil {
				s.renewNotify(snapshot.UserID, false, snapshot.Name, snapshot.PriceCents)
			}
			continue
		}
	}
	s.retryAutoRenewUpstream(ctx)
}
