package service

import (
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/SakuraOpenSource/levis/internal/captcha"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// CaptchaConfig 是验证码的站点级配置。
type CaptchaConfig struct {
	LoginEnabled    bool `json:"login_enabled"`
	RegisterEnabled bool `json:"register_enabled"`
	// Charset 取 captcha.CharsetMixed / CharsetDigit / CharsetLetter。
	Charset string `json:"charset"`
	Length  int    `json:"length"`
}

// DefaultCaptchaConfig 是未配置时的默认值。
//
// 注册默认开启、登录默认关闭：注册是脚本批量刷号的入口，拦一道成本很低；
// 登录每天要走很多遍，为已有用户平白加一步不划算，需要时管理员自己开。
func DefaultCaptchaConfig() CaptchaConfig {
	return CaptchaConfig{
		LoginEnabled:    false,
		RegisterEnabled: true,
		Charset:         captcha.CharsetDigit,
		Length:          captcha.DefaultLength,
	}
}

// SettingService 读写站点级设置。
type SettingService struct {
	db *gorm.DB
}

// NewSettingService 构造 SettingService。
func NewSettingService(db *gorm.DB) *SettingService {
	return &SettingService{db: db}
}

// Captcha 读取验证码配置。
//
// 任何一项缺失或存了非法值都回落到该项的默认值，而不是整体报错：设置表被
// 手工改坏时，登录注册仍应可用。
func (s *SettingService) Captcha() CaptchaConfig {
	out := DefaultCaptchaConfig()
	keys := []string{
		model.SettingCaptchaLogin,
		model.SettingCaptchaRegister,
		model.SettingCaptchaCharset,
		model.SettingCaptchaLength,
	}
	var rows []model.Setting
	// 用 map 形式而不是 `key IN ?` 字符串条件：key 是 MySQL 的保留字，
	// 只有走 map/结构体条件 GORM 才会按方言给列名加引号。
	if err := s.db.Where(map[string]any{"key": keys}).Find(&rows).Error; err != nil {
		return out
	}
	for _, row := range rows {
		switch row.Key {
		case model.SettingCaptchaLogin:
			out.LoginEnabled = row.Value == "1"
		case model.SettingCaptchaRegister:
			out.RegisterEnabled = row.Value == "1"
		case model.SettingCaptchaCharset:
			if captcha.ValidCharset(row.Value) {
				out.Charset = row.Value
			}
		case model.SettingCaptchaLength:
			if n, err := strconv.Atoi(row.Value); err == nil {
				out.Length = captcha.ClampLength(n)
			}
		}
	}
	return out
}

// SaveCaptcha 保存验证码配置并返回落库后的值。
func (s *SettingService) SaveCaptcha(in CaptchaConfig) (CaptchaConfig, error) {
	in.Charset = strings.TrimSpace(in.Charset)
	if !captcha.ValidCharset(in.Charset) {
		return CaptchaConfig{}, ErrBadRequest("无效的验证码类型")
	}
	if in.Length < captcha.MinLength || in.Length > captcha.MaxLength {
		return CaptchaConfig{}, ErrBadRequest("验证码位数需在 %d-%d 之间", captcha.MinLength, captcha.MaxLength)
	}

	rows := []model.Setting{
		{Key: model.SettingCaptchaLogin, Value: boolSetting(in.LoginEnabled)},
		{Key: model.SettingCaptchaRegister, Value: boolSetting(in.RegisterEnabled)},
		{Key: model.SettingCaptchaCharset, Value: in.Charset},
		{Key: model.SettingCaptchaLength, Value: strconv.Itoa(in.Length)},
	}
	// upsert 而不是先查后写：设置项可能从未写入过（默认值不落库），
	// 而 GORM 的 OnConflict 在三种数据库上都能翻译成对应的原生语法。
	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&rows).Error
	if err != nil {
		return CaptchaConfig{}, err
	}
	return in, nil
}

// boolSetting 把开关存成 "1"/"0"。设置表是纯字符串的键值表，
// 用固定字面量比依赖 strconv.FormatBool 的 "true"/"false" 更短也更稳。
func boolSetting(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
