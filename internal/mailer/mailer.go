// Package mailer 提供站点内置的 SMTP 发信能力。
//
// 邮件验证码与邮件插件（notify → CAPABILITY_SEND_MAIL）是两条独立通道：
// 插件负责业务通知信，本包只服务「注册/登录邮箱验证码」这类安全邮件，
// 配置存在站点设置表里，由管理后台维护。SMTP 握手可能耗时数秒，调用方
// 应放在请求可容忍的位置并设置超时。
package mailer

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// dialTimeout 是 SMTP 连接（含 TLS 握手）的超时。
const dialTimeout = 15 * time.Second

// Config 是一次发信所需的全部服务器信息。
type Config struct {
	Host     string // SMTP 服务器
	Port     int    // 端口（465 隐式 TLS / 587 STARTTLS / 25 明文）
	SSL      bool   // true = 隐式 TLS（握手即加密）；false = 先明文连，服务器支持则升级 STARTTLS
	Username string
	Password string
	From     string // 发件邮箱；为空时回落 Username
}

// Send 通过 SMTP 把一封纯文本邮件发给单个收件人。
//
// 认证依次尝试 PLAIN 与 LOGIN（QQ/163 等国内服务商只认 LOGIN）；
// 服务器不支持 AUTH 时直接放行发信（内网 relay 场景）。
func Send(cfg Config, to, subject, body string) error {
	if strings.TrimSpace(cfg.Host) == "" || cfg.Port <= 0 {
		return fmt.Errorf("SMTP 服务器未配置")
	}
	if strings.TrimSpace(cfg.From) == "" {
		cfg.From = cfg.Username
	}
	if strings.TrimSpace(cfg.From) == "" {
		return fmt.Errorf("发件邮箱未配置")
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer conn.Close()

	// 隐式 TLS：连上就握手；STARTTLS 模式先按明文客户端建立。
	var cl *smtp.Client
	if cfg.SSL {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("TLS 握手失败: %w", err)
		}
		cl, err = smtp.NewClient(tlsConn, cfg.Host)
	} else {
		cl, err = smtp.NewClient(conn, cfg.Host)
	}
	if err != nil {
		return fmt.Errorf("SMTP 会话建立失败: %w", err)
	}
	defer cl.Close()

	if err := cl.Hello(cfg.Host); err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	// STARTTLS：服务器支持则升级（不支持且未开 SSL 时按明文继续，内网 relay 常见）。
	if !cfg.SSL {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("STARTTLS 升级失败: %w", err)
			}
		}
	}
	if ok, mech := cl.Extension("AUTH"); ok && cfg.Username != "" {
		var auth smtp.Auth
		if strings.Contains(mech, "PLAIN") {
			auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		} else if strings.Contains(mech, "LOGIN") {
			auth = &loginAuth{cfg.Username, cfg.Password}
		}
		if auth != nil {
			if err := cl.Auth(auth); err != nil {
				return fmt.Errorf("SMTP 认证失败: %w", err)
			}
		}
	}
	if err := cl.Mail(cfg.From); err != nil {
		return fmt.Errorf("设置发件人失败: %w", err)
	}
	if err := cl.Rcpt(to); err != nil {
		return fmt.Errorf("设置收件人失败: %w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("开始写信失败: %w", err)
	}
	if _, err := w.Write([]byte(buildMessage(cfg.From, to, subject, body))); err != nil {
		_ = w.Close()
		return fmt.Errorf("写入邮件失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	return cl.Quit()
}

// loginAuth 实现 AUTH LOGIN（stdlib 只内置 PLAIN/CRAM-MD5，国内服务商常只开 LOGIN）。
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	}
	// 服务器问的既不是用户名也不是密码：交出去有泄露凭据的风险，直接放弃。
	return nil, fmt.Errorf("未知的服务端询问")
}

// buildMessage 组装 RFC 5322 邮件：头字段 ASCII 化，正文 base64（UTF-8）。
func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: =?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?=\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	// 每行最多 76 字符，超长行会被部分服务商拒收。
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}
