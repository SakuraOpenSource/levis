package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// resolvePluginForProduct 返回商品开通使用的插件 ID 与接口配置。
//
// 接口商品（InterfaceID != 0）以接口为准：插件取接口的模块，配置取接口
// 保存的键值，按请求透传；传统上游商品沿用商品上的插件 ID，无接口配置。
func resolvePluginForProduct(db *gorm.DB, product *model.Product) (string, map[string]string, error) {
	if product.InterfaceID != 0 {
		var iface model.UpstreamInterface
		if err := db.First(&iface, product.InterfaceID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", nil, ErrBadRequest("商品绑定的接口不存在")
			}
			return "", nil, err
		}
		return iface.PluginID, map[string]string(iface.Config), nil
	}
	if product.UpstreamPluginID == "" {
		return "", nil, ErrBadRequest("该商品未绑定上游")
	}
	return product.UpstreamPluginID, nil, nil
}

// createUpstreamOrder 是商品开通的唯一入口：向上游插件下单并等待开通，
// 返回上游服务实例 ID 与到期时间。购物车/直购订单与管理员代开共用。
func createUpstreamOrder(plugins *plugin.Manager, db *gorm.DB, product *model.Product, cycle, orderNo, clientEmail string, options map[string]string) (string, *time.Time, error) {
	if plugins == nil {
		return "", nil, ErrBadRequest("上游插件不可用，无法开通该商品")
	}
	pluginID, ifaceConfig, err := resolvePluginForProduct(db, product)
	if err != nil {
		return "", nil, err
	}

	reply, err := plugins.CreateOrder(context.Background(), pluginID, &pb.CreateOrderRequest{
		ProductId:       product.UpstreamProductID,
		BillingCycle:    cycle,
		Quantity:        1,
		ClientEmail:     clientEmail,
		Remark:          orderNo,
		InterfaceConfig: ifaceConfig,
		Options:         options,
	})
	if err != nil {
		return "", nil, ErrBadRequest("上游开通失败: %v", err)
	}
	hostID := reply.GetUpstreamOrderId()
	if hostID == "" {
		return "", nil, ErrBadRequest("上游开通失败: 未返回服务实例 ID")
	}

	// 拉取上游服务信息，同步到期时间；失败不阻断开通。
	var expiry *time.Time
	if host, err := plugins.GetHost(context.Background(), pluginID, &pb.GetHostRequest{
		HostId:          hostID,
		InterfaceConfig: ifaceConfig,
	}); err == nil {
		if e := host.GetHost().GetExpiry(); e != "" {
			if t, err := time.Parse(time.RFC3339, e); err == nil {
				expiry = &t
			}
		}
	}
	return hostID, expiry, nil
}

// defaultProvisionOptions 为没有用户选配的场景（管理员代开）推导开通选配：
// 弹性规格取下限，固定规格取固定值。系统镜像由插件按驱动自选。
func defaultProvisionOptions(cfg model.ProvisionSpec) map[string]string {
	options := map[string]string{
		"driver":    cfg.Driver,
		"cpu":       strconv.Itoa(specValue(cfg.CPU)),
		"memory_mb": strconv.Itoa(specValue(cfg.MemoryMB)),
		"disk_gb":   strconv.Itoa(specValue(cfg.DiskGB)),
	}
	if v := specValue(cfg.BandwidthMbps); v > 0 {
		options["bandwidth_mbps"] = strconv.Itoa(v)
	}
	if v := specValue(cfg.TrafficGB); v > 0 {
		options["traffic_gb"] = strconv.Itoa(v)
	}
	return options
}

// specValue 取规格值：固定取 Min，弹性取下限。
func specValue(r model.SpecRange) int {
	if r.Min > 0 {
		return r.Min
	}
	return r.Max
}

// interfaceConfigForService 解析已购服务对应的接口配置（续费、电源操作、
// 上游信息查询都要透传给插件）。非接口商品返回 nil。
func interfaceConfigForService(db *gorm.DB, svc *model.Service) (map[string]string, error) {
	var product model.Product
	if err := db.First(&product, svc.ProductID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if product.InterfaceID == 0 {
		return nil, nil
	}
	var iface model.UpstreamInterface
	if err := db.First(&iface, product.InterfaceID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBadRequest("商品绑定的接口不存在")
		}
		return nil, err
	}
	return map[string]string(iface.Config), nil
}
