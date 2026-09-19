package auth

import (
	"sync"
	"time"
)

// revocationSweepInterval 是吊销表的清扫周期。
const revocationSweepInterval = 10 * time.Minute

// RevocationList 是进程内的会话吊销表（登出失效）。
//
// JWT 本身无状态：登出只是让浏览器删掉 cookie，签出去的 token 在到期前
// 依旧有效，任何持有者都能继续用。这里按 token 的 jti 记录「已主动失效」，
// RequireAuth 每次请求顺带查一次（一次 map 读，代价可忽略）。
//
// 与 loginlimit 同一取舍：只存进程内存。重启后吊销表清空，但 token 仍在
// —— 这是已知且可接受的边界；配合改密踢会话（password_changed_at 比对，
// 见 middleware）覆盖了「凭证可能已被拿走」的高危场景。
type RevocationList struct {
	mu       sync.Mutex
	revoked  map[string]time.Time // key = jti，value = token 自然到期时间
	stopOnce sync.Once
	stop     chan struct{}
}

// NewRevocationList 构造吊销表并启动后台清扫。
func NewRevocationList() *RevocationList {
	r := &RevocationList{
		revoked: make(map[string]time.Time),
		stop:    make(chan struct{}),
	}
	go r.sweepLoop()
	return r
}

// Close 停止后台清扫。可重复调用。
func (r *RevocationList) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// Revoke 吊销指定 jti 的 token，直到其自然到期时间。
//
// expiry 之后的条目没有意义（token 本身已失效），清扫时会移除；
// 显式传入更早的到期时间不会解除吊销 —— 只会延长，绝不缩短。
func (r *RevocationList) Revoke(jti string, expiry time.Time) {
	if jti == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until, ok := r.revoked[jti]; !ok || expiry.After(until) {
		r.revoked[jti] = expiry
	}
}

// IsRevoked 报告该 jti 是否已被吊销。
func (r *RevocationList) IsRevoked(jti string) bool {
	if jti == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.revoked[jti]
	return ok
}

// sweepLoop 周期清掉已自然过期的条目，防止 map 无界增长。
func (r *RevocationList) sweepLoop() {
	ticker := time.NewTicker(revocationSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			r.mu.Lock()
			for jti, expiry := range r.revoked {
				if now.After(expiry) {
					delete(r.revoked, jti)
				}
			}
			r.mu.Unlock()
		}
	}
}
