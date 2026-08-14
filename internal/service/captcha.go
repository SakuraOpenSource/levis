package service

import (
	"strings"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/captcha"
)

// 验证码的使用场景。
const (
	CaptchaSceneLogin    = "login"
	CaptchaSceneRegister = "register"
)

// CaptchaStore 是验证码存储需要满足的最小接口。
//
// 抽成接口而非直接写死 *captcha.Store：真实存储只把答案留在服务端，测试拿
// 不到答案，也就无法构造「答对」这条路径。有了接口就能注入假存储。
type CaptchaStore interface {
	Issue(charset string, length int) (*captcha.Challenge, error)
	Verify(id, answer string) bool
}

// CaptchaService 按站点配置签发与校验验证码。
type CaptchaService struct {
	settings *SettingService
	store    CaptchaStore
}

// NewCaptchaService 构造 CaptchaService。store 必须是进程内共用的那一个 ——
// 签发与校验分属两次请求，各自新建存储会让刚发出去的验证码查无此码。
func NewCaptchaService(db *gorm.DB, store CaptchaStore) *CaptchaService {
	return &CaptchaService{settings: NewSettingService(db), store: store}
}

// Issue 按当前配置签发一张验证码。
//
// 不区分场景：字符集与位数是全站配置，哪个表单要用由前端按 bootstrap 的
// 开关决定。多发一张没被用到的验证码没有副作用，几分钟后自行过期。
func (s *CaptchaService) Issue() (*captcha.Challenge, error) {
	cfg := s.settings.Captcha()
	return s.store.Issue(cfg.Charset, cfg.Length)
}

// Verify 校验指定场景的验证码；该场景未开启时直接放行。
//
// 调用点必须放在校验账号密码之前：否则攻击者可以无视验证码直接拿接口撞库，
// 验证码等于白加。
func (s *CaptchaService) Verify(scene, id, answer string) error {
	cfg := s.settings.Captcha()
	enabled := cfg.RegisterEnabled
	if scene == CaptchaSceneLogin {
		enabled = cfg.LoginEnabled
	}
	if !enabled {
		return nil
	}
	if strings.TrimSpace(answer) == "" {
		return ErrBadRequest("请输入验证码")
	}
	if !s.store.Verify(id, answer) {
		return ErrBadRequest("验证码错误或已过期")
	}
	return nil
}
