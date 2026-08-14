package service

import (
	"testing"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/captcha"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// settingValue 直接读设置表某一项，绕过默认值回落逻辑。
func settingValue(t *testing.T, db *gorm.DB, key string) (string, bool) {
	t.Helper()
	var rows []model.Setting
	if err := db.Where(map[string]any{"key": key}).Find(&rows).Error; err != nil {
		t.Fatalf("读取设置 %s 失败: %v", key, err)
	}
	if len(rows) == 0 {
		return "", false
	}
	return rows[0].Value, true
}

// 空库必须给出「注册开、登录关、6 位纯数字」这套默认值：老库升级上来不做
// 迁移也要能正常跑。
func TestCaptchaDefaultsOnEmptyDatabase(t *testing.T) {
	db := newTestDB(t)
	got := NewSettingService(db).Captcha()
	want := DefaultCaptchaConfig()
	if got != want {
		t.Fatalf("默认配置 = %+v，期望 %+v", got, want)
	}
	if want.LoginEnabled {
		t.Fatal("登录验证码默认应关闭")
	}
	if !want.RegisterEnabled {
		t.Fatal("注册验证码默认应开启")
	}
	if want.Charset != captcha.CharsetDigit || want.Length != 6 {
		t.Fatalf("默认应为 6 位纯数字，得到 %s/%d", want.Charset, want.Length)
	}
}

func TestSaveCaptchaRoundTrip(t *testing.T) {
	db := newTestDB(t)
	svc := NewSettingService(db)

	in := CaptchaConfig{LoginEnabled: true, RegisterEnabled: false, Charset: captcha.CharsetMixed, Length: 5}
	saved, err := svc.SaveCaptcha(in)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if saved != in {
		t.Fatalf("返回值 = %+v，期望 %+v", saved, in)
	}
	if got := svc.Captcha(); got != in {
		t.Fatalf("重新读取 = %+v，期望 %+v", got, in)
	}

	// 再存一次不同的值，验证走的是 upsert 而非重复插入。
	next := CaptchaConfig{LoginEnabled: false, RegisterEnabled: true, Charset: captcha.CharsetLetter, Length: 8}
	if _, err := svc.SaveCaptcha(next); err != nil {
		t.Fatalf("二次保存失败: %v", err)
	}
	if got := svc.Captcha(); got != next {
		t.Fatalf("二次保存后读取 = %+v，期望 %+v", got, next)
	}
	var count int64
	if err := db.Model(&model.Setting{}).Count(&count).Error; err != nil {
		t.Fatalf("统计设置项失败: %v", err)
	}
	if count != 4 {
		t.Fatalf("设置表有 %d 行，期望 4 行（重复插入了）", count)
	}
}

func TestSaveCaptchaKeepsSiteSettings(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Setting{Key: model.SettingSiteName, Value: "Levis"}).Error; err != nil {
		t.Fatalf("写入站点名称失败: %v", err)
	}
	if _, err := NewSettingService(db).SaveCaptcha(DefaultCaptchaConfig()); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got, ok := settingValue(t, db, model.SettingSiteName); !ok || got != "Levis" {
		t.Fatalf("站点名称被破坏，得到 %q（存在: %v）", got, ok)
	}
}

func TestSaveCaptchaRejectsInvalidInput(t *testing.T) {
	db := newTestDB(t)
	svc := NewSettingService(db)
	cases := []struct {
		name string
		in   CaptchaConfig
	}{
		{"空字符集", CaptchaConfig{Charset: "", Length: 6}},
		{"未知字符集", CaptchaConfig{Charset: "number", Length: 6}},
		{"位数过小", CaptchaConfig{Charset: captcha.CharsetDigit, Length: captcha.MinLength - 1}},
		{"位数过大", CaptchaConfig{Charset: captcha.CharsetDigit, Length: captcha.MaxLength + 1}},
		{"位数为零", CaptchaConfig{Charset: captcha.CharsetDigit, Length: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SaveCaptcha(tc.in); err == nil {
				t.Fatalf("%+v 应当被拒绝", tc.in)
			}
			// 校验失败不能留下半套配置。
			if _, ok := settingValue(t, db, model.SettingCaptchaCharset); ok {
				t.Fatal("校验失败却写入了设置")
			}
		})
	}
}

func TestSaveCaptchaTrimsCharset(t *testing.T) {
	db := newTestDB(t)
	svc := NewSettingService(db)
	saved, err := svc.SaveCaptcha(CaptchaConfig{Charset: "  digit\n", Length: 6})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if saved.Charset != captcha.CharsetDigit {
		t.Fatalf("字符集 = %q，期望 %q", saved.Charset, captcha.CharsetDigit)
	}
}

// 设置表被手工改坏时，非法项应逐项回落到默认值，而不是让登录注册整体失灵。
func TestCaptchaFallsBackOnCorruptValues(t *testing.T) {
	db := newTestDB(t)
	rows := []model.Setting{
		{Key: model.SettingCaptchaLogin, Value: "yes"},   // 非 "1" 即视为关
		{Key: model.SettingCaptchaRegister, Value: "1"},  // 开
		{Key: model.SettingCaptchaCharset, Value: "abc"}, // 非法 → 默认
		{Key: model.SettingCaptchaLength, Value: "oops"}, // 非法 → 默认
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
	got := NewSettingService(db).Captcha()
	want := CaptchaConfig{
		LoginEnabled:    false,
		RegisterEnabled: true,
		Charset:         captcha.CharsetDigit,
		Length:          captcha.DefaultLength,
	}
	if got != want {
		t.Fatalf("读取 = %+v，期望 %+v", got, want)
	}
}

// 越界的位数在读取端也要收敛，哪怕它是被手工塞进库里的。
func TestCaptchaClampsStoredLength(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Setting{Key: model.SettingCaptchaLength, Value: "99"}).Error; err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
	if got := NewSettingService(db).Captcha().Length; got != captcha.MaxLength {
		t.Fatalf("位数 = %d，期望收敛到 %d", got, captcha.MaxLength)
	}
}
