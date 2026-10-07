package service

import (
	"github.com/SakuraOpenSource/levis/internal/model"
	"net"
	"regexp"
)

var productUplinkPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func normalizeProductNetwork(cfg *model.ProvisionSpec) error {
	if cfg.NetworkMode == "" {
		cfg.NetworkMode = "nat"
	}
	if cfg.NetworkMode != "nat" && cfg.NetworkMode != "dedicated" {
		return ErrBadRequest("商品网络模式仅支持 NAT 或独立地址")
	}
	if cfg.DedicatedMode != "" && cfg.DedicatedMode != "auto" && cfg.DedicatedMode != "routed" && cfg.DedicatedMode != "bridge" {
		return ErrBadRequest("独立网络模式仅支持 auto/routed/bridge")
	}
	if cfg.NetworkMode != "dedicated" && cfg.DedicatedMode != "" {
		return ErrBadRequest("NAT 商品不能指定独立网络模式")
	}
	if cfg.NetworkMode == "dedicated" && cfg.DedicatedMode == "" {
		cfg.DedicatedMode = "auto"
	}
	if cfg.NetworkBridge != "" && (!productUplinkPattern.MatchString(cfg.NetworkBridge) || cfg.NetworkBridge == "." || cfg.NetworkBridge == "..") {
		return ErrBadRequest("商品上联接口名无效")
	}
	if len(cfg.NetworkDNS) > 4 {
		return ErrBadRequest("最多配置 4 个 DNS 地址")
	}
	for _, dns := range cfg.NetworkDNS {
		if net.ParseIP(dns) == nil {
			return ErrBadRequest("DNS 地址无效")
		}
	}
	if len(cfg.SecurityGroupIDs) > 16 {
		return ErrBadRequest("最多绑定 16 个安全组")
	}
	seen := map[uint]bool{}
	for _, id := range cfg.SecurityGroupIDs {
		if id == 0 || seen[id] {
			return ErrBadRequest("安全组 ID 必须为唯一的正整数")
		}
		seen[id] = true
	}
	return nil
}
