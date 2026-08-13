package service

import (
	"errors"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/auth"
	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/database"
	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/runtime"
)

// InstallRequest 是安装请求。
type InstallRequest struct {
	Database        config.Database `json:"database"`
	SiteName        string          `json:"site_name"`
	SiteDescription string          `json:"site_description"`
	AdminUsername   string          `json:"admin_username"`
	AdminEmail      string          `json:"admin_email"`
	AdminPassword   string          `json:"admin_password"`
}

// InstallService 处理安装流程。
type InstallService struct {
	rt *runtime.Runtime
}

// NewInstallService 构造 InstallService。
func NewInstallService(rt *runtime.Runtime) *InstallService {
	return &InstallService{rt: rt}
}

// TestDatabase 验证数据库连接参数可用，不做任何持久化。
func (s *InstallService) TestDatabase(db config.Database) error {
	s.normalizeDatabase(&db)
	if err := db.Validate(); err != nil {
		return ErrBadRequest("%s", err.Error())
	}
	if err := database.TestConnection(db); err != nil {
		return ErrBadRequest("%s", err.Error())
	}
	return nil
}

// Install 执行安装：建库、迁移、写入站点设置与管理员账号、保存配置，
// 最后原子激活运行时。任一步失败都不会写下配置文件。
func (s *InstallService) Install(req InstallRequest) error {
	if s.rt.Installed() {
		return ErrConflict("程序已完成安装")
	}

	s.normalizeDatabase(&req.Database)
	req.SiteName = strings.TrimSpace(req.SiteName)
	req.SiteDescription = strings.TrimSpace(req.SiteDescription)
	req.AdminUsername = strings.TrimSpace(req.AdminUsername)

	if req.SiteName == "" {
		return ErrBadRequest("站点名称不能为空")
	}
	if err := ValidateUsername(req.AdminUsername); err != nil {
		return err
	}
	email, err := ValidateEmail(req.AdminEmail)
	if err != nil {
		return err
	}
	if err := ValidatePassword(req.AdminPassword); err != nil {
		return err
	}
	if err := req.Database.Validate(); err != nil {
		return ErrBadRequest("%s", err.Error())
	}

	// 先连库，连不上就直接失败，避免留下不可用的配置。
	db, err := database.Open(req.Database)
	if err != nil {
		return ErrBadRequest("%s", err.Error())
	}
	if err := database.Migrate(db); err != nil {
		return ErrBadRequest("%s", err.Error())
	}

	// 目标库可能是已装过 Levis 的旧库，此时不应覆盖既有数据。
	var existing int64
	if err := db.Model(&model.User{}).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return ErrConflict("该数据库中已存在用户数据，请更换数据库或删除 data/config.json 后重试")
	}

	hash, err := auth.HashPassword(req.AdminPassword)
	if err != nil {
		return err
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		admin := model.User{
			Username:     req.AdminUsername,
			Email:        email,
			PasswordHash: hash,
			Role:         model.RoleAdmin,
			Status:       model.UserActive,
		}
		if err := tx.Create(&admin).Error; err != nil {
			return err
		}
		settings := []model.Setting{
			{Key: model.SettingSiteName, Value: req.SiteName},
			{Key: model.SettingSiteDescription, Value: req.SiteDescription},
		}
		return tx.Create(&settings).Error
	})
	if err != nil {
		return err
	}

	secret, err := config.GenerateSecret()
	if err != nil {
		return err
	}
	cfg := &config.Config{
		Database:  req.Database,
		JWTSecret: secret,
		Listen:    config.DefaultListen,
	}
	if err := config.Save(s.rt.DataDir(), cfg); err != nil {
		return err
	}

	s.rt.Activate(cfg, db)
	return nil
}

// normalizeDatabase 补齐默认值并清理输入。
func (s *InstallService) normalizeDatabase(db *config.Database) {
	db.Driver = strings.ToLower(strings.TrimSpace(db.Driver))
	db.Host = strings.TrimSpace(db.Host)
	db.User = strings.TrimSpace(db.User)
	db.Name = strings.TrimSpace(db.Name)
	db.Path = strings.TrimSpace(db.Path)

	switch db.Driver {
	case config.DriverSQLite:
		// 默认库文件必须落在数据目录内，而不是相对当前工作目录 ——
		// 否则从不同目录启动会连到不同的库，-data 参数也形同虚设。
		if db.Path == "" {
			db.Path = filepath.Join(s.rt.DataDir(), "levis.db")
		}
		// SQLite 用不到网络参数，清空避免写进配置文件。
		db.Host, db.Port, db.User, db.Password, db.Name = "", 0, "", "", ""
	case config.DriverMySQL:
		if db.Port == 0 {
			db.Port = 3306
		}
	case config.DriverPostgres:
		if db.Port == 0 {
			db.Port = 5432
		}
	}
}

// Bootstrap 是前端启动时获取的站点基础信息。
type Bootstrap struct {
	Installed       bool   `json:"installed"`
	SiteName        string `json:"site_name"`
	SiteDescription string `json:"site_description"`
}

// Bootstrap 返回安装状态与站点信息，供前端路由守卫使用。
func (s *InstallService) Bootstrap() Bootstrap {
	out := Bootstrap{Installed: s.rt.Installed()}
	if !out.Installed {
		return out
	}
	var settings []model.Setting
	if err := s.rt.DB().Find(&settings).Error; err != nil {
		return out
	}
	for _, item := range settings {
		switch item.Key {
		case model.SettingSiteName:
			out.SiteName = item.Value
		case model.SettingSiteDescription:
			out.SiteDescription = item.Value
		}
	}
	return out
}

// SiteName 返回站点名称，读取失败时返回 "Levis"。
func SiteName(db *gorm.DB) string {
	var setting model.Setting
	if err := db.First(&setting, "key = ?", model.SettingSiteName).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "Levis"
		}
		return "Levis"
	}
	if setting.Value == "" {
		return "Levis"
	}
	return setting.Value
}
