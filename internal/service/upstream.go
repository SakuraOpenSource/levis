package service

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// UpstreamService 管理「接口」：同一个开通插件模块可以配置多个上游接口
// （不同的站点地址与密钥），商品选择接口即可完成开通。
//
// 与插件全局配置的区别：全局配置每个插件只有一份；接口是插件的多个
// 实例化入口，配置按请求透传给插件（proto 的 interface_config）。
type UpstreamService struct {
	db      *gorm.DB
	plugins *plugin.Manager
}

// NewUpstreamService 构造 UpstreamService。plugins 可为 nil（测试场景），
// 此时仅做 CRUD，连通性测试一律报插件不可用。
func NewUpstreamService(db *gorm.DB, plugins *plugin.Manager) *UpstreamService {
	return &UpstreamService{db: db, plugins: plugins}
}

// InterfaceInput 是接口的新增/修改入参。
type InterfaceInput struct {
	Name     string            `json:"name"`
	PluginID string            `json:"plugin_id"`
	Config   map[string]string `json:"config"`
}

// Interfaces 返回全部接口。
func (s *UpstreamService) Interfaces() ([]model.UpstreamInterface, error) {
	var items []model.UpstreamInterface
	err := s.db.Order("id ASC").Find(&items).Error
	return items, err
}

// Interface 读取单个接口。
func (s *UpstreamService) Interface(id uint) (*model.UpstreamInterface, error) {
	var item model.UpstreamInterface
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("接口不存在")
		}
		return nil, err
	}
	return &item, nil
}

// InterfaceForPlugin 是商品开通时的内部取用：返回接口并校验插件模块。
func (s *UpstreamService) InterfaceForPlugin(id uint) (*model.UpstreamInterface, error) {
	item, err := s.Interface(id)
	if err != nil {
		return nil, err
	}
	if s.plugins != nil {
		inst, err := s.plugins.Get(item.PluginID)
		if err != nil {
			return nil, ErrBadRequest("接口使用的插件 %s 不存在", item.PluginID)
		}
		if !inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT) {
			return nil, ErrBadRequest("插件 %s 不支持产品对接", item.PluginID)
		}
	}
	return item, nil
}

// Create 新增接口。
func (s *UpstreamService) Create(in InterfaceInput) (*model.UpstreamInterface, error) {
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	item := model.UpstreamInterface{
		Name:     in.Name,
		PluginID: in.PluginID,
		Config:   model.OptionMap(in.Config),
	}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// Update 修改接口。
func (s *UpstreamService) Update(id uint, in InterfaceInput) (*model.UpstreamInterface, error) {
	var item model.UpstreamInterface
	if err := s.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("接口不存在")
		}
		return nil, err
	}
	if err := s.validate(&in); err != nil {
		return nil, err
	}
	item.Name = in.Name
	item.PluginID = in.PluginID
	item.Config = model.OptionMap(in.Config)
	if err := s.db.Save(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// Delete 删除接口。仍有商品引用时拒绝，避免商品悬空。
func (s *UpstreamService) Delete(id uint) error {
	var products int64
	if err := s.db.Model(&model.Product{}).Where("interface_id = ?", id).Count(&products).Error; err != nil {
		return err
	}
	if products > 0 {
		return ErrConflict("仍有 %d 个商品使用该接口，请先调整商品", products)
	}
	result := s.db.Delete(&model.UpstreamInterface{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound("接口不存在")
	}
	return nil
}

// Test 通过插件发起一次轻量调用（拉取上游产品列表），验证接口配置可用。
// 虚拟机类上游通常没有「产品」概念，返回空列表也算成功 —— 只要上游
// 没有报错，就说明地址与密钥是对的。
func (s *UpstreamService) Test(id uint) error {
	item, err := s.InterfaceForPlugin(id)
	if err != nil {
		return err
	}
	if s.plugins == nil {
		return ErrBadRequest("插件系统未启用")
	}
	inst, err := s.plugins.Get(item.PluginID)
	if err != nil {
		return ErrBadRequest("插件 %s 不存在", item.PluginID)
	}
	client := inst.Client()
	if client == nil {
		return ErrBadRequest("插件 %s 未运行", item.PluginID)
	}
	reply, err := client.ListProducts(inst.TokenContext(context.Background()), &pb.ListProductsRequest{
		Page:            1,
		Limit:           1,
		InterfaceConfig: optionMapToProto(item.Config),
	})
	if err != nil {
		return ErrBadRequest("插件调用失败: %v", err)
	}
	if reply.GetError() != "" {
		return ErrBadRequest("上游返回错误: %s", reply.GetError())
	}
	return nil
}

// ProductOS 返回接口商品购买时可选的系统镜像：按商品绑定的接口调用插件的
// ListProductOS，以商品的驱动过滤。非接口商品直接报错 —— 购买页只对
// 接口商品展示系统选择。
func (s *UpstreamService) ProductOS(productID uint) ([]*pb.OSImage, error) {
	var product model.Product
	if err := s.db.First(&product, "id = ? AND status = ?", productID, model.ProductActive).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("商品不存在或已下架")
		}
		return nil, err
	}
	if product.InterfaceID == 0 {
		return nil, ErrBadRequest("该商品不支持选择系统")
	}
	iface, err := s.InterfaceForPlugin(product.InterfaceID)
	if err != nil {
		return nil, err
	}
	if s.plugins == nil {
		return nil, ErrBadRequest("插件系统未启用")
	}
	inst, err := s.plugins.Get(iface.PluginID)
	if err != nil {
		return nil, ErrBadRequest("插件 %s 不存在", iface.PluginID)
	}
	client := inst.Client()
	if client == nil {
		return nil, ErrBadRequest("插件 %s 未运行", iface.PluginID)
	}
	reply, err := client.ListProductOS(inst.TokenContext(context.Background()), &pb.ListProductOSRequest{
		ProductId:       strconv.FormatUint(uint64(productID), 10),
		InterfaceConfig: optionMapToProto(iface.Config),
		Options:         map[string]string{"driver": product.ProvisionConfig.Driver},
	})
	if err != nil {
		return nil, ErrBadRequest("获取系统列表失败: %v", err)
	}
	if reply.GetError() != "" {
		return nil, ErrBadRequest("上游返回错误: %s", reply.GetError())
	}
	return reply.GetOs(), nil
}

// validate 校验接口入参。
func (s *UpstreamService) validate(in *InterfaceInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return ErrBadRequest("接口名称不能为空")
	}
	in.PluginID = strings.TrimSpace(in.PluginID)
	if in.PluginID == "" {
		return ErrBadRequest("请选择接口使用的插件模块")
	}
	return nil
}

// optionMapToProto 把接口配置转成 proto map；空表返回 nil 保持线上形态干净。
func optionMapToProto(in model.OptionMap) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
