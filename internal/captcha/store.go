package captcha

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// TTL 是验证码的有效期。够用户看清并输入，又不至于让答案长期可用。
const TTL = 5 * time.Minute

// maxEntries 是待校验验证码的驻留上限。
//
// 签发接口是公开的，没有上限的话，脚本反复请求就能把内存撑爆。超额时按
// 到期时间从早到晚淘汰，牺牲最老的那批（它们本来也快过期了）。
const maxEntries = 8192

// Challenge 是一次验证码挑战。Image 是可直接用作 <img src> 的 data URL；
// 答案只留在服务端，绝不下发。
type Challenge struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	ExpiresIn int    `json:"expires_in"`
}

// entry 是服务端保存的答案。
type entry struct {
	code    string
	expires time.Time
}

// Store 是验证码的内存存储。
//
// 不落库：验证码是几分钟内就作废的一次性数据，写数据库既无必要也会拖慢
// 登录注册。代价是多进程部署时签发与校验必须落在同一进程 —— Levis 以单
// 二进制单进程交付，这个前提成立。
type Store struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]entry
}

// NewStore 构造一个空的验证码存储。
func NewStore() *Store {
	return &Store{ttl: TTL, items: make(map[string]entry)}
}

// Issue 按指定字符集与位数签发一张验证码。
func (s *Store) Issue(charset string, length int) (*Challenge, error) {
	length = ClampLength(length)
	code, err := randomCode(charset, length)
	if err != nil {
		return nil, err
	}
	image, err := render(code)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	s.mu.Lock()
	s.evictLocked(now)
	s.items[id] = entry{code: code, expires: now.Add(s.ttl)}
	s.mu.Unlock()

	return &Challenge{ID: id, Image: dataURL(image), ExpiresIn: int(s.ttl.Seconds())}, nil
}

// Verify 校验答案，忽略大小写与首尾空格。
//
// 无论对错都立即作废该 id：留着的话，攻击者可以拿同一张图反复试，位数再多
// 也挡不住穷举。
func (s *Store) Verify(id, answer string) bool {
	if id == "" || answer == "" {
		return false
	}
	s.mu.Lock()
	item, ok := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()

	if !ok || time.Now().After(item.expires) {
		return false
	}
	got := strings.ToUpper(strings.TrimSpace(answer))
	return subtle.ConstantTimeCompare([]byte(got), []byte(item.code)) == 1
}

// Len 返回当前待校验的验证码数量，供测试观察淘汰行为。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// evictLocked 清理过期项；仍然超额时按到期时间淘汰最老的一批。
// 调用方必须已持有锁。
func (s *Store) evictLocked(now time.Time) {
	for id, item := range s.items {
		if now.After(item.expires) {
			delete(s.items, id)
		}
	}
	if len(s.items) < maxEntries {
		return
	}
	type aged struct {
		id      string
		expires time.Time
	}
	all := make([]aged, 0, len(s.items))
	for id, item := range s.items {
		all = append(all, aged{id: id, expires: item.expires})
	}
	slices.SortFunc(all, func(a, b aged) int { return a.expires.Compare(b.expires) })
	// 留出一个空位给本次签发。
	for _, item := range all[:len(all)-maxEntries+1] {
		delete(s.items, item.id)
	}
}

// newID 生成验证码 id。它只是查找键，但仍用密码学随机数：可猜测的 id
// 会让攻击者能挑走别人的验证码，制造无谓的失败。
func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成验证码标识失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
