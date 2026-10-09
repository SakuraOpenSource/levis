package service

import (
	"context"
	"log"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

func (s *LifecycleService) retryRefundSuspensions(ctx context.Context) {
	var services []model.Service
	if err := s.db.Where("status = ? AND suspend_reason = ? AND refund_suspend_pending = ?",
		model.ServiceSuspended, suspendReasonRefund, true).Find(&services).Error; err != nil {
		log.Printf("refund suspension scan failed: %v", err)
		return
	}
	for i := range services {
		if ctx.Err() != nil {
			return
		}
		svc := &services[i]
		query := s.db.Model(&model.Service{}).Where("id = ? AND status = ? AND suspend_reason = ? AND refund_suspend_pending = ?",
			svc.ID, model.ServiceSuspended, suspendReasonRefund, true)
		if svc.UpstreamPluginID != "" && svc.UpstreamHostID != "" {
			if err := s.manageHost(ctx, svc, pb.HostAction_HOST_ACTION_SUSPEND); err != nil {
				// Suspension is idempotent; retain the outbox flag until the provider confirms revoked access.
				if update := query.Update("provision_error", truncateProvisionError(err.Error())); update.Error != nil {
					log.Printf("refund suspension diagnostic failed service=%d: %v", svc.ID, update.Error)
				}
				continue
			}
		}
		if err := query.Updates(map[string]any{"refund_suspend_pending": false, "provision_error": ""}).Error; err != nil {
			log.Printf("refund suspension acknowledgement failed service=%d: %v", svc.ID, err)
		}
	}
}
