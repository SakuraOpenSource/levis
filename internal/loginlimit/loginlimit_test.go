package loginlimit

import (
	"fmt"
	"testing"
	"time"
)

// newTestTracker 构造使用假时钟的 Tracker，并返回推进时钟的函数。
func newTestTracker(t *testing.T) (*Tracker, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tracker := NewWithClock(func() time.Time { return now })
	t.Cleanup(tracker.Close)
	advance := func(d time.Duration) { now = now.Add(d) }
	return tracker, advance
}

// 连续失败达到阈值后账号被锁定，且锁定期内换一个来源 IP 仍被拒绝 ——
// 账号锁定不能被换 IP 绕过。
func TestAccountLockSurvivesIPChange(t *testing.T) {
	tracker, _ := newTestTracker(t)
	const scope = "login"
	for i := 0; i < accountFailLimit; i++ {
		if ok, _ := tracker.Check(scope, "alice", "1.2.3.4"); !ok {
			t.Fatalf("第 %d 次失败前不应被拒", i+1)
		}
		tracker.RecordFailure(scope, "alice", "1.2.3.4")
	}
	ok, wait := tracker.Check(scope, "alice", "5.6.7.8")
	if ok {
		t.Fatal("达到阈值后应锁定账号")
	}
	if wait != accountLockBase {
		t.Fatalf("首次锁定应为 %v，实际 %v", accountLockBase, wait)
	}
}

// 持续攻击下锁定时长指数翻倍，封顶 24 小时。
func TestAccountLockEscalates(t *testing.T) {
	tracker, advance := newTestTracker(t)
	const scope = "login"
	// 首轮：5 次失败 → 15 分钟。
	for i := 0; i < accountFailLimit; i++ {
		tracker.RecordFailure(scope, "admin", "1.2.3.4")
	}
	if _, wait := tracker.Check(scope, "admin", "1.2.3.4"); wait != accountLockBase {
		t.Fatalf("首轮锁定应为 %v，实际 %v", accountLockBase, wait)
	}
	// 锁定结束后再失败一次 → 30 分钟。
	advance(accountLockBase + time.Second)
	tracker.RecordFailure(scope, "admin", "1.2.3.4")
	if _, wait := tracker.Check(scope, "admin", "1.2.3.4"); wait < 2*accountLockBase {
		t.Fatalf("第二轮锁定应翻倍到 %v，实际 %v", 2*accountLockBase, wait)
	}
	// 连续失败足够多次后封顶在 24 小时。
	for i := 0; i < 30; i++ {
		advance(maxLock)
		tracker.RecordFailure(scope, "admin", "1.2.3.4")
	}
	if _, wait := tracker.Check(scope, "admin", "1.2.3.4"); wait > maxLock {
		t.Fatalf("锁定不应超过 %v，实际 %v", maxLock, wait)
	}
}

// 一小时窗口外的旧失败不再叠加：计数归零，锁长回到基准值。
func TestAccountCounterDecays(t *testing.T) {
	tracker, advance := newTestTracker(t)
	const scope = "login"
	tracker.RecordFailure(scope, "bob", "1.2.3.4")
	tracker.RecordFailure(scope, "bob", "1.2.3.4")
	advance(accountWindow + time.Minute)
	for i := 0; i < accountFailLimit; i++ {
		tracker.RecordFailure(scope, "bob", "1.2.3.4")
	}
	if _, wait := tracker.Check(scope, "bob", "1.2.3.4"); wait != accountLockBase {
		t.Fatalf("旧失败过期后锁定应回到 %v，实际 %v", accountLockBase, wait)
	}
}

// 成功登录清除账号失败计数。
func TestSuccessResetsAccount(t *testing.T) {
	tracker, _ := newTestTracker(t)
	const scope = "login"
	for i := 0; i < accountFailLimit-1; i++ {
		tracker.RecordFailure(scope, "carol", "1.2.3.4")
	}
	tracker.RecordSuccess(scope, "carol")
	for i := 0; i < accountFailLimit-1; i++ {
		tracker.RecordFailure(scope, "carol", "1.2.3.4")
	}
	if ok, _ := tracker.Check(scope, "carol", "1.2.3.4"); !ok {
		t.Fatal("成功登录应清零失败计数，不该在此时锁定")
	}
}

// 入口 scope 隔离：普通入口的失败不影响管理员入口的计数。
func TestScopeIsolation(t *testing.T) {
	tracker, _ := newTestTracker(t)
	for i := 0; i < accountFailLimit; i++ {
		tracker.RecordFailure("login", "admin", "1.2.3.4")
	}
	if ok, _ := tracker.Check("admin", "admin", "1.2.3.4"); !ok {
		t.Fatal("普通入口的失败不应锁住管理员入口")
	}
}

// identifier 大小写与首尾空白不影响计数键。
func TestAccountKeyNormalization(t *testing.T) {
	tracker, _ := newTestTracker(t)
	const scope = "login"
	for i := 0; i < accountFailLimit; i++ {
		tracker.RecordFailure(scope, "Alice", "1.2.3.4")
	}
	if ok, _ := tracker.Check(scope, "  alice ", "1.2.3.4"); ok {
		t.Fatal("大小写/空白变体不应绕过账号锁定")
	}
}

// 来源 IP 的滑动窗口：单 IP 失败满 50 次锁定 15 分钟，其他 IP 不受影响。
func TestSourceLock(t *testing.T) {
	tracker, advance := newTestTracker(t)
	const scope = "login"
	// 49 次失败分散在多个账号上（撞库形态）。
	for i := 0; i < ipFailLimit-1; i++ {
		tracker.RecordFailure(scope, fmt.Sprintf("user%d", i), "9.9.9.9")
	}
	if ok, _ := tracker.Check(scope, "somebody", "9.9.9.9"); !ok {
		t.Fatal("未到阈值不应锁定来源")
	}
	tracker.RecordFailure(scope, "final", "9.9.9.9")
	ok, wait := tracker.Check(scope, "anyone", "9.9.9.9")
	if ok {
		t.Fatal("单 IP 失败满阈值应锁定来源")
	}
	if wait != ipLock {
		t.Fatalf("来源锁定应为 %v，实际 %v", ipLock, wait)
	}
	if ok, _ := tracker.Check(scope, "somebody", "8.8.8.8"); !ok {
		t.Fatal("来源锁定不应波及其他 IP")
	}
	// 锁定过期后，窗口里的旧失败还在：再来一次失败立即重新锁定。
	advance(ipLock + time.Second)
	tracker.RecordFailure(scope, "again", "9.9.9.9")
	if ok, _ := tracker.Check(scope, "x", "9.9.9.9"); ok {
		t.Fatal("锁定过期后窗口残留的失败应让下一次失败立刻重新锁定")
	}
}

// 成功登录不清洗来源维度的失败记录。
func TestSuccessKeepsSourceHistory(t *testing.T) {
	tracker, _ := newTestTracker(t)
	const scope = "login"
	for i := 0; i < ipFailLimit-1; i++ {
		tracker.RecordFailure(scope, fmt.Sprintf("u%d", i), "7.7.7.7")
	}
	tracker.RecordSuccess(scope, "u0")
	tracker.RecordFailure(scope, "final", "7.7.7.7")
	if ok, _ := tracker.Check(scope, "any", "7.7.7.7"); ok {
		t.Fatal("成功登录不应清零来源维度的失败窗口")
	}
}

// 过期记录被清扫掉：长时间无活动后内存不残留。
func TestSweepRemovesStaleRecords(t *testing.T) {
	tracker, advance := newTestTracker(t)
	tracker.RecordFailure("login", "gone", "1.1.1.1")
	advance(recordTTL + time.Minute)
	// 直接调用内部清扫（假时钟下 ticker 永不触发）。
	tracker.mu.Lock()
	tracker.sweepLocked(tracker.now())
	accounts := len(tracker.accounts)
	sources := len(tracker.sources)
	tracker.mu.Unlock()
	if accounts != 0 || sources != 0 {
		t.Fatalf("过期记录应被清扫，剩账号 %d 条 / 来源 %d 条", accounts, sources)
	}
}
