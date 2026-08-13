package service

import (
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// UserService 处理注册、登录与账号自助维护。
type UserService struct {
	db *gorm.DB
}

// NewUserService 构造 UserService。
func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db}
}

// RegisterRequest 是注册请求。
//
// 注意这里没有 Role 与 BalanceCents 字段：请求体绝不能直接绑定到 model.User，
// 否则客户端可以传 role=admin 自我提权。新用户角色一律由服务端写死。
type RegisterRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Register 创建普通用户。
func (s *UserService) Register(req RegisterRequest) (*model.User, error) {
	req.Username = strings.TrimSpace(req.Username)
	if err := ValidateUsername(req.Username); err != nil {
		return nil, err
	}
	email, err := ValidateEmail(req.Email)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}

	// 先查一次给出友好提示；唯一索引仍是最终防线（并发注册时靠它兜底）。
	var count int64
	if err := s.db.Model(&model.User{}).
		Where("username = ? OR email = ?", req.Username, email).
		Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrConflict("用户名或邮箱已被注册")
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	user := model.User{
		Username:     req.Username,
		Email:        email,
		PasswordHash: hash,
		Role:         model.RoleUser, // 固定为普通用户
		Status:       model.UserActive,
	}
	if err := s.db.Create(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, ErrConflict("用户名或邮箱已被注册")
		}
		return nil, err
	}
	return &user, nil
}

// Login 校验凭证。管理员与普通用户共用此入口。
func (s *UserService) Login(identifier, password string) (*model.User, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || password == "" {
		return nil, ErrBadRequest("请输入账号与密码")
	}

	var user model.User
	err := s.db.First(&user, "username = ? OR email = ?", identifier, strings.ToLower(identifier)).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 不区分「用户不存在」与「密码错误」，避免账号枚举。
			return nil, ErrUnauthorized("账号或密码错误")
		}
		return nil, err
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		return nil, ErrUnauthorized("账号或密码错误")
	}
	if user.Status != model.UserActive {
		return nil, ErrForbidden("账号已被禁用")
	}
	return &user, nil
}

// ChangeEmail 修改当前用户邮箱，需验证当前密码。
func (s *UserService) ChangeEmail(userID uint, password, newEmail string) error {
	user, err := s.requireUser(userID)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(user.PasswordHash, password) {
		return ErrForbidden("当前密码不正确")
	}
	email, err := ValidateEmail(newEmail)
	if err != nil {
		return err
	}
	if email == user.Email {
		return ErrBadRequest("新邮箱与当前邮箱相同")
	}

	var count int64
	if err := s.db.Model(&model.User{}).
		Where("email = ? AND id <> ?", email, userID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrConflict("该邮箱已被使用")
	}
	return s.db.Model(&model.User{}).Where("id = ?", userID).Update("email", email).Error
}

// ChangePassword 修改当前用户密码，需验证当前密码。
func (s *UserService) ChangePassword(userID uint, oldPassword, newPassword string) error {
	user, err := s.requireUser(userID)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(user.PasswordHash, oldPassword) {
		return ErrForbidden("当前密码不正确")
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	if auth.CheckPassword(user.PasswordHash, newPassword) {
		return ErrBadRequest("新密码与当前密码相同")
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.db.Model(&model.User{}).Where("id = ?", userID).Update("password_hash", hash).Error
}

// Get 按 ID 读取用户。
func (s *UserService) Get(userID uint) (*model.User, error) {
	return s.requireUser(userID)
}

// requireUser 读取用户，不存在时返回 404 业务错误。
func (s *UserService) requireUser(userID uint) (*model.User, error) {
	var user model.User
	if err := s.db.First(&user, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("用户不存在")
		}
		return nil, err
	}
	return &user, nil
}
