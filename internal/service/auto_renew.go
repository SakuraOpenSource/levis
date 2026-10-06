package service

import (
	"github.com/SakuraOpenSource/levis/internal/model"
)

func (s *BillingService) SetAutoRenew(userID, serviceID uint, enabled bool) (*model.Service, error) {
	svc, err := s.Service(userID, serviceID)
	if err != nil {
		return nil, err
	}
	if enabled {
		if svc.BillingCyc == model.CycleOneTime || !model.ValidCycle(svc.BillingCyc) {
			return nil, ErrBadRequest("该服务无需续费")
		}
		if svc.Status != model.ServiceActive && !(svc.Status == model.ServiceSuspended && svc.SuspendReason == "expired") {
			return nil, ErrConflict("该服务不可自动续费")
		}
	}
	if err = s.db.Model(svc).Update("auto_renew", enabled).Error; err != nil {
		return nil, err
	}
	svc.AutoRenew = enabled
	return svc, nil
}
