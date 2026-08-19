package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// ErrNotFound 表示没有这个插件。
var ErrNotFound = errors.New("插件不存在")

// ErrUnavailable 表示插件未运行或未声明所需能力。
var ErrUnavailable = errors.New("插件当前不可用")

// ErrNotLoadable 表示插件未通过加载前检查（权限、缺文件等），无法启用。
//
// 单独一个哨兵值是为了让接口层能把它映射成 4xx：原因是管理员可以自己修好的
// （chmod go-w、补上可执行文件），报 500 会让人以为是程序坏了。具体原因用
// %w 包在外层的错误消息里。
var ErrNotLoadable = errors.New("插件不可加载")

// hookTimeout 是能力类调用的超时。
//
// 比生命周期调用宽松：发信要过 SMTP 握手，创建支付要过渠道 API，都可能到
// 秒级。但不能没有上限 —— 支付类调用是同步的，用户正在等页面响应。
const hookTimeout = 10 * time.Second

// ConfigProvider 提供某个插件的配置值。
//
// 定义成接口而不是直接依赖 service：本包不该知道配置存在数据库还是别处，
// 也避免 plugin → service → plugin 的循环引用。
type ConfigProvider interface {
	// PluginConfig 返回插件的配置键值对。
	PluginConfig(id string) (map[string]string, error)
	// PluginScopes 返回管理员授予该插件的权限位。
	//
	// 权限来自管理员的勾选，不来自 manifest 里的申请：manifest 是插件自己写的，
	// 照着它签发等于让插件改一行代码就能给自己扩权。首次启动时也只有这里有
	// 答案 —— 那时还没握手过，manifest 是空的。
	PluginScopes(id string) ([]string, error)
}

// KeyIssuer 为插件签发回调主程序用的凭证。
//
// 同样是接口：签发要写数据库，实现放在 service 层。
type KeyIssuer interface {
	// IssueKey 为插件签发一把 Key，返回明文。scopes 取自插件 manifest。
	IssueKey(id string, scopes []string) (string, error)
	// RevokeKeys 吊销插件的全部 Key，插件停止时调用。
	RevokeKeys(id string) error
}

// Manager 管理全部插件实例。
type Manager struct {
	dataDir string
	apiBase string

	config ConfigProvider
	keys   KeyIssuer
	logf   func(string, ...any)

	mu        sync.RWMutex
	instances map[string]*Instance
	// order 保存插件 ID 的展示顺序，与 Discover 的排序一致。
	order []string

	// ctx 是所有实例监控循环的父 context，Close 时取消。
	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager 构造 Manager。
//
// apiBase 是插件回调主程序的基址；config 与 keys 可为 nil，此时插件以空配置
// 启动且不签发回调凭证（未安装状态下就是这样）。
func NewManager(dataDir, apiBase string, config ConfigProvider, keys KeyIssuer, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = defaultLogf
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		dataDir:   dataDir,
		apiBase:   apiBase,
		config:    config,
		keys:      keys,
		logf:      logf,
		instances: make(map[string]*Instance),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Reload 重新扫描插件目录。
//
// 已在运行且仍存在于磁盘上的插件保持不动 —— 重扫的目的是发现新插件，不该
// 顺手把正在工作的插件重启一遍。磁盘上已消失的插件会被停掉。
func (m *Manager) Reload(ctx context.Context) error {
	found, err := Discover(m.dataDir)
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(found))
	order := make([]string, 0, len(found))
	for _, spec := range found {
		seen[spec.ID] = true
		order = append(order, spec.ID)

		m.mu.Lock()
		existing, ok := m.instances[spec.ID]
		if !ok {
			inst := newInstance(spec, m.logf)
			m.instances[spec.ID] = inst
			m.mu.Unlock()
			m.logf("发现插件 %s", spec.ID)
			if spec.Skipped != "" {
				m.logf("插件 %s 已跳过：%s", spec.ID, spec.Skipped)
				continue
			}
			// 新发现的插件默认不自动启动：它是刚被放进目录的陌生二进制，
			// 该由管理员看过之后再决定是否运行。
			continue
		}
		m.mu.Unlock()

		// 已存在的实例只更新磁盘信息（权限可能已修好）。
		existing.mu.Lock()
		wasSkipped := existing.spec.Skipped
		existing.spec = spec
		if spec.Skipped != "" {
			existing.state = StateSkipped
			existing.lastErr = spec.Skipped
		} else if wasSkipped != "" && existing.state == StateSkipped {
			existing.state = StateStopped
			existing.lastErr = ""
		}
		existing.mu.Unlock()
	}

	// 磁盘上已删除的插件：停掉并移除。
	m.mu.Lock()
	var gone []*Instance
	for id, inst := range m.instances {
		if !seen[id] {
			gone = append(gone, inst)
			delete(m.instances, id)
		}
	}
	m.order = order
	m.mu.Unlock()

	for _, inst := range gone {
		m.logf("插件 %s 已从磁盘移除，正在停止", inst.ID())
		inst.Stop()
		m.revoke(inst.ID())
	}
	return nil
}

// StartEnabled 启动所有此前被启用的插件。
//
// 由调用方在已安装后触发；enabled 状态存在数据库里，由 config 提供。
func (m *Manager) StartEnabled(ctx context.Context, enabled map[string]bool) {
	for _, inst := range m.List() {
		if !enabled[inst.ID()] {
			continue
		}
		if err := m.Enable(ctx, inst.ID()); err != nil {
			m.logf("启用插件 %s 失败：%v", inst.ID(), err)
		}
	}
}

// Enable 启用并启动一个插件。
func (m *Manager) Enable(ctx context.Context, id string) error {
	inst, err := m.get(id)
	if err != nil {
		return err
	}
	inst.mu.RLock()
	skipped := inst.spec.Skipped
	inst.mu.RUnlock()
	if skipped != "" {
		return fmt.Errorf("%w：%s", ErrNotLoadable, skipped)
	}

	values := map[string]string{}
	if m.config != nil {
		values, err = m.config.PluginConfig(id)
		if err != nil {
			return err
		}
	}

	env := []string{}
	if m.apiBase != "" {
		env = append(env, EnvAPIBase+"="+m.apiBase)
	}
	// 每次启动签发新 Key 并吊销旧的：插件重启后旧 Key 就该失效，
	// 免得进程都没了凭证还能用。
	//
	// 权限按管理员授予的来，不按 manifest 申请的来。一项都没授予时不签发凭证
	// —— 一个只管发信的插件根本不需要回调主程序，给它一把空权限的 Key 只是
	// 多一件可能泄露的东西。
	if m.keys != nil {
		m.revoke(id)
		var scopes []string
		if m.config != nil {
			if scopes, err = m.config.PluginScopes(id); err != nil {
				return err
			}
		}
		if len(scopes) > 0 {
			secret, err := m.keys.IssueKey(id, scopes)
			if err != nil {
				return err
			}
			env = append(env, EnvAPIKey+"="+secret)
		}
	}

	inst.Start(m.ctx, env, values)
	return nil
}

// Disable 停用一个插件。
func (m *Manager) Disable(id string) error {
	inst, err := m.get(id)
	if err != nil {
		return err
	}
	inst.Stop()
	m.revoke(id)
	return nil
}

// Reconfigure 下发新配置到运行中的插件。
func (m *Manager) Reconfigure(ctx context.Context, id string) error {
	inst, err := m.get(id)
	if err != nil {
		return err
	}
	values := map[string]string{}
	if m.config != nil {
		values, err = m.config.PluginConfig(id)
		if err != nil {
			return err
		}
	}
	return inst.Reconfigure(ctx, values)
}

// List 返回全部实例，按 ID 顺序。
func (m *Manager) List() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Instance, 0, len(m.instances))
	for _, id := range m.order {
		if inst, ok := m.instances[id]; ok {
			out = append(out, inst)
		}
	}
	// order 里没有的（理论上不该发生）兜底补上，保证不漏。
	if len(out) != len(m.instances) {
		seen := make(map[string]bool, len(out))
		for _, inst := range out {
			seen[inst.ID()] = true
		}
		extra := make([]*Instance, 0)
		for id, inst := range m.instances {
			if !seen[id] {
				extra = append(extra, inst)
			}
		}
		sort.Slice(extra, func(i, j int) bool { return extra[i].ID() < extra[j].ID() })
		out = append(out, extra...)
	}
	return out
}

// Get 返回指定插件的实例。
func (m *Manager) Get(id string) (*Instance, error) { return m.get(id) }

// get 是 Get 的内部形式。
func (m *Manager) get(id string) (*Instance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	if !ok {
		return nil, ErrNotFound
	}
	return inst, nil
}

// revoke 吊销插件的回调凭证，失败只记日志。
//
// 吊销失败不该阻断停用流程 —— 插件已经停了，Key 留着虽不理想但不致命，
// 而让「停用」这个操作报错会让管理员以为插件还在跑。
func (m *Manager) revoke(id string) {
	if m.keys == nil {
		return
	}
	if err := m.keys.RevokeKeys(id); err != nil {
		m.logf("吊销插件 %s 的回调凭证失败：%v", id, err)
	}
}

// find 返回声明了指定能力且正在运行的第一个插件。
//
// 同一能力只用一个插件：装了两个支付插件时该用哪个是管理员的选择，
// 由调用方按 ID 指定，不在这里猜。
func (m *Manager) find(c pb.Capability) *Instance {
	for _, inst := range m.List() {
		if inst.Has(c) {
			return inst
		}
	}
	return nil
}

// Mailer 返回当前可用的邮件插件 ID，没有则返回空串。
func (m *Manager) Mailer() string {
	if inst := m.find(pb.Capability_CAPABILITY_SEND_MAIL); inst != nil {
		return inst.ID()
	}
	return ""
}

// PaymentPlugins 返回全部可用的支付插件 ID。
func (m *Manager) PaymentPlugins() []string {
	out := []string{}
	for _, inst := range m.List() {
		if inst.Has(pb.Capability_CAPABILITY_CREATE_PAYMENT) {
			out = append(out, inst.ID())
		}
	}
	return out
}

// ProvisionPlugins 返回全部可用的上游产品对接插件 ID。
func (m *Manager) ProvisionPlugins() []string {
	out := []string{}
	for _, inst := range m.List() {
		if inst.Has(pb.Capability_CAPABILITY_PROVISION_PRODUCT) {
			out = append(out, inst.ID())
		}
	}
	return out
}

// SendMail 通过任一可用的邮件插件发信。
//
// 没有可用插件时返回 ErrUnavailable，由调用方决定如何处理 —— 通知类邮件
// 应当忽略这个错误，而不是让业务失败。
func (m *Manager) SendMail(ctx context.Context, req *pb.SendMailRequest) error {
	inst := m.find(pb.Capability_CAPABILITY_SEND_MAIL)
	if inst == nil {
		return ErrUnavailable
	}
	client, c := inst.client()
	if client == nil {
		return ErrUnavailable
	}
	return c.call(ctx, hookTimeout, func(ctx context.Context) error {
		_, err := client.SendMail(ctx, req)
		return err
	})
}

// CreatePayment 让指定插件创建一笔支付。
func (m *Manager) CreatePayment(ctx context.Context, id string, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	inst, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if !inst.Has(pb.Capability_CAPABILITY_CREATE_PAYMENT) {
		return nil, ErrUnavailable
	}
	client, c := inst.client()
	if client == nil {
		return nil, ErrUnavailable
	}
	var out *pb.CreatePaymentReply
	err = c.call(ctx, hookTimeout, func(ctx context.Context) error {
		reply, err := client.CreatePayment(ctx, req)
		if err != nil {
			return err
		}
		out = reply
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.GetError() != "" {
		return nil, fmt.Errorf("%s", out.GetError())
	}
	return out, nil
}

// QueryPayment 查询一笔支付在渠道侧的状态。
func (m *Manager) QueryPayment(ctx context.Context, id string, req *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	inst, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if !inst.Has(pb.Capability_CAPABILITY_CREATE_PAYMENT) {
		return nil, ErrUnavailable
	}
	client, c := inst.client()
	if client == nil {
		return nil, ErrUnavailable
	}
	var out *pb.QueryPaymentReply
	err = c.call(ctx, hookTimeout, func(ctx context.Context) error {
		reply, err := client.QueryPayment(ctx, req)
		if err != nil {
			return err
		}
		out = reply
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyPaymentCallback lets a payment plugin authenticate and normalize a raw gateway callback.
func (m *Manager) VerifyPaymentCallback(ctx context.Context, id string, req *pb.VerifyPaymentCallbackRequest) (*pb.VerifyPaymentCallbackReply, error) {
	inst, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if !inst.Has(pb.Capability_CAPABILITY_CREATE_PAYMENT) {
		return nil, ErrUnavailable
	}
	client, c := inst.client()
	if client == nil {
		return nil, ErrUnavailable
	}
	var out *pb.VerifyPaymentCallbackReply
	err = c.call(ctx, hookTimeout, func(ctx context.Context) error {
		reply, err := client.VerifyPaymentCallback(ctx, req)
		if err != nil {
			return err
		}
		out = reply
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Close 停止全部插件。主程序退出时调用。
func (m *Manager) Close() {
	m.cancel()
	for _, inst := range m.List() {
		inst.Stop()
		m.revoke(inst.ID())
	}
}

// sprintf 是 fmt.Sprintf 的短名，实例日志里用得频繁。
func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
