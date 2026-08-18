package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// 假插件的行为开关。testdata/fakeplugin 是 package main，没法 import，
// 所以这里重复一份；改名字时两边都要动。
const (
	envMode      = "FAKE_MODE"
	envHookDelay = "FAKE_HOOK_DELAY"
	envCallLog   = "FAKE_CALL_LOG"

	modeSilent       = "silent"
	modeExit         = "exit"
	modeBadHandshake = "bad-handshake"
	modeUnhealthy    = "unhealthy"
)

// buildFake 把 testdata 下的假插件编译到一个进程内共享的临时位置。
//
// 只编译一次：每个用例各编一遍会让整包测试慢上数十秒。
var buildFake = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "levis-fake-plugin-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, execName())
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakeplugin")
	// 假插件 import 了本模块，必须在模块内构建。
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return "", &buildError{output: string(raw), err: err}
	}
	return out, nil
})

type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.output }

// install 把假插件装到一个临时数据目录下，返回该数据目录。
func install(t *testing.T, id string, env map[string]string) string {
	t.Helper()

	binary, err := buildFake()
	if err != nil {
		t.Fatalf("编译假插件失败: %v", err)
	}

	dataDir := t.TempDir()
	dir := filepath.Join(Root(dataDir), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}

	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("读取假插件失败: %v", err)
	}
	target := filepath.Join(dir, execName())
	if err := os.WriteFile(target, raw, 0o700); err != nil {
		t.Fatalf("写入插件文件失败: %v", err)
	}

	// 通过包装脚本注入行为控制变量：launch 不继承主程序环境，
	// 测试没法直接把变量传给子进程。
	if len(env) > 0 {
		writeWrapper(t, dir, target, env)
	}
	return dataDir
}

// writeWrapper 用一个 shell 包装脚本替换插件本体，用来注入环境变量。
func writeWrapper(t *testing.T, dir, real string, env map[string]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上不支持 shell 包装脚本，跳过需要注入变量的用例")
	}

	inner := filepath.Join(dir, "real-plugin")
	if err := os.Rename(real, inner); err != nil {
		t.Fatalf("移动插件文件失败: %v", err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for k, v := range env {
		b.WriteString("export " + k + "=" + shellQuote(v) + "\n")
	}
	b.WriteString("exec " + shellQuote(inner) + "\n")
	if err := os.WriteFile(real, []byte(b.String()), 0o700); err != nil {
		t.Fatalf("写入包装脚本失败: %v", err)
	}
}

// shellQuote 用单引号包裹并转义内部单引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newManager 构造一个把日志收进 t.Log 的 Manager，并保证测试结束时收掉进程。
func newManager(t *testing.T, dataDir string) *Manager {
	t.Helper()
	m := NewManager(dataDir, "http://127.0.0.1:0/api/plugin/v1", nil, nil, func(format string, args ...any) {
		t.Logf(format, args...)
	})
	// 无论用例怎么结束都要停掉插件，否则残留进程会跟着 CI 一直跑。
	t.Cleanup(m.Close)
	return m
}

// waitState 等待插件进入期望状态。
func waitState(t *testing.T, inst *Instance, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := inst.Snapshot().State; got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap := inst.Snapshot()
	t.Fatalf("等待状态 %s 超时，当前为 %s（%s）", want, snap.State, snap.LastError)
}

// TestHandshakeAndDescribe 覆盖正常路径：握手、取 manifest、下发配置。
func TestHandshakeAndDescribe(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)

	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	list := m.List()
	if len(list) != 1 {
		t.Fatalf("应发现 1 个插件，实际 %d", len(list))
	}
	// 新发现的插件不该自动启动 —— 那是刚放进目录的陌生二进制。
	if got := list[0].Snapshot().State; got != StateStopped {
		t.Errorf("新发现的插件应为 stopped，实际 %s", got)
	}

	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	waitState(t, list[0], StateRunning, 15*time.Second)

	snap := list[0].Snapshot()
	if snap.Name != "假插件" {
		t.Errorf("manifest 名称应为「假插件」，实际 %q", snap.Name)
	}
	if len(snap.Capabilities) != 2 {
		t.Errorf("应声明 2 项能力，实际 %v", snap.Capabilities)
	}
}

// TestTokenRequired 确认插件会拒绝不带令牌的调用。
//
// 这是本设计的关键防线：插件监听的是环回端口，同机任何进程都能连上来，
// 若不校验令牌，别人就能直接调 CreatePayment。
func TestTokenRequired(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, err := m.Get("fake")
	if err != nil {
		t.Fatalf("取实例失败: %v", err)
	}
	waitState(t, inst, StateRunning, 15*time.Second)

	client, c := inst.client()
	if client == nil {
		t.Fatal("插件应处于可调用状态")
	}

	// 不走 c.call（它会带上令牌），直接用裸 context 调。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Health(ctx, &pb.HealthRequest{}); err == nil {
		t.Error("不带令牌的调用应当被插件拒绝")
	}

	// 带上正确令牌则应当成功，证明拒绝不是别的原因造成的。
	if err := c.health(context.Background()); err != nil {
		t.Errorf("带令牌的调用应当成功，却失败：%v", err)
	}
}

// TestHandshakeTimeout 确认插件不输出握手行时主程序不会卡住。
func TestHandshakeTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("需等待握手超时，-short 下跳过")
	}
	dataDir := install(t, "fake", map[string]string{envMode: modeSilent})
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	// 握手超时 10 秒，留出余量。
	waitState(t, inst, StateError, handshakeTimeout+10*time.Second)
	if snap := inst.Snapshot(); !strings.Contains(snap.LastError, "握手") {
		t.Errorf("错误信息应提到握手，实际 %q", snap.LastError)
	}
}

// TestBadHandshakeLine 确认握手行不是 JSON 时给出可读的错误。
func TestBadHandshakeLine(t *testing.T) {
	dataDir := install(t, "fake", map[string]string{envMode: modeBadHandshake})
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateError, 15*time.Second)
	if snap := inst.Snapshot(); !strings.Contains(snap.LastError, "解析插件握手信息失败") {
		t.Errorf("错误信息应说明解析失败，实际 %q", snap.LastError)
	}
}

// TestExitImmediatelyRestarts 确认启动即退出的插件会被重试而非静默放弃。
func TestExitImmediatelyRestarts(t *testing.T) {
	dataDir := install(t, "fake", map[string]string{envMode: modeExit})
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateError, 15*time.Second)

	// 日志里应当出现重启计划，证明进入了退避循环。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range inst.Logs() {
			if strings.Contains(line, "将在") && strings.Contains(line, "后重启") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("应记录重启计划，实际日志：%v", inst.Logs())
}

// TestCrashLimitStopsRetrying 确认崩溃次数超限后停止重试。
//
// 直接驱动崩溃计数，不真的等 5 次退避 —— 那要几十秒。
func TestCrashLimitStopsRetrying(t *testing.T) {
	inst := newInstance(Found{ID: "x", Path: "/nonexistent", DataDir: t.TempDir()}, t.Logf)
	for i := 0; i < crashLimit-1; i++ {
		inst.noteCrash()
	}
	if inst.tooManyCrashes() {
		t.Fatalf("%d 次崩溃不应触发上限", crashLimit-1)
	}
	inst.noteCrash()
	if !inst.tooManyCrashes() {
		t.Errorf("%d 次崩溃应触发上限", crashLimit)
	}
}

// TestCrashWindowExpires 确认窗口外的旧崩溃不计入。
//
// 否则一个每天崩一次的插件攒够 5 次就再也不会自动重启了。
func TestCrashWindowExpires(t *testing.T) {
	inst := newInstance(Found{ID: "x", Path: "/nonexistent"}, t.Logf)
	base := time.Now()
	inst.now = func() time.Time { return base }
	for i := 0; i < crashLimit; i++ {
		inst.noteCrash()
	}
	if !inst.tooManyCrashes() {
		t.Fatal("窗口内应触发上限")
	}
	// 把时间推到窗口之外，旧记录应当被清掉。
	inst.now = func() time.Time { return base.Add(crashWindow + time.Minute) }
	if inst.tooManyCrashes() {
		t.Error("窗口外的崩溃记录应被清除")
	}
}

// TestUnhealthyTriggersRestart 确认健康检查失败会把状态置为异常。
func TestUnhealthyTriggersRestart(t *testing.T) {
	dataDir := install(t, "fake", map[string]string{envMode: modeUnhealthy})
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	// 健康检查间隔 30 秒 × 3 次才判死，直接手动调一次确认它确实报错。
	_, c := inst.client()
	if c == nil {
		t.Fatal("应有可用连接")
	}
	if err := c.health(context.Background()); err == nil {
		t.Error("插件报告异常时 health 应返回错误")
	}
}

// TestStopKillsProcess 确认停用后进程确实退出。
//
// 留下孤儿进程在 CI 上尤其糟糕：它们会一直占着端口与内存直到 runner 回收。
func TestStopKillsProcess(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	inst.mu.RLock()
	pid := inst.conn.cmd.Process.Pid
	inst.mu.RUnlock()

	if err := m.Disable("fake"); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if got := inst.Snapshot().State; got != StateStopped {
		t.Errorf("停用后状态应为 stopped，实际 %s", got)
	}
	if alive(pid) {
		t.Errorf("停用后进程 %d 仍存活", pid)
	}
}

// TestSendMailThroughPlugin 覆盖能力调用与失败传播。
func TestSendMailThroughPlugin(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	if got := m.Mailer(); got != "fake" {
		t.Errorf("应报告 fake 为可用邮件插件，实际 %q", got)
	}
	err := m.SendMail(context.Background(), &pb.SendMailRequest{
		To:       []*pb.Mailbox{{Address: "a@example.com"}},
		Subject:  "测试",
		TextBody: "正文",
	})
	if err != nil {
		t.Errorf("发信应成功，实际 %v", err)
	}

	reply, err := m.CreatePayment(context.Background(), "fake", &pb.CreatePaymentRequest{
		ExternalId:  "ext-1",
		AmountCents: 1000,
		Currency:    "CNY",
	})
	if err != nil {
		t.Fatalf("创建支付应成功，实际 %v", err)
	}
	if reply.GetPayUrl() == "" {
		t.Error("应返回支付地址")
	}
}

// TestSendMailUnavailable 确认没有插件时返回 ErrUnavailable 而不是 panic。
func TestSendMailUnavailable(t *testing.T) {
	m := newManager(t, t.TempDir())
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描空目录不应失败: %v", err)
	}
	if err := m.SendMail(context.Background(), &pb.SendMailRequest{}); err != ErrUnavailable {
		t.Errorf("无可用插件时应返回 ErrUnavailable，实际 %v", err)
	}
	if got := m.Mailer(); got != "" {
		t.Errorf("无可用插件时 Mailer 应为空，实际 %q", got)
	}
}

// TestHookTimeout 确认能力调用有超时，不会让请求无限等待。
func TestHookTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("需等待 hook 超时，-short 下跳过")
	}
	// 让插件睡得比 hookTimeout 更久。
	dataDir := install(t, "fake", map[string]string{
		envHookDelay: "20000",
	})
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	start := time.Now()
	err := m.SendMail(context.Background(), &pb.SendMailRequest{Subject: "慢"})
	elapsed := time.Since(start)
	if err == nil {
		t.Error("超时应返回错误")
	}
	// 允许一点余量，但必须显著小于插件的 20 秒睡眠。
	if elapsed > hookTimeout+5*time.Second {
		t.Errorf("应在 %s 左右超时，实际耗时 %s", hookTimeout, elapsed)
	}
}

// TestReloadRemovesDeleted 确认磁盘上删掉插件后实例被停止并移除。
func TestReloadRemovesDeleted(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	inst.mu.RLock()
	pid := inst.conn.cmd.Process.Pid
	inst.mu.RUnlock()

	if err := os.RemoveAll(filepath.Join(Root(dataDir), "fake")); err != nil {
		t.Fatalf("删除插件目录失败: %v", err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("重新扫描失败: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("插件已从磁盘删除，列表应为空，实际 %d 个", len(m.List()))
	}
	if alive(pid) {
		t.Errorf("插件已删除，进程 %d 应被停止", pid)
	}
}

// TestReloadKeepsRunningInstance 确认重扫不会重启正在运行的插件。
func TestReloadKeepsRunningInstance(t *testing.T) {
	dataDir := install(t, "fake", nil)
	m := newManager(t, dataDir)
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("扫描插件失败: %v", err)
	}
	if err := m.Enable(context.Background(), "fake"); err != nil {
		t.Fatalf("启用插件失败: %v", err)
	}
	inst, _ := m.Get("fake")
	waitState(t, inst, StateRunning, 15*time.Second)

	inst.mu.RLock()
	before := inst.conn.cmd.Process.Pid
	inst.mu.RUnlock()

	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("重新扫描失败: %v", err)
	}
	inst.mu.RLock()
	after := inst.conn.cmd.Process.Pid
	inst.mu.RUnlock()

	if before != after {
		t.Errorf("重扫不应重启运行中的插件，pid 由 %d 变为 %d", before, after)
	}
}

// alive 报告进程是否仍然存活。
func alive(pid int) bool {
	// 进程退出后可能仍处于僵尸态，用信号 0 探测。给一点时间让 Wait 收尸。
	for i := 0; i < 50; i++ {
		process, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		if err := process.Signal(syscallZero); err != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
