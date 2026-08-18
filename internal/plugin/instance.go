package plugin

import (
	"context"
	"sync"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// State 是插件实例的运行状态。
type State string

const (
	// StateStopped 表示未运行（管理员停用，或从未启动）。
	StateStopped State = "stopped"
	// StateRunning 表示进程存活且健康检查通过。
	StateRunning State = "running"
	// StateError 表示启动失败或健康检查未通过，仍会按退避重试。
	StateError State = "error"
	// StateCrashed 表示反复崩溃已放弃重试，需管理员介入。
	StateCrashed State = "crashed"
	// StateSkipped 表示磁盘上的文件未通过加载前检查（权限、缺文件等）。
	StateSkipped State = "skipped"
)

// 重启策略。
const (
	// restartBaseDelay 是首次重启前的等待时间，之后逐次翻倍。
	restartBaseDelay = time.Second
	// restartMaxDelay 是退避上限。
	restartMaxDelay = time.Minute
	// crashWindow 与 crashLimit 一起决定何时放弃：窗口内崩溃次数超过上限
	// 就停止重试。没有这个上限，一个启动即崩的插件会无休止地刷日志。
	crashWindow = 10 * time.Minute
	crashLimit  = 5
)

// 健康检查参数。
const (
	healthInterval = 30 * time.Second
	// healthFailLimit 是判定死亡前允许的连续失败次数。给到 3 次是为了
	// 容忍插件偶发的一次慢响应，不因单次抖动就重启。
	healthFailLimit = 3
)

// 关闭时各阶段的等待时长。
const (
	shutdownWait = 3 * time.Second
	termWait     = 5 * time.Second
)

// logLines 是每个插件保留的日志行数。
//
// 环形缓冲存在内存里，不落盘：插件日志同时已经进了主程序的标准日志，这里
// 留的只是给管理界面看的最近若干行。
const logLines = 200

// Instance 是一个插件的运行时实例，并发安全。
type Instance struct {
	// id 是插件 ID，构造后不再变动，因此可以无锁读取。
	//
	// 它就是 Manager.instances 的键，Reload 重扫时只会更新 spec 的其余字段。
	// 单独存一份是为了让 ID() 与写日志的 record() 不必加锁 —— record 本身
	// 已经持有 mu 写日志，再去读 i.spec.ID 会与 Reload 的整体赋值竞争。
	id string

	// spec 是磁盘信息，Reload 时可能整体替换，读写都要持 mu。
	spec Found

	// env 是启动时注入的额外环境变量（API 基址与 Key）。
	env []string
	// values 是最近一次下发的配置。
	values map[string]string

	mu       sync.RWMutex
	state    State
	lastErr  string
	manifest *pb.Manifest
	conn     *conn
	logs     *ring

	// enabled 记录管理员的意图。停用状态下健康检查与重启都不进行。
	enabled bool

	// crashes 是崩溃时间戳，用于判断窗口内的崩溃频率。
	crashes []time.Time

	// cancel 停止当前实例的监控循环。
	cancel context.CancelFunc
	// wg 等待监控循环退出，避免 Stop 返回后 goroutine 还在跑。
	wg sync.WaitGroup

	logf func(string, ...any)
	// now 可在测试中替换，用于控制崩溃窗口的时间判断。
	now func() time.Time
}

// newInstance 构造一个未启动的实例。
func newInstance(spec Found, logf func(string, ...any)) *Instance {
	if logf == nil {
		logf = defaultLogf
	}
	state := StateStopped
	if spec.Skipped != "" {
		state = StateSkipped
	}
	inst := &Instance{
		id:      spec.ID,
		spec:    spec,
		state:   state,
		lastErr: spec.Skipped,
		logs:    newRing(logLines),
		logf:    logf,
		now:     time.Now,
	}
	return inst
}

// ID 返回插件 ID。
func (i *Instance) ID() string { return i.id }

// Snapshot 是实例状态的一份只读快照，供接口层使用。
type Snapshot struct {
	ID           string   `json:"id"`
	State        State    `json:"state"`
	LastError    string   `json:"last_error,omitempty"`
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version,omitempty"`
	Description  string   `json:"description,omitempty"`
	Author       string   `json:"author,omitempty"`
	Capabilities []string `json:"capabilities"`
	Scopes       []string `json:"required_scopes"`
	Enabled      bool     `json:"enabled"`
	HasFrontend  bool     `json:"has_frontend"`
	FrontendURL  string   `json:"frontend_url,omitempty"`
}

// Snapshot 返回当前状态。
func (i *Instance) Snapshot() Snapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := Snapshot{
		ID:           i.id,
		State:        i.state,
		LastError:    i.lastErr,
		Enabled:      i.enabled,
		HasFrontend:  i.spec.HasFrontend,
		Capabilities: []string{},
		Scopes:       []string{},
	}
	if out.HasFrontend {
		out.FrontendURL = "/api/admin/plugins/" + i.id + "/frontend/"
	}
	if i.manifest != nil {
		out.Name = i.manifest.GetName()
		out.Version = i.manifest.GetVersion()
		out.Description = i.manifest.GetDescription()
		out.Author = i.manifest.GetAuthor()
		for _, c := range i.manifest.GetCapabilities() {
			out.Capabilities = append(out.Capabilities, capabilityName(c))
		}
		out.Scopes = append(out.Scopes, i.manifest.GetRequiredScopes()...)
	}
	return out
}

// Manifest 返回最近一次 Describe 的结果，未启动过时为 nil。
func (i *Instance) Manifest() *pb.Manifest {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest
}

// Logs 返回最近的日志行。
func (i *Instance) Logs() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.logs.lines()
}

// Has 报告插件是否声明了某项能力。
func (i *Instance) Has(c pb.Capability) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.state != StateRunning || i.manifest == nil {
		return false
	}
	for _, got := range i.manifest.GetCapabilities() {
		if got == c {
			return true
		}
	}
	return false
}

// client 返回可用的 gRPC 客户端，未运行时返回 nil。
func (i *Instance) client() (pb.PluginClient, *conn) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.state != StateRunning || i.conn == nil {
		return nil, nil
	}
	return i.conn.client, i.conn
}

// record 写一行实例日志，同时进主程序日志。
func (i *Instance) record(format string, args ...any) {
	line := sprintf(format, args...)
	i.mu.Lock()
	i.logs.push(i.now().UTC().Format(time.RFC3339) + " " + line)
	i.mu.Unlock()
	i.logf("[plugin:%s] %s", i.id, line)
}

// Start 启动插件并进入监控循环。
//
// 已在运行时是无操作。启动失败不返回错误而是记录状态并交给重启循环 ——
// 管理员点「启用」的意图是「保持它运行」，一次失败不该让这个意图丢失。
func (i *Instance) Start(ctx context.Context, env []string, values map[string]string) {
	i.mu.Lock()
	if i.spec.Skipped != "" {
		i.mu.Unlock()
		return
	}
	if i.enabled && i.state == StateRunning {
		i.mu.Unlock()
		return
	}
	i.enabled = true
	i.env = env
	i.values = values
	// 清掉崩溃记录：管理员显式启用等于「再试一次」。
	i.crashes = nil
	if i.cancel != nil {
		i.cancel()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	i.cancel = cancel
	i.mu.Unlock()

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.supervise(loopCtx)
	}()
}

// Stop 停用插件并结束进程。
func (i *Instance) Stop() {
	i.mu.Lock()
	i.enabled = false
	cancel := i.cancel
	i.cancel = nil
	i.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	i.wg.Wait()

	i.mu.Lock()
	c := i.conn
	i.conn = nil
	if i.state != StateSkipped {
		i.state = StateStopped
	}
	i.mu.Unlock()

	if c != nil {
		c.close(shutdownWait, termWait)
	}
}

// Reconfigure 下发新配置。
//
// 未运行时只记下值，等下次启动时随握手一起下发。
func (i *Instance) Reconfigure(ctx context.Context, values map[string]string) error {
	i.mu.Lock()
	i.values = values
	c := i.conn
	running := i.state == StateRunning
	i.mu.Unlock()

	if !running || c == nil {
		return nil
	}
	if err := c.configure(ctx, values); err != nil {
		i.setError(err)
		return err
	}
	i.record("配置已更新")
	return nil
}

// supervise 是「启动 → 监控 → 崩溃 → 退避重启」的主循环。
func (i *Instance) supervise(ctx context.Context) {
	delay := restartBaseDelay
	for {
		if ctx.Err() != nil {
			return
		}

		if err := i.attempt(ctx); err != nil {
			// context 被取消说明是正常停用，不算崩溃。
			if ctx.Err() != nil {
				return
			}
			i.setError(err)
			i.record("启动失败：%v", err)
		} else {
			// 启动成功，退避重置，进入健康检查直到进程出问题。
			delay = restartBaseDelay
			i.watch(ctx)
			if ctx.Err() != nil {
				return
			}
		}

		if i.tooManyCrashes() {
			i.mu.Lock()
			i.state = StateCrashed
			i.lastErr = sprintf("%s 内崩溃超过 %d 次，已停止重试；修复后请在后台重新启用", crashWindow, crashLimit)
			msg := i.lastErr
			i.mu.Unlock()
			i.record("%s", msg)
			return
		}

		i.record("将在 %s 后重启", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > restartMaxDelay {
			delay = restartMaxDelay
		}
	}
}

// attempt 拉起进程、握手、取 manifest 并下发配置。
func (i *Instance) attempt(ctx context.Context) error {
	i.mu.RLock()
	spec, env, values := i.spec, i.env, i.values
	i.mu.RUnlock()

	c, err := launch(ctx, spec, env, i.logf)
	if err != nil {
		return err
	}

	manifest, err := c.describe(ctx)
	if err != nil {
		c.close(shutdownWait, termWait)
		return err
	}
	if err := c.configure(ctx, values); err != nil {
		c.close(shutdownWait, termWait)
		return err
	}

	i.mu.Lock()
	i.conn = c
	i.manifest = manifest
	i.state = StateRunning
	i.lastErr = ""
	i.mu.Unlock()

	i.record("已启动：%s %s", manifest.GetName(), manifest.GetVersion())
	return nil
}

// watch 周期性健康检查，直到插件被判定死亡或 context 取消。
func (i *Instance) watch(ctx context.Context) {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()

	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		_, c := i.client()
		if c == nil {
			return
		}
		if err := c.health(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			fails++
			i.record("健康检查失败（%d/%d）：%v", fails, healthFailLimit, err)
			if fails >= healthFailLimit {
				i.noteCrash()
				i.mu.Lock()
				i.state = StateError
				i.lastErr = err.Error()
				dead := i.conn
				i.conn = nil
				i.mu.Unlock()
				if dead != nil {
					dead.close(shutdownWait, termWait)
				}
				return
			}
			continue
		}
		fails = 0
	}
}

// setError 记录一次错误状态。
func (i *Instance) setError(err error) {
	i.noteCrash()
	i.mu.Lock()
	i.state = StateError
	i.lastErr = err.Error()
	i.conn = nil
	i.mu.Unlock()
}

// noteCrash 记下一次崩溃时间。
func (i *Instance) noteCrash() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.crashes = append(i.crashes, i.now())
}

// tooManyCrashes 报告窗口内崩溃次数是否已超上限。
func (i *Instance) tooManyCrashes() bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	cutoff := i.now().Add(-crashWindow)
	kept := i.crashes[:0]
	for _, t := range i.crashes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	i.crashes = kept
	return len(kept) >= crashLimit
}

// capabilityName 把 enum 转成接口里用的短名。
//
// 不直接下发 protobuf 的全大写枚举名：那是线缆格式，前端要显示的是
// send_mail 这样的键，用它去查 i18n 文案。
func capabilityName(c pb.Capability) string {
	switch c {
	case pb.Capability_CAPABILITY_SEND_MAIL:
		return "send_mail"
	case pb.Capability_CAPABILITY_CREATE_PAYMENT:
		return "create_payment"
	}
	return "unknown"
}

// ring 是固定容量的字符串环形缓冲。
type ring struct {
	buf   []string
	next  int
	full  bool
	limit int
}

func newRing(limit int) *ring {
	return &ring{buf: make([]string, limit), limit: limit}
}

func (r *ring) push(line string) {
	r.buf[r.next] = line
	r.next = (r.next + 1) % r.limit
	if r.next == 0 {
		r.full = true
	}
}

// lines 按时间顺序返回缓冲中的行。
func (r *ring) lines() []string {
	if !r.full {
		out := make([]string, r.next)
		copy(out, r.buf[:r.next])
		return out
	}
	out := make([]string, 0, r.limit)
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}
