package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"unicode"

	"github.com/SakuraOpenSource/levis/internal/model"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
	"gorm.io/gorm"
)

func validateSecurityGroupBindingPayload(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return ErrBadRequest("安全组参数必须为 JSON 对象")
	}
	if !d.More() {
		return ErrBadRequest("缺少 security_group_ids")
	}
	if key, err := d.Token(); err != nil || key != "security_group_ids" {
		return ErrBadRequest("不允许的安全组参数")
	}
	var ids []uint64
	if d.Decode(&ids) != nil || ids == nil || len(ids) > 16 || d.More() {
		return ErrBadRequest("安全组 ID 列表无效、重复字段或超过 16 个")
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return ErrBadRequest("安全组 JSON 无效")
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrBadRequest("安全组 JSON 含多余内容")
	}
	seen := map[uint64]bool{}
	for _, id := range ids {
		if id == 0 || seen[id] {
			return ErrBadRequest("安全组 ID 必须为唯一的正整数")
		}
		seen[id] = true
	}
	return nil
}

// requiredSecurityGroupIDs 返回订单快照里商品要求的安全组：改绑不能移除。
func requiredSecurityGroupIDs(db *gorm.DB, svc *model.Service) map[uint64]bool {
	required := map[uint64]bool{}
	if svc.OrderID == 0 || db == nil {
		return required
	}
	items := make([]model.OrderItem, 0, 1)
	if err := db.Where("order_id = ? AND product_id = ?", svc.OrderID, svc.ProductID).Find(&items).Error; err != nil {
		return required
	}
	for _, item := range items {
		for _, raw := range strings.Split(item.Options["security_group_ids"], ",") {
			if id, err := parseStrictUint64(strings.TrimSpace(raw)); err == nil && id > 0 {
				required[id] = true
			}
		}
	}
	return required
}

func parseStrictUint64(raw string) (uint64, error) {
	if raw == "" {
		return 0, ErrBadRequest("空 ID")
	}
	var id uint64
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return 0, err
	}
	return id, nil
}

// InterfaceSecurityGroups 仅供管理员接口目录使用，不接受外部 path/method。
func (s *HostFeatureService) InterfaceSecurityGroups(ctx context.Context, id uint) (json.RawMessage, error) {
	iface, err := NewUpstreamService(s.db, nil).Interface(id)
	if err != nil {
		return nil, err
	}
	if s.host == nil {
		return nil, ErrBadRequest("插件系统未启用")
	}
	reply, err := s.host.HostOperation(ctx, iface.PluginID, &pb.HostOperationRequest{Action: "security_groups_list", PayloadJson: "{}", InterfaceConfig: map[string]string(iface.Config)})
	if err != nil {
		return nil, err
	}
	if reply == nil || reply.Error != "" {
		return nil, ErrBadRequest("无法获取上游安全组")
	}
	raw := json.RawMessage(reply.DataJson)
	if len(raw) > 4*1024*1024 || !json.Valid(raw) {
		return nil, ErrBadRequest("上游安全组列表无效")
	}
	var page struct {
		Items []securityGroupChoice `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Items == nil || len(page.Items) > 1000 {
		return nil, ErrBadRequest("上游安全组列表无效")
	}
	seen := map[uint64]bool{}
	for i := range page.Items {
		group := &page.Items[i]
		if group.ID == 0 || seen[group.ID] || group.Name == "" || len(group.Name) > 64 || len(group.Description) > 1024 || strings.IndexFunc(group.Name+group.Description, unicode.IsControl) >= 0 || !groupPolicyValid(group.IngressPolicy) || !groupPolicyValid(group.EgressPolicy) {
			return nil, ErrBadRequest("上游安全组字段无效")
		}
		seen[group.ID] = true
		for _, secret := range []string{iface.Config["api_key"], iface.Config["api_url"]} {
			if secret != "" {
				group.Name = strings.ReplaceAll(group.Name, secret, "[redacted]")
				group.Description = strings.ReplaceAll(group.Description, secret, "[redacted]")
			}
		}
	}
	return json.Marshal(page)
}

type securityGroupChoice struct {
	ID            uint64 `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	IngressPolicy string `json:"ingress_policy"`
	EgressPolicy  string `json:"egress_policy"`
}

func groupPolicyValid(value string) bool { return value == "accept" || value == "drop" }
