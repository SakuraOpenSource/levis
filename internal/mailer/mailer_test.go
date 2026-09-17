package mailer

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP 是一台极简 SMTP 服务器：只实现本包客户端会用到的命令，
// 用于验证认证机制选择、TLS 自动探测与报文组装逻辑。
type fakeSMTP struct {
	listener net.Listener
	cert     tls.Certificate // STARTTLS 升级与 TLS 监听共用

	mu       sync.Mutex
	banner   string // EHLO 能力行（以 \r\n 连接，含结尾的 250 OK 行）
	authUser string
	authPass string
	authMech string // 实际使用的机制（LOGIN / PLAIN），空表示未认证
	authLine string // 收到的第一条 AUTH 命令原文
	mailFrom string
	rcptTo   string
	data     string
}

func newFakeSMTP(t *testing.T, banner string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{listener: ln, banner: banner, cert: selfSigned(t)}
	go f.accept()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func newFakeSMTPTLS(t *testing.T, banner string) *fakeSMTP {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	f := &fakeSMTP{listener: ln, banner: banner, cert: selfSigned(t)}
	go f.accept()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) accept() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

// serve 处理一条连接。SSL 错配的回退场景里客户端第一轮只会发来 TLS
// ClientHello 乱码，这里统一按未知命令回 500 即可，连接随读错误退出。
func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	write := func(s string) { _, _ = conn.Write([]byte(s)) }
	write("220 test.local ESMTP\r\n")

	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250-test.local\r\n" + f.banner)
		case strings.HasPrefix(cmd, "STARTTLS"):
			write("220 go ahead\r\n")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			// write 闭包按引用捕获 conn，升级后自动写 TLS 连接。
			conn = tlsConn
			br = bufio.NewReader(conn)
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		case strings.HasPrefix(cmd, "AUTH LOGIN"):
			f.setAuth(line, "LOGIN")
			if rest := strings.TrimPrefix(line, "AUTH LOGIN "); rest != line {
				// 兼容带初始响应的写法（本包客户端不应发送）。
				f.setUser(decodeB64(rest))
			}
			write("334 VXNlcm5hbWU6\r\n") // base64("Username:")
			uname, err := br.ReadString('\n')
			if err != nil {
				return
			}
			f.setUser(decodeB64(uname))
			write("334 UGFzc3dvcmQ6\r\n") // base64("Password:")
			pass, err := br.ReadString('\n')
			if err != nil {
				return
			}
			f.setPass(decodeB64(pass))
			write("235 ok\r\n")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			f.setAuth(line, "PLAIN")
			payload := ""
			if rest := strings.TrimPrefix(line, "AUTH PLAIN "); rest != line {
				payload = rest
			} else {
				write("334 \r\n")
				resp, err := br.ReadString('\n')
				if err != nil {
					return
				}
				payload = strings.TrimRight(resp, "\r\n")
			}
			parts := strings.Split(decodeB64(payload), "\x00")
			if len(parts) >= 3 {
				f.setUser(parts[len(parts)-2])
				f.setPass(parts[len(parts)-1])
			}
			write("235 ok\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			f.setFrom(extractAddr(line))
			write("250 ok\r\n")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.setRcpt(extractAddr(line))
			write("250 ok\r\n")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 end with <CRLF>.<CRLF>\r\n")
			var sb strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				sb.WriteString(l)
			}
			f.setData(sb.String())
			write("250 queued\r\n")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye\r\n")
			return
		default:
			write("500 5.5.2 unrecognized\r\n")
		}
	}
}

func (f *fakeSMTP) setAuth(line, mech string) {
	f.mu.Lock()
	f.authLine, f.authMech = line, mech
	f.mu.Unlock()
}
func (f *fakeSMTP) setUser(u string) { f.mu.Lock(); f.authUser = u; f.mu.Unlock() }
func (f *fakeSMTP) setPass(p string) { f.mu.Lock(); f.authPass = p; f.mu.Unlock() }
func (f *fakeSMTP) setFrom(s string) { f.mu.Lock(); f.mailFrom = s; f.mu.Unlock() }
func (f *fakeSMTP) setRcpt(s string) { f.mu.Lock(); f.rcptTo = s; f.mu.Unlock() }
func (f *fakeSMTP) setData(s string) { f.mu.Lock(); f.data = s; f.mu.Unlock() }

func (f *fakeSMTP) snapshot() (authLine, authMech, authUser, authPass, mailFrom, rcptTo, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authLine, f.authMech, f.authUser, f.authPass, f.mailFrom, f.rcptTo, f.data
}

func (f *fakeSMTP) cfg(ssl bool) Config {
	port := f.listener.Addr().(*net.TCPAddr).Port
	return Config{
		Host: "127.0.0.1", Port: port, SSL: ssl, SkipTLSVerify: true,
		Username: "user@example.com", Password: "secret", From: "user@example.com",
	}
}

func decodeB64(s string) string {
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "<bad base64: " + s + ">"
	}
	return string(dec)
}

func extractAddr(line string) string {
	i := strings.IndexByte(line, '<')
	j := strings.LastIndexByte(line, '>')
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	return line
}

// selfSigned 生成一张覆盖 127.0.0.1 的临时证书，供 TLS 假服务器使用。
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	return cert
}

// TestSendLoginAuthNoInitialResponse 验证 AUTH LOGIN 使用规范的空初始响应，
// 并把用户名/密码作为对服务器询问的回答逐轮提交。
func TestSendLoginAuthNoInitialResponse(t *testing.T) {
	f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250-8BITMIME\r\n250 OK\r\n")
	if err := Send(f.cfg(false), "to@example.com", "验证码", "正文内容"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	authLine, authMech, authUser, authPass, mailFrom, rcptTo, data := f.snapshot()
	if authLine != "AUTH LOGIN" {
		t.Fatalf("AUTH 命令应为无初始响应的 AUTH LOGIN，实际: %q", authLine)
	}
	if authMech != "LOGIN" || authUser != "user@example.com" || authPass != "secret" {
		t.Fatalf("认证结果不符: mech=%q user=%q pass=%q", authMech, authUser, authPass)
	}
	if mailFrom != "user@example.com" || rcptTo != "to@example.com" {
		t.Fatalf("信封不符: from=%q rcpt=%q", mailFrom, rcptTo)
	}
	assertHeaders(t, data, "正文内容")
}

// TestSendPlainAuthOverImplicitTLS 验证 465 隐式 TLS 下选择 PLAIN，
// 且不会因标准库误判「未加密连接」而失败。
func TestSendPlainAuthOverImplicitTLS(t *testing.T) {
	f := newFakeSMTPTLS(t, "250-AUTH LOGIN PLAIN\r\n250 OK\r\n")
	if err := Send(f.cfg(true), "to@example.com", "s", "b"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	_, authMech, authUser, authPass, _, _, _ := f.snapshot()
	if authMech != "PLAIN" || authUser != "user@example.com" || authPass != "secret" {
		t.Fatalf("PLAIN 认证结果不符: mech=%q user=%q pass=%q", authMech, authUser, authPass)
	}
}

// TestSendAutoDetectsImplicitTLSWhenSSLOff 复现「端口 465 但 SSL 开关没开」
// 的错配：客户端应通过问候字节识别出 TLS 并自动升级，最终成功发信。
func TestSendAutoDetectsImplicitTLSWhenSSLOff(t *testing.T) {
	f := newFakeSMTPTLS(t, "250-AUTH LOGIN PLAIN\r\n250 OK\r\n")
	if err := Send(f.cfg(false), "to@example.com", "s", "b"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	_, authMech, _, _, _, _, _ := f.snapshot()
	if authMech != "PLAIN" {
		t.Fatalf("自动升级 TLS 后应选择 PLAIN，实际: %q", authMech)
	}
}

// TestSendFallsBackWhenSSLFlagWrong 复现反向错配（SSL 开了但端口是明文）：
// 客户端应回退明文重连并完成发信。
func TestSendFallsBackWhenSSLFlagWrong(t *testing.T) {
	f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250 OK\r\n")
	if err := Send(f.cfg(true), "to@example.com", "s", "b"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	_, authMech, _, _, _, _, data := f.snapshot()
	if authMech != "LOGIN" {
		t.Fatalf("明文回退后应选择 LOGIN，实际: %q", authMech)
	}
	if data == "" {
		t.Fatal("回退路径未完成 DATA 投递")
	}
}

// TestSendRelayWithoutAuth 验证服务器不宣告 AUTH 时直接放行发信。
func TestSendRelayWithoutAuth(t *testing.T) {
	f := newFakeSMTP(t, "250-8BITMIME\r\n250 OK\r\n")
	if err := Send(f.cfg(false), "to@example.com", "s", "b"); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	_, authMech, _, _, mailFrom, rcptTo, data := f.snapshot()
	if authMech != "" {
		t.Fatalf("不应发起认证，实际: %q", authMech)
	}
	if mailFrom != "user@example.com" || rcptTo != "to@example.com" || data == "" {
		t.Fatalf("投递不完整: from=%q rcpt=%q data=%q", mailFrom, rcptTo, data)
	}
}

func assertHeaders(t *testing.T, data, wantBody string) {
	t.Helper()
	for _, head := range []string{
		"From: user@example.com\r\n",
		"To: to@example.com\r\n",
		"Date: ",
		"Message-ID: <",
		"MIME-Version: 1.0\r\n",
		"Content-Transfer-Encoding: base64\r\n",
	} {
		if !strings.Contains(data, head) {
			t.Fatalf("报文缺少必要头 %q，报文:\n%s", head, data)
		}
	}
	idx := strings.Index(data, "\r\n\r\n")
	if idx < 0 {
		t.Fatal("报文缺少头/体分隔")
	}
	body := strings.ReplaceAll(data[idx+4:], "\r\n", "")
	dec, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("正文 base64 解码失败: %v", err)
	}
	if string(dec) != wantBody {
		t.Fatalf("正文不符: got %q want %q", string(dec), wantBody)
	}
}

// TestEncodeSubject 验证长主题按 UTF-8 边界折叠成多个编码词，
// 每个词不超过 75 字符，拼接后可还原原文。
func TestEncodeSubject(t *testing.T) {
	long := strings.Repeat("邮箱验证码主题", 30)
	got := encodeSubject(long)
	for _, word := range strings.Split(got, "\r\n ") {
		if len(word) > 75 {
			t.Fatalf("编码词超长: %d 字节", len(word))
		}
	}
	var sb strings.Builder
	for _, word := range strings.Split(got, "\r\n ") {
		const p = "=?UTF-8?B?"
		if !strings.HasPrefix(word, p) || !strings.HasSuffix(word, "?=") {
			t.Fatalf("编码词格式错误: %q", word)
		}
		raw, err := base64.StdEncoding.DecodeString(word[len(p) : len(word)-2])
		if err != nil {
			t.Fatalf("编码词 base64 解码失败: %v", err)
		}
		sb.Write(raw)
	}
	if sb.String() != long {
		t.Fatalf("主题还原失败:\n got %q\nwant %q", sb.String(), long)
	}
	// 短主题应只产生单个编码词且不以空格续行。
	if short := encodeSubject("验证码"); strings.Contains(short, "\r\n") {
		t.Fatalf("短主题不应折叠: %q", short)
	}
}

// TestHeloName 验证 EHLO 标识取自发件域名。
func TestHeloName(t *testing.T) {
	if got := heloName("a@mail.example.com"); got != "mail.example.com" {
		t.Fatalf("heloName = %q", got)
	}
	if got := heloName("no-at-sign"); got != "localhost" {
		t.Fatalf("heloName = %q", got)
	}
}

// TestSendEncryptionModes 覆盖显式加密方式：ssl 强制隐式 TLS 不回退、
// starttls 强制升级且服务器不支持时报错、none 跳过 STARTTLS。
func TestSendEncryptionModes(t *testing.T) {
	t.Run("ssl 对明文服务器直接报错", func(t *testing.T) {
		f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250-STARTTLS\r\n250 OK\r\n")
		cfg := f.cfg(true)
		cfg.Encryption = EncryptionSSL
		if err := Send(cfg, "to@example.com", "s", "b"); err == nil {
			t.Fatal("对明文服务器强制 SSL 应失败")
		}
	})
	t.Run("starttls 成功升级", func(t *testing.T) {
		f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250-STARTTLS\r\n250 OK\r\n")
		cfg := f.cfg(false)
		cfg.Encryption = EncryptionStartTLS
		if err := Send(cfg, "to@example.com", "s", "b"); err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
	})
	t.Run("starttls 服务器不支持时报错", func(t *testing.T) {
		f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250 OK\r\n")
		cfg := f.cfg(false)
		cfg.Encryption = EncryptionStartTLS
		if err := Send(cfg, "to@example.com", "s", "b"); err == nil {
			t.Fatal("服务器不支持 STARTTLS 时应报错")
		}
	})
	t.Run("none 跳过 STARTTLS", func(t *testing.T) {
		// 服务器广告了 STARTTLS，但 none 模式不应升级：若客户端升级了
		// TLS，明文假服务器会把 ClientHello 当垃圾命令回 500，发信必然失败。
		f := newFakeSMTP(t, "250-AUTH LOGIN\r\n250-STARTTLS\r\n250 OK\r\n")
		cfg := f.cfg(false)
		cfg.Encryption = EncryptionNone
		if err := Send(cfg, "to@example.com", "s", "b"); err != nil {
			t.Fatalf("Send 失败: %v", err)
		}
	})
}
