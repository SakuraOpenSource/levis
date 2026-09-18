// Package loginlimit 提供登录失败的进程内限速：按账号与来源 IP 双维度计数，
// 超过阈值后临时锁定，防止在线爆破。
//
// 刻意做成进程内存态而非数据库表：登录失败计数是短生命周期的防御数据，落库
// 反而给攻击者一个写放大入口（每次失败都写盘）。重启清零是可接受的取舍 ——
// 重启同样打断攻击者已经积累的爆破进度，而合法用户没有需要保留的失败计数。
package loginlimit

import (
	"strings"
	"sync"
	"time"
)

// 限速参数。
//
// 账号维度采用「连续失败计数 + 指数锁定」：失败 5 次锁 15 分钟，攻击持续则
// 15→30→60 分钟翻倍，封顶 24 小时；计数在 1 小时无新失败后归零，偶发输错的
// 正常用户不会叠加。IP 维度是滑动窗口计数，阈值放宽到 50 次/小时，避免共享
// 出口（公司 NAT、校园网）的无关用户被个别攻击者连坐。
const (
	accountFailLimit = 5
	accountWindow    = time.Hour
	accountLockBase  = 15 * time.Minute
	ipFailLimit      = 50
	ipWindow         = time.Hour
	ipLock           = 15 * time.Minute
	maxLock          = 24 * time.Hour
	recordTTL        = 24 * time.Hour
	sweepInterval    = 10 * time.Minute
	// maxRecords 是账号记录数的硬上限（内存保护）。正常流量下远达不到：
	// IP 维度限速已经把单来源的记录创建速率压住了，这里只是防御性兜底。
	maxRecords = 100_000
)

// accountRecord 是单个账号（含入口 scope）的失败状态。
type accountRecord struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// sourceRecord 是单个来源 IP 的失败状态。
type sourceRecord struct {
	failures    []time.Time // 滑动窗口内的失败时间点
	lockedUntil time.Time
	lastFailure time.Time
}

// Tracker 是进程内共用的登录限速器。签发与校验分属两次请求，必须整个进程
// 共用一份；Handler 持有它并在 Close 时释放后台清扫协程。
type Tracker struct {
	now func() time.Time

	mu       sync.Mutex
	accounts map[string]*accountRecord
	sources  map[string]*sourceRecord

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 构造使用真实时钟的 Tracker。
func New() *Tracker {
	return NewWithClock(time.Now)
}

// NewWithClock 允许测试注入假时钟；生产代码一律用 New。
func NewWithClock(now func() time.Time) *Tracker {
	t := &Tracker{
		now:      now,
		accounts: make(map[string]*accountRecord),
		sources:  make(map[string]*sourceRecord),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go t.sweepLoop()
	return t
}

// Close 停止后台清扫协程。可重复调用。
func (t *Tracker) Close() {
	t.stopOnce.Do(func() { close(t.stop) })
}

// Check 报告该入口的账号与来源 IP 当前是否允许尝试登录。
// 被拒绝时 wait 是剩余锁定时长；提示不区分账号锁定还是来源锁定，
// 避免向探测方暴露锁定的具体维度。
func (t *Tracker) Check(scope, identifier, ip string) (ok bool, wait time.Duration) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	wait = time.Duration(0)
	if r := t.accounts[accountKey(scope, identifier)]; r != nil {
		if d := r.lockedUntil.Sub(now); d > wait {
			wait = d
		}
	}
	if s := t.sources[ip]; s != nil {
		if d := s.lockedUntil.Sub(now); d > wait {
			wait = d
		}
	}
	return wait <= 0, wait
}

// RecordFailure 记录一次凭证校验失败。达到阈值即进入锁定。
//
// 未知账号同样计数：按账号维度看，不存在的 identifier 永远失败，把它计入
// 同一个键可以挡住「换着不存在账号刷」的绕法；按 IP 维度看，这类请求正是
// 撞库探测的主要形态。验证码失败、参数格式错误这类「还没走到凭证校验」的
// 失败不应调用本方法 —— 否则攻击者可以不碰密码就把别人的账号锁死。
func (t *Tracker) RecordFailure(scope, identifier, ip string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordAccountFailure(scope, identifier, now)
	t.recordSourceFailure(ip, now)
}

// RecordSuccess 清除该入口 scope 下账号的失败计数。
// 来源维度的历史失败保留，靠滑动窗口自然过期 —— 一次成功登录不应洗白
// 同一 IP 上的撞库记录。
func (t *Tracker) RecordSuccess(scope, identifier string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.accounts, accountKey(scope, identifier))
}

func (t *Tracker) recordAccountFailure(scope, identifier string, now time.Time) {
	key := accountKey(scope, identifier)
	r := t.accounts[key]
	if r == nil {
		if len(t.accounts) >= maxRecords {
			// 兜底：先清一轮过期记录，仍超限则复用（覆盖）既有条目，
			// 绝不让 map 无界增长。
			t.sweepLocked(now)
			if len(t.accounts) >= maxRecords {
				return
			}
		}
		r = &accountRecord{}
		t.accounts[key] = r
	}
	if now.Sub(r.lastFailure) > accountWindow {
		r.failures = 0
	}
	r.failures++
	r.lastFailure = now
	if r.failures >= accountFailLimit {
		lock := accountLockBase << uint(r.failures-accountFailLimit)
		if lock <= 0 || lock > maxLock {
			lock = maxLock
		}
		if until := now.Add(lock); until.After(r.lockedUntil) {
			r.lockedUntil = until
		}
	}
}

func (t *Tracker) recordSourceFailure(ip string, now time.Time) {
	s := t.sources[ip]
	if s == nil {
		s = &sourceRecord{}
		t.sources[ip] = s
	}
	window := s.failures[:0]
	for _, ts := range s.failures {
		if now.Sub(ts) <= ipWindow {
			window = append(window, ts)
		}
	}
	window = append(window, now)
	// 只保留最近 ipFailLimit 个时间点：锁定期内不会再有新记录，窗口膨胀
	// 没有意义，反而白白占内存。
	if len(window) > ipFailLimit {
		window = window[len(window)-ipFailLimit:]
	}
	s.failures = window
	s.lastFailure = now
	if len(s.failures) >= ipFailLimit {
		if until := now.Add(ipLock); until.After(s.lockedUntil) {
			s.lockedUntil = until
		}
	}
}

// sweepLoop 周期清理早已过期且无锁定残留的记录。
func (t *Tracker) sweepLoop() {
	defer close(t.done)
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			t.sweepLocked(t.now())
		}
	}
}

// sweepLocked 清理过期记录，调用方必须已持有锁。
func (t *Tracker) sweepLocked(now time.Time) {
	for key, r := range t.accounts {
		if now.Sub(r.lastFailure) > recordTTL && now.After(r.lockedUntil) {
			delete(t.accounts, key)
		}
	}
	for key, s := range t.sources {
		if now.Sub(s.lastFailure) > ipWindow && now.After(s.lockedUntil) {
			delete(t.sources, key)
		}
	}
}

// accountKey 生成账号维度的键。入口 scope 一并编入：普通入口与管理员入口
// 各自计数，攻击者在普通入口上的失败不会把管理员从管理员入口锁在外面，
// 反之亦然。identifier 统一小写，防大小写变体绕过计数（MySQL 默认排序规则
// 下用户名匹配大小写不敏感，可用来每个变体重置一次计数）。
func accountKey(scope, identifier string) string {
	return scope + "|" + strings.ToLower(strings.TrimSpace(identifier))
}
