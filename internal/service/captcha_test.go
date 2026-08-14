package service

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/captcha"
)

// fakeStore 是可控的验证码存储：answer 是唯一被接受的答案。
type fakeStore struct {
	answer  string
	charset string
	length  int
	checked []string
	err     error
}

func (f *fakeStore) Issue(charset string, length int) (*captcha.Challenge, error) {
	f.charset, f.length = charset, length
	if f.err != nil {
		return nil, f.err
	}
	return &captcha.Challenge{ID: "fake-id", Image: "data:image/png;base64,AAAA", ExpiresIn: 300}, nil
}

func (f *fakeStore) Verify(id, answer string) bool {
	f.checked = append(f.checked, id+":"+answer)
	return id == "fake-id" && answer == f.answer
}

// saveCaptcha 落一份配置，省去每个用例重复四行。
func saveCaptcha(t *testing.T, db *gorm.DB, cfg CaptchaConfig) {
	t.Helper()
	if cfg.Charset == "" {
		cfg.Charset = captcha.CharsetDigit
	}
	if cfg.Length == 0 {
		cfg.Length = captcha.DefaultLength
	}
	if _, err := NewSettingService(db).SaveCaptcha(cfg); err != nil {
		t.Fatalf("保存验证码配置失败: %v", err)
	}
}

// 场景关闭时直接放行，连空答案都不该拦 —— 否则前端不显示验证码就登录不上。
func TestCaptchaVerifySkipsWhenDisabled(t *testing.T) {
	db := newTestDB(t)
	store := &fakeStore{answer: "1234"}
	svc := NewCaptchaService(db, store)

	// 空库走默认配置：登录关、注册开。
	if err := svc.Verify(CaptchaSceneLogin, "", ""); err != nil {
		t.Fatalf("登录验证码默认关闭，应当放行，得到: %v", err)
	}
	if err := svc.Verify(CaptchaSceneRegister, "", ""); err == nil {
		t.Fatal("注册验证码默认开启，空答案应当被拒")
	}
	// 关闭的场景不该白跑一次存储查询。
	for _, c := range store.checked {
		t.Fatalf("关闭场景仍访问了存储: %s", c)
	}
}

func TestCaptchaVerifyAcceptsCorrectAnswer(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{LoginEnabled: true, RegisterEnabled: true})
	store := &fakeStore{answer: "A1B2"}
	svc := NewCaptchaService(db, store)

	for _, scene := range []string{CaptchaSceneLogin, CaptchaSceneRegister} {
		if err := svc.Verify(scene, "fake-id", "A1B2"); err != nil {
			t.Fatalf("scene=%s 正确答案被拒: %v", scene, err)
		}
	}
}

func TestCaptchaVerifyRejectsWrongAnswer(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{LoginEnabled: true, RegisterEnabled: true})
	svc := NewCaptchaService(db, &fakeStore{answer: "A1B2"})

	cases := []struct{ id, answer string }{
		{"fake-id", "ZZZZ"}, // 答案错
		{"unknown", "A1B2"}, // id 不存在（或已过期）
		{"", "A1B2"},        // 前端没带上 id
	}
	for _, tc := range cases {
		err := svc.Verify(CaptchaSceneLogin, tc.id, tc.answer)
		bizErr, ok := AsError(err)
		if !ok {
			t.Fatalf("id=%q answer=%q 期望业务错误，得到 %v", tc.id, tc.answer, err)
		}
		if bizErr.Message != "验证码错误或已过期" {
			t.Fatalf("id=%q 提示为 %q", tc.id, bizErr.Message)
		}
	}
}

// 空白答案要给出「请输入验证码」这类可读提示，而不是笼统的校验失败。
func TestCaptchaVerifyRejectsBlankAnswer(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{LoginEnabled: true, RegisterEnabled: true})
	store := &fakeStore{answer: "A1B2"}
	svc := NewCaptchaService(db, store)

	for _, answer := range []string{"", "   ", "\t\n"} {
		err := svc.Verify(CaptchaSceneLogin, "fake-id", answer)
		bizErr, ok := AsError(err)
		if !ok {
			t.Fatalf("answer=%q 期望业务错误，得到 %v", answer, err)
		}
		if bizErr.Message != "请输入验证码" {
			t.Fatalf("answer=%q 提示为 %q", answer, bizErr.Message)
		}
	}
	if len(store.checked) != 0 {
		t.Fatalf("空答案不该查存储，实际查了 %d 次", len(store.checked))
	}
}

// 两个开关必须各自独立生效，不能一开都开。
func TestCaptchaVerifyRespectsPerSceneSwitch(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{LoginEnabled: true, RegisterEnabled: false})
	svc := NewCaptchaService(db, &fakeStore{answer: "A1B2"})

	if err := svc.Verify(CaptchaSceneLogin, "", ""); err == nil {
		t.Fatal("登录开关已开，应当拦下空答案")
	}
	if err := svc.Verify(CaptchaSceneRegister, "", ""); err != nil {
		t.Fatalf("注册开关已关，应当放行，得到: %v", err)
	}
}

// 未知场景按注册开关处理：新增场景时若忘了加分支，宁可跟着注册走也别默认放行。
func TestCaptchaVerifyUnknownSceneFollowsRegisterSwitch(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{LoginEnabled: false, RegisterEnabled: true})
	svc := NewCaptchaService(db, &fakeStore{answer: "A1B2"})
	if err := svc.Verify("某个新场景", "", ""); err == nil {
		t.Fatal("注册开关已开，未知场景应当被拦")
	}
}

func TestCaptchaIssueUsesConfiguredCharsetAndLength(t *testing.T) {
	db := newTestDB(t)
	saveCaptcha(t, db, CaptchaConfig{RegisterEnabled: true, Charset: captcha.CharsetLetter, Length: 8})
	store := &fakeStore{}
	svc := NewCaptchaService(db, store)

	ch, err := svc.Issue()
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if ch.ID == "" || ch.Image == "" {
		t.Fatalf("签发结果不完整: %+v", ch)
	}
	if store.charset != captcha.CharsetLetter || store.length != 8 {
		t.Fatalf("签发用了 %s/%d，期望 %s/8", store.charset, store.length, captcha.CharsetLetter)
	}
}

// 空库也要能出图：默认配置就是 6 位纯数字。
func TestCaptchaIssueFallsBackToDefaults(t *testing.T) {
	db := newTestDB(t)
	store := &fakeStore{}
	if _, err := NewCaptchaService(db, store).Issue(); err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if store.charset != captcha.CharsetDigit || store.length != captcha.DefaultLength {
		t.Fatalf("签发用了 %s/%d，期望 %s/%d",
			store.charset, store.length, captcha.CharsetDigit, captcha.DefaultLength)
	}
}

func TestCaptchaIssuePropagatesError(t *testing.T) {
	db := newTestDB(t)
	want := errors.New("渲染炸了")
	if _, err := NewCaptchaService(db, &fakeStore{err: want}).Issue(); !errors.Is(err, want) {
		t.Fatalf("错误 = %v，期望 %v", err, want)
	}
}
