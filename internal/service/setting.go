package service

import (
	"encoding/json"
	"github.com/SakuraOpenSource/levis/internal/captcha"
	"github.com/SakuraOpenSource/levis/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"net/url"
	"strconv"
	"strings"
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

// KYCModeManual 是实名认证的人工审核模式：用户上传证件照，管理员比对。
const KYCModeManual = "manual"

// KYCMode 读取实名认证模式：manual 或实名认证插件 ID。
// 未配置或存了空值时回落人工审核 —— 模式缺失不该挡住实名流程。
func (s *SettingService) KYCMode() string {
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingKYCMode).Error; err != nil {
		return KYCModeManual
	}
	if value := strings.TrimSpace(row.Value); value != "" {
		return value
	}
	return KYCModeManual
}

// SaveKYCMode 保存实名认证模式。是否为可用的插件 ID 由调用方（handler）
// 对照当前插件列表校验，这里只管落库。
func (s *SettingService) SaveKYCMode(mode string) error {
	row := model.Setting{Key: model.SettingKYCMode, Value: strings.TrimSpace(mode)}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error
}

// boolSetting 把开关存成 "1"/"0"。设置表是纯字符串的键值表，
// 用固定字面量比依赖 strconv.FormatBool 的 "true"/"false" 更短也更稳。
func boolSetting(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// 以下是公开主页（landing page）与站点信息的设置。
//
// 主页配置以 JSON 整体存放在 home_config 键下，而不是拆成十几个键：
// 拆键会让「是否启用」与各字段的读写变成多行事务，整体存取则天然原子。
// 读取端对缺失或损坏的 JSON 一律回落到默认关闭，与验证码配置的容错思路一致。

// 主页字段的长度上限（按 rune 计）。超限的保存请求直接拒绝，由管理端提示。
const (
	HomeBadgeMaxLen        = 30
	HomeTitleMaxLen        = 60
	HomeSubtitleMaxLen     = 120
	HomeDescriptionMaxLen  = 500
	HomeButtonTextMaxLen   = 20
	HomeLinkMaxLen         = 500
	HomeImageURLMaxLen     = 1000
	HomeStatValueMaxLen    = 30
	HomeStatLabelMaxLen    = 30
	HomeFeatureTitleMaxLen = 30
	HomeFeatureDescMaxLen  = 200
	// HomeMaxStats 与 HomeMaxFeatures 是列表项数量上限。
	HomeMaxStats    = 4
	HomeMaxFeatures = 8
	// SiteNameMaxLen 与 SiteDescriptionMaxLen 是站点名称与简介的长度上限。
	SiteNameMaxLen        = 64
	SiteDescriptionMaxLen = 500
)

// HomeFeatureIcons 是特性卡片可选的图标取值表（lucide 图标名）。
// 前端据此渲染下拉框，后端保存时校验成员关系，两边必须保持一致。
var HomeFeatureIcons = []string{
	"zap", "rocket", "shield-check", "server", "cloud", "database",
	"cpu", "globe", "lock", "sparkles", "package", "credit-card",
}

// HomeButton 是主页上的一个行动按钮。Text 为空时前端不渲染该按钮。
type HomeButton struct {
	Text string `json:"text"`
	Link string `json:"link"`
}

// HomeStat 是主视觉下方的一条数据（如“99.9% 可用性”）。
type HomeStat struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// HomeFeature 是一张特性卡片。Icon 为 HomeFeatureIcons 中的图标名，
// Link 为空时卡片不可点击。
type HomeFeature struct {
	Icon  string `json:"icon"`
	Title string `json:"title"`
	Desc  string `json:"desc"`
	Link  string `json:"link"`
}

// HomeConfig 是公开主页的完整配置。
type HomeConfig struct {
	Enabled         bool          `json:"enabled"`
	Badge           string        `json:"badge"`
	Title           string        `json:"title"`
	Subtitle        string        `json:"subtitle"`
	Description     string        `json:"description"`
	PrimaryButton   HomeButton    `json:"primary_button"`
	SecondaryButton HomeButton    `json:"secondary_button"`
	HeroImageURL    string        `json:"hero_image_url"`
	Stats           []HomeStat    `json:"stats"`
	Features        []HomeFeature `json:"features"`
	ShowProducts    bool          `json:"show_products"`
}

// DefaultHomeConfig 返回主页的默认配置：关闭状态。
// 未配置过的主页不应把半成品推给访客，关闭后前端根路由回落到商店。
func DefaultHomeConfig() HomeConfig {
	return HomeConfig{ShowProducts: true}
}

// GetHomeConfig 读取主页配置。键缺失或 JSON 损坏时回落默认关闭，
// 手工改坏设置表不该把公开首页炸成 500。
func (s *SettingService) GetHomeConfig() HomeConfig {
	out := DefaultHomeConfig()
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingHomeConfig).Error; err != nil {
		return out
	}
	if strings.TrimSpace(row.Value) == "" {
		return out
	}
	var cfg HomeConfig
	if err := json.Unmarshal([]byte(row.Value), &cfg); err != nil {
		return out
	}
	return cfg.normalizeForRead()
}

// normalizeForRead 把手工塞进库里的越界列表截断，避免公开接口吐出超长数组。
// 字段长度不在这里收敛 —— 那是保存时的事，读侧只保上限不保内容合法。
func (c HomeConfig) normalizeForRead() HomeConfig {
	if len(c.Stats) > HomeMaxStats {
		c.Stats = c.Stats[:HomeMaxStats]
	}
	if len(c.Features) > HomeMaxFeatures {
		c.Features = c.Features[:HomeMaxFeatures]
	}
	if c.Stats == nil {
		c.Stats = []HomeStat{}
	}
	if c.Features == nil {
		c.Features = []HomeFeature{}
	}
	return c
}

// SaveHomeConfig 校验并保存主页配置，返回落库后的值。
func (s *SettingService) SaveHomeConfig(in HomeConfig) (HomeConfig, error) {
	cfg, err := in.trimmed()
	if err != nil {
		return HomeConfig{}, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return HomeConfig{}, err
	}
	row := model.Setting{Key: model.SettingHomeConfig, Value: string(raw)}
	if err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error; err != nil {
		return HomeConfig{}, err
	}
	return cfg, nil
}

// trimmed 清理输入并逐项校验，失败返回中文业务错误。
func (c HomeConfig) trimmed() (HomeConfig, error) {
	c.Badge = strings.TrimSpace(c.Badge)
	c.Title = strings.TrimSpace(c.Title)
	c.Subtitle = strings.TrimSpace(c.Subtitle)
	c.Description = strings.TrimSpace(c.Description)
	c.HeroImageURL = strings.TrimSpace(c.HeroImageURL)
	if l := runeLen(c.Badge); l > HomeBadgeMaxLen {
		return HomeConfig{}, ErrBadRequest("徽标文字最多 %d 个字", HomeBadgeMaxLen)
	}
	if l := runeLen(c.Title); l > HomeTitleMaxLen {
		return HomeConfig{}, ErrBadRequest("主标题最多 %d 个字", HomeTitleMaxLen)
	}
	if l := runeLen(c.Subtitle); l > HomeSubtitleMaxLen {
		return HomeConfig{}, ErrBadRequest("副标题最多 %d 个字", HomeSubtitleMaxLen)
	}
	if l := runeLen(c.Description); l > HomeDescriptionMaxLen {
		return HomeConfig{}, ErrBadRequest("主页简介最多 %d 个字", HomeDescriptionMaxLen)
	}
	if l := runeLen(c.HeroImageURL); l > HomeImageURLMaxLen {
		return HomeConfig{}, ErrBadRequest("主视觉图片地址过长")
	}
	if c.HeroImageURL != "" {
		if err := checkHomeLink(c.HeroImageURL, "主视觉图片地址"); err != nil {
			return HomeConfig{}, err
		}
	}
	var err error
	if c.PrimaryButton, err = trimHomeButton(c.PrimaryButton, "主按钮"); err != nil {
		return HomeConfig{}, err
	}
	if c.SecondaryButton, err = trimHomeButton(c.SecondaryButton, "次按钮"); err != nil {
		return HomeConfig{}, err
	}
	if len(c.Stats) > HomeMaxStats {
		return HomeConfig{}, ErrBadRequest("数据条目最多 %d 个", HomeMaxStats)
	}
	stats := make([]HomeStat, 0, len(c.Stats))
	for i := range c.Stats {
		item := HomeStat{Value: strings.TrimSpace(c.Stats[i].Value), Label: strings.TrimSpace(c.Stats[i].Label)}
		if item.Value == "" || item.Label == "" {
			return HomeConfig{}, ErrBadRequest("第 %d 条数据的值与名称均不能为空", i+1)
		}
		if runeLen(item.Value) > HomeStatValueMaxLen || runeLen(item.Label) > HomeStatLabelMaxLen {
			return HomeConfig{}, ErrBadRequest("第 %d 条数据的值与名称均最多 %d 个字", i+1, HomeStatValueMaxLen)
		}
		stats = append(stats, item)
	}
	c.Stats = stats
	if len(c.Features) > HomeMaxFeatures {
		return HomeConfig{}, ErrBadRequest("特性卡片最多 %d 个", HomeMaxFeatures)
	}
	features := make([]HomeFeature, 0, len(c.Features))
	for i := range c.Features {
		item := HomeFeature{
			Icon:  strings.TrimSpace(c.Features[i].Icon),
			Title: strings.TrimSpace(c.Features[i].Title),
			Desc:  strings.TrimSpace(c.Features[i].Desc),
			Link:  strings.TrimSpace(c.Features[i].Link),
		}
		if item.Title == "" {
			return HomeConfig{}, ErrBadRequest("第 %d 张卡片的标题不能为空", i+1)
		}
		if runeLen(item.Title) > HomeFeatureTitleMaxLen {
			return HomeConfig{}, ErrBadRequest("第 %d 张卡片的标题最多 %d 个字", i+1, HomeFeatureTitleMaxLen)
		}
		if runeLen(item.Desc) > HomeFeatureDescMaxLen {
			return HomeConfig{}, ErrBadRequest("第 %d 张卡片的描述最多 %d 个字", i+1, HomeFeatureDescMaxLen)
		}
		if !validHomeIcon(item.Icon) {
			return HomeConfig{}, ErrBadRequest("第 %d 张卡片的图标不在可选范围内", i+1)
		}
		if item.Link != "" {
			if l := runeLen(item.Link); l > HomeLinkMaxLen {
				return HomeConfig{}, ErrBadRequest("第 %d 张卡片的链接过长", i+1)
			}
			if err := checkHomeLink(item.Link, "特性卡片链接"); err != nil {
				return HomeConfig{}, err
			}
		}
		features = append(features, item)
	}
	c.Features = features
	return c, nil
}

// trimHomeButton 清理按钮输入。文字与链接都为空表示不展示该按钮；
// 只填了一边时要求文字必须有 —— 没有文字的按钮点不下去。
func trimHomeButton(b HomeButton, name string) (HomeButton, error) {
	b.Text = strings.TrimSpace(b.Text)
	b.Link = strings.TrimSpace(b.Link)
	if b.Text == "" && b.Link == "" {
		return b, nil
	}
	if b.Text == "" {
		return HomeButton{}, ErrBadRequest("%s的文字不能为空", name)
	}
	if l := runeLen(b.Text); l > HomeButtonTextMaxLen {
		return HomeButton{}, ErrBadRequest("%s的文字最多 %d 个字", name, HomeButtonTextMaxLen)
	}
	if l := runeLen(b.Link); l > HomeLinkMaxLen {
		return HomeButton{}, ErrBadRequest("%s的链接过长", name)
	}
	if b.Link != "" {
		if err := checkHomeLink(b.Link, name+"链接"); err != nil {
			return HomeButton{}, err
		}
	}
	return b, nil
}

// validHomeIcon 报告图标名是否在可选取值表内。
func validHomeIcon(icon string) bool {
	for _, allow := range HomeFeatureIcons {
		if icon == allow {
			return true
		}
	}
	return false
}

// checkHomeLink 校验主页里的链接与图片地址：只允许 http(s) 外链或站内相对路径。
// javascript:、data: 等伪协议与协议相对的 // 开头一律拒绝 —— 这些值最终会原样
// 进入 <a href> 与 <img src>，放行即 XSS 或开放重定向。
func checkHomeLink(link, name string) error {
	if strings.HasPrefix(link, "/") {
		if strings.HasPrefix(link, "//") || strings.ContainsAny(link, " \t\r\n") {
			return ErrBadRequest("%s格式不正确", name)
		}
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrBadRequest("%s须为 http(s) 地址或以 / 开头的站内路径", name)
	}
	return nil
}

// runeLen 按字符计数，用于中文场景的长度上限。
func runeLen(s string) int {
	return len([]rune(s))
}

// Site 返回站点名称与简介。安装时必写这两项，缺失时回落默认值。
func (s *SettingService) Site() (name, description string) {
	name, description = "Levis", ""
	var rows []model.Setting
	keys := []string{model.SettingSiteName, model.SettingSiteDescription}
	if err := s.db.Where(map[string]any{"key": keys}).Find(&rows).Error; err != nil {
		return name, description
	}
	for _, row := range rows {
		switch row.Key {
		case model.SettingSiteName:
			if strings.TrimSpace(row.Value) != "" {
				name = row.Value
			}
		case model.SettingSiteDescription:
			description = row.Value
		}
	}
	return name, description
}

// SiteIconPath 返回站点图标（favicon）的存储相对路径；空表示未设置。
func (s *SettingService) SiteIconPath() string {
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingSiteIcon).Error; err != nil {
		return ""
	}
	return row.Value
}

// SaveSiteIconPath 记录站点图标的存储路径。iconPath 为空表示清除图标。
func (s *SettingService) SaveSiteIconPath(iconPath string) error {
	row := model.Setting{Key: model.SettingSiteIcon, Value: iconPath}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error
}

// SaveSiteSettings 保存安装后可编辑的站点名称与简介，返回落库后的值。
func (s *SettingService) SaveSiteSettings(name, description string) (string, string, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" {
		return "", "", ErrBadRequest("站点名称不能为空")
	}
	if runeLen(name) > SiteNameMaxLen {
		return "", "", ErrBadRequest("站点名称最多 %d 个字", SiteNameMaxLen)
	}
	if runeLen(description) > SiteDescriptionMaxLen {
		return "", "", ErrBadRequest("站点简介最多 %d 个字", SiteDescriptionMaxLen)
	}
	rows := []model.Setting{
		{Key: model.SettingSiteName, Value: name},
		{Key: model.SettingSiteDescription, Value: description},
	}
	if err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&rows).Error; err != nil {
		return "", "", err
	}
	return name, description, nil
}

// TrafficPricePerGBMax 是流量包兜底单价的输入上限（分/GB）：100 万元/GB，
// 只为拦住手滑多打几个 0 的保存请求，正常业务远用不到。
const TrafficPricePerGBMax = 100_000_000

// TrafficPricePerGB 读取流量包兜底单价（分/GB）。
//
// 未配置、存了非法值或非正数时返回 0：0 表示未定价，计费侧
// （billing.trafficUnitPrice）据此拒绝流量加购而不是按 0 元放行。
func (s *SettingService) TrafficPricePerGB() int64 {
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingTrafficPricePerGB).Error; err != nil {
		return 0
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(row.Value), 10, 64); err == nil && v > 0 {
		return v
	}
	return 0
}

// SaveTrafficPricePerGB 保存流量包兜底单价（分/GB），0 表示清除定价。
func (s *SettingService) SaveTrafficPricePerGB(cents int64) error {
	if cents < 0 || cents > TrafficPricePerGBMax {
		return ErrBadRequest("流量包单价需在 0-%d 分之间", TrafficPricePerGBMax)
	}
	// 0 存空串：与“未配置”同义，读取端两个路径都会归一为 0。
	value := ""
	if cents > 0 {
		value = strconv.FormatInt(cents, 10)
	}
	row := model.Setting{Key: model.SettingTrafficPricePerGB, Value: value}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error
}

// LifecycleTerminateEnabled 读取生命周期删机开关；缺省关闭（干跑模式）。
func (s *SettingService) LifecycleTerminateEnabled() bool {
	var row model.Setting
	if err := s.db.First(&row, "key = ?", model.SettingLifecycleTerminate).Error; err != nil {
		return false
	}
	return row.Value == "1"
}

// SaveLifecycleTerminateEnabled 保存生命周期删机开关。
// 开启前建议先在日志里观察干跑清单，确认无误删风险。
func (s *SettingService) SaveLifecycleTerminateEnabled(enabled bool) error {
	value := ""
	if enabled {
		value = "1"
	}
	row := model.Setting{Key: model.SettingLifecycleTerminate, Value: value}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&row).Error
}
