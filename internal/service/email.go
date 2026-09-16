package service

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/mailer"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// 邮箱验证码的关键参数：
//
//	codeTTL         验证码有效期；
//	codeMaxTTL      登录票据有效期；
//	codeMaxAttempts 单个验证码最大校验次数（防爆破）；
//	resendInterval  同一邮箱同一场景两次发码的最小间隔；
//	hourlyCap       同一邮箱同一场景每小时发码上限。
const (
	codeTTL         = 10 * time.Minute
	codeMaxTTL      = 5 * time.Minute
	codeMaxAttempts = 5
	resendInterval  = 60 * time.Second
	hourlyCap       = 5
)

// 邮箱验证码场景。
const (
	EmailSceneRegister = "register"
	EmailSceneLogin    = "login"
)

// Setting 键名：SMTP 与邮箱验证码开关。
const (
	SettingSMTPHost          = "smtp_host"
	SettingSMTPPort          = "smtp_port"
	SettingSMTPSSL           = "smtp_ssl"
	SettingSMTPUsername      = "smtp_username"
	SettingSMTPPassword      = "smtp_password"
	SettingSMTPFrom          = "smtp_from"
	SettingEmailCodeRegister = "email_code_register"
	SettingEmailCodeLogin    = "email_code_login"
)

// EmailSettings 是管理后台读写的邮件配置。
//
// 密码只写不读：读取端永远只给 has_password，避免把凭据回显给浏览器。
type EmailSettings struct {
	SMTPHost            string `json:"smtp_host"`
	SMTPPort            int    `json:"smtp_port"`
	SMTPSSL             bool   `json:"smtp_ssl"`
	SMTPUsername        string `json:"smtp_username"`
	SMTPFrom            string `json:"smtp_from"`
	HasPassword         bool   `json:"has_password"`
	RegisterCodeEnabled bool   `json:"register_code_enabled"`
	LoginCodeEnabled    bool   `json:"login_code_enabled"`
}

// EmailService 发送与校验邮箱验证码，并维护站点 SMTP 配置。
type EmailService struct {
	db *gorm.DB
	// send 是发信函数，默认 mailer.Send；测试里可以注入假实现。
	send func(cfg mailer.Config, to, subject, body string) error
	// 以下状态仅存内存：验证码是短生命周期数据，重启作废可接受，
	// 与图形验证码同一取舍。
	mu      sync.Mutex
	codes   map[string]*emailCode   // key = scene + "|" + email
	resend  map[string]time.Time    // key = scene + "|" + email，上次发码时间
	hourly  map[string][]time.Time  // key = scene + "|" + email，一小时内发码时间
	tickets map[string]*loginTicket // key = login ticket
}

type emailCode struct {
	codeHash string
	expires  time.Time
	attempts int
}

type loginTicket struct {
	userID   uint
	codeHash string
	expires  time.Time
	attempts int
}

// NewEmailService 构造 EmailService。
func NewEmailService(db *gorm.DB) *EmailService {
	return &EmailService{
		db:      db,
		send:    mailer.Send,
		codes:   make(map[string]*emailCode),
		resend:  make(map[string]time.Time),
		hourly:  make(map[string][]time.Time),
		tickets: make(map[string]*loginTicket),
	}
}

// EmailSettings 读取邮件配置（密码不回显）。
func (s *EmailService) EmailSettings() EmailSettings {
	out := EmailSettings{}
	var rows []model.Setting
	keys := []string{
		SettingSMTPHost, SettingSMTPPort, SettingSMTPSSL, SettingSMTPUsername,
		SettingSMTPPassword, SettingSMTPFrom, SettingEmailCodeRegister, SettingEmailCodeLogin,
	}
	// key 是 MySQL 保留字，走 map 条件让 GORM 按方言给列名加引号。
	if err := s.db.Where(map[string]any{"key": keys}).Find(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		switch r.Key {
		case SettingSMTPHost:
			out.SMTPHost = r.Value
		case SettingSMTPPort:
			if n, e := strconv.Atoi(r.Value); e == nil {
				out.SMTPPort = n
			}
		case SettingSMTPSSL:
			out.SMTPSSL = r.Value == "1"
		case SettingSMTPUsername:
			out.SMTPUsername = r.Value
		case SettingSMTPPassword:
			out.HasPassword = r.Value != ""
		case SettingSMTPFrom:
			out.SMTPFrom = r.Value
		case SettingEmailCodeRegister:
			out.RegisterCodeEnabled = r.Value == "1"
		case SettingEmailCodeLogin:
			out.LoginCodeEnabled = r.Value == "1"
		}
	}
	if out.SMTPPort == 0 {
		out.SMTPPort = 465
	}
	return out
}

// SaveEmailSettings 保存邮件配置。password 为空表示保留原密码。
func (s *EmailService) SaveEmailSettings(in EmailSettings, password string) (EmailSettings, error) {
	in.SMTPHost = strings.TrimSpace(in.SMTPHost)
	in.SMTPUsername = strings.TrimSpace(in.SMTPUsername)
	in.SMTPFrom = strings.TrimSpace(in.SMTPFrom)
	if in.SMTPPort < 1 || in.SMTPPort > 65535 {
		return EmailSettings{}, ErrBadRequest("SMTP 端口需在 1-65535 之间")
	}
	rows := []model.Setting{
		{Key: SettingSMTPHost, Value: in.SMTPHost},
		{Key: SettingSMTPPort, Value: strconv.Itoa(in.SMTPPort)},
		{Key: SettingSMTPSSL, Value: boolSetting(in.SMTPSSL)},
		{Key: SettingSMTPUsername, Value: in.SMTPUsername},
		{Key: SettingSMTPFrom, Value: in.SMTPFrom},
		{Key: SettingEmailCodeRegister, Value: boolSetting(in.RegisterCodeEnabled)},
		{Key: SettingEmailCodeLogin, Value: boolSetting(in.LoginCodeEnabled)},
	}
	if password != "" {
		rows = append(rows, model.Setting{Key: SettingSMTPPassword, Value: password})
	}
	for i := range rows {
		if err := s.db.Save(&rows[i]).Error; err != nil {
			return EmailSettings{}, err
		}
	}
	out := in
	out.HasPassword = password != "" || s.hasStoredPassword()
	return out, nil
}

// SendTestMail 给指定邮箱发一封测试信，供管理后台验证 SMTP 配置。
func (s *EmailService) SendTestMail(to string) error {
	cfg, ok := s.smtpConfig()
	if !ok {
		return ErrBadRequest("请先填写并保存 SMTP 服务器配置")
	}
	return s.send(cfg, strings.TrimSpace(to), "Levis SMTP 测试邮件",
		"这是一封测试邮件。收到即说明 SMTP 配置正确。")
}

// smtpConfig 拉出发信所需的完整配置；host/port 缺失视为未配置。
func (s *EmailService) smtpConfig() (mailer.Config, bool) {
	set := s.EmailSettings()
	if set.SMTPHost == "" || set.SMTPPort == 0 {
		return mailer.Config{}, false
	}
	var password string
	var row model.Setting
	if err := s.db.First(&row, "`key` = ?", SettingSMTPPassword).Error; err == nil {
		password = row.Value
	}
	return mailer.Config{
		Host:     set.SMTPHost,
		Port:     set.SMTPPort,
		SSL:      set.SMTPSSL,
		Username: set.SMTPUsername,
		Password: password,
		From:     set.SMTPFrom,
	}, true
}

func (s *EmailService) hasStoredPassword() bool {
	var row model.Setting
	if err := s.db.First(&row, "`key` = ?", SettingSMTPPassword).Error; err != nil {
		return false
	}
	return row.Value != ""
}

// EmailCodeEnabled 报告某场景是否开启了邮箱验证码。
func (s *EmailService) EmailCodeEnabled(scene string) bool {
	key := SettingEmailCodeRegister
	if scene == EmailSceneLogin {
		key = SettingEmailCodeLogin
	}
	var row model.Setting
	if err := s.db.First(&row, "`key` = ?", key).Error; err != nil {
		return false
	}
	return row.Value == "1"
}

// SendEmailCode 给 email 发送 scene 场景的验证码。
//
// 频控（按 邮箱+场景）：60 秒内不重发、每小时至多 5 封。验证码只存哈希，
// 内存泄露也拿不到明文。
func (s *EmailService) SendEmailCode(scene, email, siteName string) error {
	if scene != EmailSceneRegister && scene != EmailSceneLogin {
		return ErrBadRequest("不支持的验证码场景")
	}
	if !s.EmailCodeEnabled(scene) {
		return ErrBadRequest("当前未开启邮箱验证码")
	}
	cleaned, err := ValidateEmail(email)
	if err != nil {
		return err
	}
	cfg, ok := s.smtpConfig()
	if !ok {
		return ErrBadRequest("站点尚未配置邮件服务，请联系管理员")
	}

	key := scene + "|" + strings.ToLower(cleaned)
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.resend[key]; ok && now.Sub(last) < resendInterval {
		s.mu.Unlock()
		return ErrBadRequest("发送过于频繁，请稍后再试")
	}
	times := append(s.hourly[key], now)
	recent := times[:0]
	for _, t := range times {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= hourlyCap {
		s.mu.Unlock()
		return ErrBadRequest("该邮箱发码次数已达上限，请一小时后再试")
	}
	code, err := randomCode()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.codes[key] = &emailCode{codeHash: codeHash(code), expires: now.Add(codeTTL), attempts: codeMaxAttempts}
	s.resend[key] = now
	s.hourly[key] = recent
	s.mu.Unlock()

	subject := fmt.Sprintf("%s 邮箱验证码", siteName)
	body := fmt.Sprintf("您的验证码是：%s\n\n%v 分钟内有效。若非本人操作请忽略本邮件。", code, int(codeTTL.Minutes()))
	if err := s.send(cfg, cleaned, subject, body); err != nil {
		// 发送失败就作废刚发的码：不给暴力尝试留空间，也避免用户拿着废码反复试。
		s.mu.Lock()
		delete(s.codes, key)
		s.mu.Unlock()
		return ErrBadRequest("验证码邮件发送失败，请稍后重试")
	}
	return nil
}

// VerifyEmailCode 校验并消费验证码。
func (s *EmailService) VerifyEmailCode(scene, email, code string) error {
	key := scene + "|" + strings.ToLower(email)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.codes[key]
	if !ok || now.After(entry.expires) {
		delete(s.codes, key)
		return ErrBadRequest("验证码已过期，请重新获取")
	}
	if entry.attempts <= 0 {
		delete(s.codes, key)
		return ErrBadRequest("验证码已失效，请重新获取")
	}
	entry.attempts--
	if subtle.ConstantTimeCompare([]byte(entry.codeHash), []byte(codeHash(code))) != 1 {
		return ErrBadRequest("验证码错误")
	}
	// 一次验证即作废。
	delete(s.codes, key)
	return nil
}

// IssueLoginTicket 为登录二次校验签发票据：给账号邮箱发码，返回票据与掩码邮箱。
func (s *EmailService) IssueLoginTicket(user *model.User, siteName string) (string, string, error) {
	if strings.TrimSpace(user.Email) == "" {
		return "", "", ErrBadRequest("账号未绑定邮箱，无法使用邮箱验证码登录")
	}
	ticketBytes := make([]byte, 32)
	if _, err := rand.Read(ticketBytes); err != nil {
		return "", "", err
	}
	ticket := hex.EncodeToString(ticketBytes)
	cfg, ok := s.smtpConfig()
	if !ok {
		return "", "", ErrBadRequest("站点尚未配置邮件服务，请联系管理员")
	}
	code, err := randomCode()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	s.mu.Lock()
	s.tickets[ticket] = &loginTicket{userID: user.ID, codeHash: codeHash(code), expires: now.Add(codeMaxTTL), attempts: codeMaxAttempts}
	s.mu.Unlock()

	subject := fmt.Sprintf("%s 登录验证码", siteName)
	body := fmt.Sprintf("您的登录验证码是：%s\n\n%v 分钟内有效。若非本人操作请及时修改密码。",
		code, int(codeMaxTTL.Minutes()))
	if err := s.send(cfg, user.Email, subject, body); err != nil {
		s.mu.Lock()
		delete(s.tickets, ticket)
		s.mu.Unlock()
		return "", "", ErrBadRequest("验证码邮件发送失败，请稍后重试")
	}
	return ticket, maskEmail(user.Email), nil
}

// VerifyLoginTicket 校验登录票据与验证码，返回用户 ID。
func (s *EmailService) VerifyLoginTicket(ticket, code string) (uint, error) {
	if ticket == "" || code == "" {
		return 0, ErrBadRequest("请输入验证码")
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tickets[ticket]
	if !ok || now.After(entry.expires) {
		delete(s.tickets, ticket)
		return 0, ErrBadRequest("登录会话已过期，请重新登录")
	}
	if entry.attempts <= 0 {
		delete(s.tickets, ticket)
		return 0, ErrBadRequest("验证码已失效，请重新登录")
	}
	entry.attempts--
	if subtle.ConstantTimeCompare([]byte(entry.codeHash), []byte(codeHash(code))) != 1 {
		return 0, ErrBadRequest("验证码错误")
	}
	delete(s.tickets, ticket)
	return entry.userID, nil
}

// maskEmail 把邮箱中间段打成星号：a***@example.com。
func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	local := email[:at]
	keep := 1
	if len(local) < keep {
		keep = len(local)
	}
	return local[:keep] + "***" + email[at:]
}

func codeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
