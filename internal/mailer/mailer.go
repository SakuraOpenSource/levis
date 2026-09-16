// Package mailer 提供站点内置的 SMTP 发信能力。
//
// 邮件验证码与邮件插件（notify → CAPABILITY_SEND_MAIL）是两条独立通道：
// 插件负责业务通知信，本包只服务「注册/登录邮箱验证码」这类安全邮件，
// 配置存在站点设置表里，由管理后台维护。SMTP 握手可能耗时数秒，调用方
// 应放在请求可容忍的位置并设置超时。
package mailer

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// dialTimeout 是单次 TCP 连接（不含 TLS 握手）的超时。
	dialTimeout = 15 * time.Second
	// sessionTimeout 是整个 SMTP 会话（含 TLS 握手、命令往返与 DATA）的
	// 总时限，防止对端半死不活时把请求协程无限挂住。
	sessionTimeout = 60 * time.Second
	// greetProbe 是明文模式下等待服务器问候的探测窗：明文 SMTP 服务器
	// 会立即送出「220 」，而隐式 TLS 服务器在收到 ClientHello 前一言不发。
	greetProbe = 3 * time.Second
)

// Config 是一次发信所需的全部服务器信息。
type Config struct {
	Host          string // SMTP 服务器
	Port          int    // 端口（465 隐式 TLS / 587 STARTTLS / 25 明文）
	SSL           bool   // true = 隐式 TLS（握手即加密）；false = 先明文连，服务器支持则升级 STARTTLS
	Username      string
	Password      string
	From          string // 发件邮箱；为空时回落 Username
	SkipTLSVerify bool   // 跳过 TLS 证书校验（自签证书的内网 relay 场景）
}

// Send 通过 SMTP 把一封纯文本邮件发给单个收件人。
//
// 连接层有容错：SSL 开了但对方其实是明文/STARTTLS 端口时回退重连；
// SSL 没开但对方直接吐 TLS 记录（465 忘开开关的常见错配）时自动升级。
// 认证机制选择：TLS 已建立时优先 PLAIN，否则优先 LOGIN（明文连接上
// 标准库会拒绝 PLAIN，而端口 25 的 relay 常只开 LOGIN）；服务器不支持
// AUTH 时直接放行发信（无鉴权 relay 场景）。
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

	conn, tlsUp, err := dialConn(cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(sessionTimeout))

	cl, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 会话建立失败: %w", err)
	}
	defer cl.Close()

	// EHLO 标识用发件域名（早前误传了 SMTP 服务器自己的域名，
	// 部分反垃圾策略会因此拒信）。
	if err := cl.Hello(heloName(cfg.From)); err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	// STARTTLS：服务器支持则升级（不支持时按明文继续，内网 relay 常见）。
	if !tlsUp {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(tlsConfig(cfg)); err != nil {
				return fmt.Errorf("STARTTLS 升级失败: %w", err)
			}
			tlsUp = true
		}
	}
	if ok, mech := cl.Extension("AUTH"); ok && cfg.Username != "" {
		var auth smtp.Auth
		switch {
		case tlsUp && strings.Contains(mech, "PLAIN"):
			auth = &plainAuth{username: cfg.Username, password: cfg.Password}
		case strings.Contains(mech, "LOGIN"):
			auth = &loginAuth{username: cfg.Username, password: cfg.Password}
		case strings.Contains(mech, "PLAIN"):
			// 明文连接 + 服务器只开 PLAIN：交给标准库拒绝
			//（它会报 unencrypted connection，把原因如实带回给调用方）。
			auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
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
	// w.Close() 收到 250 即代表服务器已接收；之后的 Quit 只是礼貌收尾，
	// 它的失败不应把成功的投递误报为失败（否则上层会重发造成重复信）。
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	_ = cl.Quit()
	return nil
}

// dialConn 建立 SMTP 连接并返回（连接, 是否已处于 TLS 之上）。
//
// 两种常见错配都会被自动纠正：
//   - 配了 SSL 但对方其实是明文/STARTTLS 端口：TLS 握手读到的不是
//     ServerHello，此时关闭重连明文，后续按 STARTTLS 或纯明文继续；
//   - 没配 SSL 但对方是 465 这类隐式 TLS 端口：明文问候永远等不到
//     （TLS 服务器在收到 ClientHello 前一言不发），探测窗超时后改走 TLS。
func dialConn(cfg Config) (net.Conn, bool, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	if cfg.SSL {
		return dialTLSWithPlainFallback(cfg, addr)
	}
	return dialPlainWithTLSFallback(cfg, addr)
}

// dialTLSWithPlainFallback 先按隐式 TLS 握手；只有当对端根本没在说 TLS
// （错误为 first record does not look like a TLS handshake，即先收到的是
// 明文问候）时才回退明文重连。证书校验失败等错误原样暴露。
func dialTLSWithPlainFallback(cfg Config, addr string) (net.Conn, bool, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(sessionTimeout))
	tlsConn := tls.Client(conn, tlsConfig(cfg))
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		if !strings.Contains(err.Error(), "first record does not look like a TLS handshake") {
			return nil, false, fmt.Errorf("TLS 握手失败: %w", err)
		}
		plain, err2 := net.DialTimeout("tcp", addr, dialTimeout)
		if err2 != nil {
			return nil, false, fmt.Errorf("TLS 握手失败: %w", err)
		}
		_ = plain.SetDeadline(time.Now().Add(sessionTimeout))
		return plain, false, nil
	}
	return tlsConn, true, nil
}

// dialPlainWithTLSFallback 先按明文连接并给问候一个短探测窗：
//   - 窗口内收到问候字节 → 明文服务器，继续（首字节若是 TLS 记录则改走 TLS）；
//   - 窗口内一直沉默或直接断开 → 隐式 TLS 服务器，关闭后改走 TLS，
//     必要时再回退明文。
func dialPlainWithTLSFallback(cfg Config, addr string) (net.Conn, bool, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(sessionTimeout))
	bc := &peekConn{r: bufio.NewReader(conn), Conn: conn}
	_ = conn.SetReadDeadline(time.Now().Add(greetProbe))
	_, perr := bc.r.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if perr == nil {
		// 收到了问候字节。明文 SMTP 固定以「220 」开头；若首字节是 TLS
		// 记录（0x16 0x03，标准 TLS 服务器不会先说话，这里兜底极端实现）。
		if head, _ := bc.r.Peek(3); len(head) >= 2 && head[0] == 0x16 && head[1] == 0x03 {
			_ = conn.Close()
			return dialTLSWithPlainFallback(cfg, addr)
		}
		return bc, false, nil
	}
	// 沉默（探测窗超时）或对端断开：按隐式 TLS 重试。
	_ = conn.Close()
	return dialTLSWithPlainFallback(cfg, addr)
}

// peekConn 把 bufio.Reader 的缓冲并入 net.Conn 的读取路径，让已探测过
// 的字节不丢失地流给 TLS 握手或 SMTP 应答解析。
type peekConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *peekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func tlsConfig(cfg Config) *tls.Config {
	return &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.SkipTLSVerify}
}

// heloName 返回 HELO/EHLO 应当声明的本端标识：发件域名。
func heloName(from string) string {
	if at := strings.LastIndexByte(from, '@'); at >= 0 && at+1 < len(from) {
		return from[at+1:]
	}
	return "localhost"
}

// plainAuth 实现 AUTH PLAIN。不用标准库 smtp.PlainAuth 的原因：它要求
// Client.tls 标志为真，而该标志只在 STARTTLS 路径被置位——465 隐式 TLS
// 走 NewClient 时标准库无从得知连接已加密，会误报 unencrypted connection。
// 加密与否由 dialConn 自己建立并跟踪，这里无需重复检查。
type plainAuth struct{ username, password string }

func (a *plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	resp := "\x00" + a.username + "\x00" + a.password
	return "PLAIN", []byte(resp), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("PLAIN 不应有多轮询问")
	}
	return nil, nil
}

// loginAuth 实现 AUTH LOGIN（stdlib 只内置 PLAIN/CRAM-MD5，国内服务商常只开 LOGIN）。
//
// Start 必须返回空初始响应：LOGIN 是 server-first 机制，把用户名塞进
// `AUTH LOGIN <resp>` 属于协议违规，部分服务器会直接拒绝整个认证。
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	// 服务器问的既不是用户名也不是密码：交出去有泄露凭据的风险，直接放弃。
	return nil, fmt.Errorf("未知的服务端询问")
}

// buildMessage 组装 RFC 5322 邮件：头字段 ASCII 化，正文 base64（UTF-8）。
// Date 与 Message-ID 是必需头，缺失会被 Gmail 等收件方直接拒收或记为垃圾。
func buildMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeSubject(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: " + messageID(from) + "\r\n")
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

// encodeSubject 把主题编码为可折叠的 UTF-8 B 编码词序列（每个编码词
// 不超过 75 字符），长主题以「CRLF + 空格」续行，避免产生超长头被拒收。
func encodeSubject(subject string) string {
	const (
		prefix = "=?UTF-8?B?"
		suffix = "?="
		maxLen = 75
	)
	// 每个编码词容纳的 base64 字符数（取 4 的倍数，且按 UTF-8 边界回退）。
	b64PerWord := (maxLen - len(prefix) - len(suffix)) / 4 * 4
	rawPerWord := b64PerWord / 4 * 3
	if rawPerWord <= 0 {
		rawPerWord = 1
	}

	words := make([]string, 0, 1+len(subject)/rawPerWord)
	src := []byte(subject)
	for len(src) > 0 {
		n := rawPerWord
		if n > len(src) {
			n = len(src)
		}
		// 让下一块从 UTF-8 字符边界开始，防止把多字节字符切成两半。
		for n > 0 && n < len(src) && !utf8.RuneStart(src[n]) {
			n--
		}
		if n == 0 {
			n = 1
		}
		words = append(words, prefix+base64.StdEncoding.EncodeToString(src[:n])+suffix)
		src = src[n:]
	}
	return strings.Join(words, "\r\n ")
}

// messageID 生成一封邮件唯一的 Message-ID：<随机 hex.纳秒@发件域>。
func messageID(from string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// 随机源失败极罕见；退化为纳秒时间戳仍能保证基本唯一。
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), heloName(from))
	}
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(buf), time.Now().UnixNano(), heloName(from))
}
