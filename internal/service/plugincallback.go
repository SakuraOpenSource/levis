package service

import (
	"errors"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
)

// PluginCallbackService 服务插件回主程序的读取类调用。
//
// 与 /api/open/v1 那批「按当前用户过滤」的读取分开成一个 service：那些方法
// 都以 userID 为过滤条件，语义是「读我自己的」；插件读的是任意用户的资料，
// 语义是「以系统身份读」。硬把 userID 参数复用过去，日后很容易有人误以为
// 已经做过归属校验。
type PluginCallbackService struct {
	db *gorm.DB
}

// NewPluginCallbackService 构造 PluginCallbackService。
func NewPluginCallbackService(db *gorm.DB) *PluginCallbackService {
	return &PluginCallbackService{db: db}
}

// PluginUser 是插件可见的用户资料。
//
// 刻意只有这四个字段：插件要的是「给谁发信」，不需要余额、角色或状态。
// 直接回 model.User 会把 BalanceCents 一并给出去，那是多给的信息。
type PluginUser struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Status   string `json:"status"`
}

// User 按 ID 返回用户资料。
func (s *PluginCallbackService) User(userID uint) (*PluginUser, error) {
	var user model.User
	err := s.db.Select("id", "username", "email", "status").First(&user, userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("用户不存在")
		}
		return nil, err
	}
	return &PluginUser{
		ID:       user.ID,
		Username: user.Username,
		Email:    user.Email,
		Status:   user.Status,
	}, nil
}

// Order 按 ID 返回订单及其明细。
//
// 不按用户过滤：插件在处理回调时手上只有渠道给的单号，未必知道是谁的订单。
// 这也正是 order:read 这个 scope 的边界所在 —— 勾了它就等于允许插件读任意
// 订单，管理界面上必须让管理员看清这一点。
func (s *PluginCallbackService) Order(orderID uint) (*model.Order, error) {
	var order model.Order
	err := s.db.Preload("Items").First(&order, orderID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("订单不存在")
		}
		return nil, err
	}
	return &order, nil
}
