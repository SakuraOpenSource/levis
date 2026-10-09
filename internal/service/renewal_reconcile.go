package service

import (
	"context"
	"fmt"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/notify"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"log"
	"time"
)

func (s *LifecycleService) WithNotifier(n *notify.Notifier) *LifecycleService {
	s.renewNotify = n.AutoRenew
	return s
}
func (s *LifecycleService) retryAutoRenewUpstream(ctx context.Context) {
	var events []model.RenewalEvent
	if s.db.Where("kind = ? AND upstream_state <> ?", "paid", "done").Find(&events).Error != nil {
		return
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		if event.UpstreamState == "sending" && time.Since(event.UpdatedAt) < 3*time.Minute {
			continue
		}
		claim := s.db.Model(&model.RenewalEvent{}).Where("id = ? AND upstream_state = ? AND updated_at = ?", event.ID, event.UpstreamState, event.UpdatedAt).Update("upstream_state", "sending")
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		var svc model.Service
		err := s.db.First(&svc, event.ServiceID).Error
		if err == nil && needsUpstreamRenew(&svc) {
			config, e := interfaceConfigForService(s.db, &svc)
			err = e
			if err == nil && s.host == nil {
				err = fmt.Errorf("上游插件不可用")
			}
			if err == nil {
				rctx, cancel := context.WithTimeout(ctx, 120*time.Second)
				reply, e := s.host.ManageHost(rctx, svc.UpstreamPluginID, &pb.ManageHostRequest{HostId: svc.UpstreamHostID, Action: pb.HostAction_HOST_ACTION_RENEW, BillingCycle: svc.BillingCyc, InterfaceConfig: config, OperationId: event.Key})
				cancel()
				err = e
				if err == nil && (reply == nil || !reply.GetSuccess()) {
					err = fmt.Errorf("上游续费未成功")
				}
			}
		}
		state, msg := "done", ""
		if err != nil {
			state, msg = "failed", truncateProvisionError(err.Error())
		}
		if e := s.db.Model(&model.RenewalEvent{}).Where("id = ? AND upstream_state = ?", event.ID, "sending").Updates(map[string]any{"upstream_state": state, "error": msg}).Error; e != nil {
			log.Printf("自动续费对账落库失败: %v", e)
			continue
		}
		if err != nil {
			_ = s.db.Model(&svc).Update("provision_error", msg).Error
			continue
		}
		if s.renewNotify != nil && event.UpstreamState == "pending" {
			s.renewNotify(svc.UserID, true, svc.Name, svc.PriceCents)
		}
		if svc.Status == model.ServiceSuspended && svc.SuspendReason == "expired" {
			resumeSuspended(s.db, s.host, &svc, "expired")
		}
	}
}
