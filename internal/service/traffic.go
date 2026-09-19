package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// UsedGB/QuotaGB 用浮点只用于展示；服务端权威值仍保留原始字节与整数 GB。
type TrafficProgress struct {
	UsedBytes     int64   `json:"used_bytes"`
	QuotaGB       int     `json:"quota_gb"`
	UsedGB        float64 `json:"used_gb"`
	Percent       float64 `json:"percent"`
	Unlimited     bool    `json:"unlimited"`
	Exceeded      bool    `json:"exceeded"`
	SuspendReason string  `json:"suspend_reason,omitempty"`
}

// TrafficProgress 读取当前用户服务的累计流量与总配额。
func (s *BillingService) TrafficProgress(userID, serviceID uint) (*TrafficProgress, error) {
	var svc model.Service
	if err := s.db.First(&svc, "id = ? AND user_id = ?", serviceID, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("服务不存在")
		}
		return nil, err
	}
	quota := TrafficQuotaTotalGB(s.db, &svc)
	usedGB := float64(svc.TrafficUsedBytes) / float64(bytesPerGiB)
	out := &TrafficProgress{
		UsedBytes:     svc.TrafficUsedBytes,
		QuotaGB:       quota,
		UsedGB:        usedGB,
		Unlimited:     quota <= 0,
		SuspendReason: svc.SuspendReason,
	}
	if quota > 0 {
		out.Percent = usedGB / float64(quota) * 100
		if out.Percent > 100 {
			out.Percent = 100
		}
		out.Exceeded = svc.TrafficUsedBytes >= int64(quota)*bytesPerGiB
	}
	return out, nil
}
