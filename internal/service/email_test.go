package service

import (
	"strings"
	"testing"

	"github.com/SakuraOpenSource/levis/internal/mailer"
)

// TestEmailCodeFlow 覆盖邮箱验证码闭环：默认关闭 → 配置并开启 → 发码 → 频控 → 错码拒绝。
func TestEmailCodeFlow(t *testing.T) {
	db := newTestDB(t)
	svc := NewEmailService(db)
	var sentTo []string
	svc.send = func(cfg mailer.Config, to, subject, body string) error {
		sentTo = append(sentTo, to)
		return nil
	}

	// 默认关闭：发码应被拒。
	if err := svc.SendEmailCode(EmailSceneRegister, "a@b.com", "T"); err == nil {
		t.Fatal("未开启邮箱验证码时发码应被拒绝")
	}

	if _, err := svc.SaveEmailSettings(EmailSettings{
		SMTPHost:            "smtp.test",
		SMTPPort:            465,
		SMTPSSL:             true,
		SMTPUsername:        "u",
		SMTPFrom:            "noreply@test",
		RegisterCodeEnabled: true,
		LoginCodeEnabled:    true,
	}, "secret"); err != nil {
		t.Fatalf("保存邮件配置失败: %v", err)
	}
	out := svc.EmailSettings()
	if !out.HasPassword {
		t.Fatal("保存密码后 has_password 应为 true")
	}
	if out.SMTPPort != 465 || !out.RegisterCodeEnabled || !out.LoginCodeEnabled {
		t.Fatalf("配置回读不符: %+v", out)
	}

	if err := svc.SendEmailCode(EmailSceneRegister, "A@B.com", "T"); err != nil {
		t.Fatalf("开启后发码失败: %v", err)
	}
	if len(sentTo) != 1 || sentTo[0] != "a@b.com" {
		t.Fatalf("收件人应为小写邮箱，实际 %v", sentTo)
	}

	// 60 秒频控：立即重发必须被拒。
	if err := svc.SendEmailCode(EmailSceneRegister, "a@b.com", "T"); err == nil {
		t.Fatal("60 秒内重复发码应被拒绝")
	}

	// 错误验证码被拒（正确验证码的闭环由浏览器端 E2E 覆盖）。
	if err := svc.VerifyEmailCode(EmailSceneRegister, "a@b.com", "000000"); err == nil {
		t.Fatal("错误验证码应校验失败")
	}
}

// TestLoginTicketAttempts 覆盖登录票据的防爆破计数与邮箱打码。
func TestLoginTicketAttempts(t *testing.T) {
	db := newTestDB(t)
	svc := NewEmailService(db)
	svc.send = func(cfg mailer.Config, to, subject, body string) error { return nil }
	if _, err := svc.SaveEmailSettings(EmailSettings{
		SMTPHost: "smtp.test", SMTPPort: 465, SMTPSSL: true,
		SMTPUsername: "u", SMTPFrom: "noreply@test", LoginCodeEnabled: true,
	}, "pw"); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	user := seedUser(t, db, "mailer", 0)
	ticket, masked, err := svc.IssueLoginTicket(user, "T")
	if err != nil {
		t.Fatalf("签发登录票据失败: %v", err)
	}
	if !strings.Contains(masked, "***") || masked == user.Email {
		t.Fatalf("邮箱应被打码，实际 %q", masked)
	}
	// 连续错 5 次后票据作废。
	for i := 0; i < codeMaxAttempts; i++ {
		if _, err := svc.VerifyLoginTicket(ticket, "000000"); err == nil {
			t.Fatalf("第 %d 次错误验证码应失败", i+1)
		}
	}
	if _, err := svc.VerifyLoginTicket(ticket, "000001"); err == nil {
		t.Fatal("超过尝试上限后应直接作废")
	}
}
