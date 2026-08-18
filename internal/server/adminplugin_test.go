package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/pluginhost"
	levisrt "github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// 本文件覆盖 /api/admin/plugins 这一组接口。
//
// 与 plugin_test.go 的分工：那边测插件回调主程序的方向（/api/plugin/v1），
// 这边测管理员操作插件的方向，并且真的把假插件进程拉起来 —— 「点启用之后进程
// 确实在跑」这件事只有起进程才能验。

// fakePluginID 是假插件装进数据目录后的 ID。
const fakePluginID = "fake"

// buildFakePlugin 把 internal/plugin/testdata 下的假插件编译到临时位置。
//
// 整个测试二进制只编一次：每个用例各编一遍会让本包慢上数十秒。
var buildFakePlugin = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "levis-server-fake-plugin-")
	if err != nil {
		return "", err
	}
	name := "plugin"
	if runtime.GOOS == "windows" {
		name = "plugin.exe"
	}
	out := filepath.Join(dir, name)
	// 相对包路径：测试的工作目录是本包所在目录。
	cmd := exec.Command("go", "build", "-o", out, "../plugin/testdata/fakeplugin")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := cmd.CombinedOutput(); err != nil {
		return "", &fakeBuildError{output: string(raw), err: err}
	}
	return out, nil
})

type fakeBuildError struct {
	output string
	err    error
}

func (e *fakeBuildError) Error() string { return e.err.Error() + "\n" + e.output }

// adminPluginEnv 是插件管理接口测试的公共环境。
type adminPluginEnv struct {
	rt      *levisrt.Runtime
	handler http.Handler
	admin   []*http.Cookie
	plugins *plugin.Manager
}

// installFakePlugin 把假插件装进 dataDir 下的插件目录。
func installFakePlugin(t *testing.T, dataDir, id string) {
	t.Helper()
	binary, err := buildFakePlugin()
	if err != nil {
		t.Fatalf("编译假插件失败: %v", err)
	}
	dir := filepath.Join(plugin.Root(dataDir), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("读取假插件失败: %v", err)
	}
	name := "plugin"
	if runtime.GOOS == "windows" {
		name = "plugin.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o700); err != nil {
		t.Fatalf("写入插件文件失败: %v", err)
	}
}

// newAdminPluginEnv 起一个带真插件管理器的已安装服务，并装好假插件。
//
// 插件目录在 New 之前就填好，但不预先 Reload —— 让用例自己调「重新扫描」，
// 这样那个接口本身也在被测范围内。
func newAdminPluginEnv(t *testing.T) adminPluginEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上插件文件名与权限模型不同，跳过")
	}

	rt := levisrt.New(t.TempDir())
	installFakePlugin(t, rt.DataDir(), fakePluginID)

	plugins := pluginhost.New(rt, "http://127.0.0.1:1/api/plugin/v1", func(format string, args ...any) {
		t.Logf(format, args...)
	})
	// 无论用例怎么结束都要收掉子进程，否则残留进程会跟着 CI 一直跑。
	t.Cleanup(plugins.Close)

	handler, closeHandler := New(rt, plugins, false)
	t.Cleanup(closeHandler)
	installVia(t, rt, handler)
	admin := loginAs(t, handler, "admin", "password123")
	return adminPluginEnv{rt: rt, handler: handler, admin: admin, plugins: plugins}
}

// pluginList 是列表接口的响应体。
type pluginList struct {
	Items  []service.PluginDetail `json:"items"`
	Scopes []string               `json:"scopes"`
}

// reloadPlugins 调「重新扫描」并返回结果列表。
func reloadPlugins(t *testing.T, env adminPluginEnv) pluginList {
	t.Helper()
	rec := doAs(t, env.handler, http.MethodPost, "/api/admin/plugins/reload", nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("重新扫描失败: %d %s", rec.Code, rec.Body.String())
	}
	var out pluginList
	decodeJSON(t, rec, &out)
	return out
}

// pluginDetail 读一个插件的详情。
func pluginDetail(t *testing.T, env adminPluginEnv, id string) service.PluginDetail {
	t.Helper()
	rec := doAs(t, env.handler, http.MethodGet, "/api/admin/plugins/"+id, nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取插件详情失败: %d %s", rec.Code, rec.Body.String())
	}
	var out service.PluginDetail
	decodeJSON(t, rec, &out)
	return out
}

// waitPluginState 等插件进入期望状态。
//
// 启用是异步的：接口返回时进程刚被拉起，握手与 Describe 还在路上。
func waitPluginState(t *testing.T, env adminPluginEnv, id string, want plugin.State) service.PluginDetail {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last service.PluginDetail
	for time.Now().Before(deadline) {
		last = pluginDetail(t, env, id)
		if last.State == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("插件 %s 未在超时内进入 %s，当前 %s（%s）", id, want, last.State, last.LastError)
	return last
}

// fieldByKey 从详情里取一个配置字段。
func fieldByKey(t *testing.T, detail service.PluginDetail, key string) service.ConfigField {
	t.Helper()
	for _, field := range detail.Config {
		if field.Key == key {
			return field
		}
	}
	t.Fatalf("详情里没有配置字段 %q", key)
	return service.ConfigField{}
}

// TestAdminPluginsRequiresAdmin 确认这组接口不对普通用户开放。
//
// 插件管理等于「在服务器上运行任意程序」，权限边界比其它管理接口更要紧。
func TestAdminPluginsRequiresAdmin(t *testing.T) {
	rt, handler, _, users := installedWithUsers(t, "alice")
	_ = rt

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/plugins"},
		{http.MethodPost, "/api/admin/plugins/reload"},
		{http.MethodGet, "/api/admin/plugins/fake"},
		{http.MethodPut, "/api/admin/plugins/fake/config"},
		{http.MethodPost, "/api/admin/plugins/fake/enable"},
		{http.MethodPost, "/api/admin/plugins/fake/disable"},
		{http.MethodGet, "/api/admin/plugins/fake/logs"},
	}
	for _, c := range cases {
		rec := doAs(t, handler, c.method, c.path, map[string]any{}, users["alice"])
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s 普通用户应返回 403，实际 %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
		// 未登录同样不行。
		rec = do(t, handler, c.method, c.path, map[string]any{})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录应返回 401，实际 %d", c.method, c.path, rec.Code)
		}
	}
}

// TestAdminPluginsUnavailableWithoutManager 确认没有插件管理器时如实回 503。
//
// 本包多数用例传 nil Manager，这些接口必须给出明确的「未启用」而不是 panic。
func TestAdminPluginsUnavailableWithoutManager(t *testing.T) {
	_, handler := installedServer(t)
	admin := loginAs(t, handler, "admin", "password123")

	rec := doAs(t, handler, http.MethodGet, "/api/admin/plugins", nil, admin)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无插件管理器时应返回 503，实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != "PLUGIN_UNAVAILABLE" {
		t.Errorf("错误码应为 PLUGIN_UNAVAILABLE，实际 %q", body.Code)
	}
}

// TestAdminPluginReloadFindsPlugin 确认重新扫描能发现磁盘上的插件，
// 且新发现的插件不会自动启动。
//
// 不自动启动是有意的：那是刚被放进目录的陌生二进制，该由管理员看过再决定。
func TestAdminPluginReloadFindsPlugin(t *testing.T) {
	env := newAdminPluginEnv(t)

	list := reloadPlugins(t, env)
	if len(list.Items) != 1 {
		t.Fatalf("应发现 1 个插件，实际 %d 个", len(list.Items))
	}
	item := list.Items[0]
	if item.ID != fakePluginID {
		t.Errorf("插件 ID 应为 %q，实际 %q", fakePluginID, item.ID)
	}
	if item.State != plugin.StateStopped {
		t.Errorf("新发现的插件应为 stopped，实际 %s", item.State)
	}
	if item.Enabled {
		t.Error("新发现的插件不应自动启用")
	}
	// 可勾选的权限位要一并给出，否则前端没法渲染授权表单。
	if len(list.Scopes) != len(model.AllPluginScopes()) {
		t.Errorf("应返回全部可授予权限位，实际 %v", list.Scopes)
	}

	// 未知插件返回 404。
	rec := doAs(t, env.handler, http.MethodGet, "/api/admin/plugins/nope", nil, env.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知插件应返回 404，实际 %d", rec.Code)
	}
}

// TestAdminPluginEnableThenDisable 走一遍启用、停用，确认进程与库里的意图同步。
func TestAdminPluginEnableThenDisable(t *testing.T) {
	env := newAdminPluginEnv(t)
	reloadPlugins(t, env)

	rec := doAs(t, env.handler, http.MethodPost, "/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用失败: %d %s", rec.Code, rec.Body.String())
	}
	detail := waitPluginState(t, env, fakePluginID, plugin.StateRunning)

	// 握手成功后 manifest 里的东西才有值，用它确认 Describe 真跑过了。
	if detail.Name != "假插件" {
		t.Errorf("应取到 manifest 里的展示名，实际 %q", detail.Name)
	}
	if len(detail.Capabilities) != 2 {
		t.Errorf("应声明 2 项能力，实际 %v", detail.Capabilities)
	}
	// 必填项还没填，此时应报「未配置」。
	if detail.Configured {
		t.Error("必填项未填时 configured 应为 false")
	}

	// 启用意图必须落库，否则主程序重启后插件会悄悄不见。
	if enabled := enabledInDB(t, env); !enabled[fakePluginID] {
		t.Error("启用后库里应记下意图")
	}

	// 日志接口应能看到启动那一行。
	rec = doAs(t, env.handler, http.MethodGet, "/api/admin/plugins/"+fakePluginID+"/logs", nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取日志失败: %d %s", rec.Code, rec.Body.String())
	}
	var logs struct {
		Lines []string `json:"lines"`
	}
	decodeJSON(t, rec, &logs)
	if !strings.Contains(strings.Join(logs.Lines, "\n"), "已启动") {
		t.Errorf("日志里应有启动记录，实际 %v", logs.Lines)
	}

	rec = doAs(t, env.handler, http.MethodPost, "/api/admin/plugins/"+fakePluginID+"/disable", nil, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("停用失败: %d %s", rec.Code, rec.Body.String())
	}
	var stopped service.PluginDetail
	decodeJSON(t, rec, &stopped)
	if stopped.State != plugin.StateStopped {
		t.Errorf("停用后应为 stopped，实际 %s", stopped.State)
	}
	if stopped.Enabled {
		t.Error("停用后库里的意图应为 false")
	}
	if enabled := enabledInDB(t, env); enabled[fakePluginID] {
		t.Error("停用后库里不该还记着启用")
	}
}

// enabledInDB 读库里记着的启用意图。
func enabledInDB(t *testing.T, env adminPluginEnv) map[string]bool {
	t.Helper()
	out, err := pluginhost.EnabledPlugins(env.rt.DB())
	if err != nil {
		t.Fatalf("读取启用状态失败: %v", err)
	}
	return out
}

// TestAdminPluginSecretConfigNeverReturned 是配置接口最要紧的一条：
// secret 字段的值永不回传，且提交空值表示「不修改」。
//
// 读接口不回传值，前端就拿不到原值可回填；若空值被当成清空，管理员改一次
// 别的字段就会把 SMTP 密码抹掉。
func TestAdminPluginSecretConfigNeverReturned(t *testing.T) {
	env := newAdminPluginEnv(t)
	reloadPlugins(t, env)
	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("启用失败: %d %s", rec.Code, rec.Body.String())
	}
	waitPluginState(t, env, fakePluginID, plugin.StateRunning)

	const secretValue = "super-secret-smtp-password"
	path := "/api/admin/plugins/" + fakePluginID + "/config"
	rec := doAs(t, env.handler, http.MethodPut, path, map[string]any{
		"values": map[string]string{"host": "smtp.example.com", "password": secretValue},
	}, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存配置失败: %d %s", rec.Code, rec.Body.String())
	}
	// 保存的响应体里也不能有明文。
	if strings.Contains(rec.Body.String(), secretValue) {
		t.Errorf("保存响应不应回传 secret 明文，实际 %s", rec.Body.String())
	}

	detail := pluginDetail(t, env, fakePluginID)
	if !detail.Configured {
		t.Error("必填项已填，configured 应为 true")
	}
	host := fieldByKey(t, detail, "host")
	if host.Value != "smtp.example.com" {
		t.Errorf("非 secret 字段应回传当前值，实际 %q", host.Value)
	}
	password := fieldByKey(t, detail, "password")
	if password.Value != "" {
		t.Errorf("secret 字段不应回传值，实际 %q", password.Value)
	}
	if !password.HasValue {
		t.Error("secret 字段已存过值，has_value 应为 true")
	}

	// 提交空 secret：只改 host，密码必须留着。
	rec = doAs(t, env.handler, http.MethodPut, path, map[string]any{
		"values": map[string]string{"host": "smtp2.example.com", "password": ""},
	}, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("二次保存失败: %d %s", rec.Code, rec.Body.String())
	}

	values, err := service.NewPluginService(env.rt.DB()).PluginConfig(fakePluginID)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if values["password"] != secretValue {
		t.Errorf("提交空 secret 不应覆盖原值，实际 %q", values["password"])
	}
	if values["host"] != "smtp2.example.com" {
		t.Errorf("非 secret 字段应被更新，实际 %q", values["host"])
	}

	// manifest 里没声明的键一律忽略，不该在库里攒下无人认领的配置。
	rec = doAs(t, env.handler, http.MethodPut, path, map[string]any{
		"values": map[string]string{"bogus": "x"},
	}, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存未知键应被忽略而非报错，实际 %d %s", rec.Code, rec.Body.String())
	}
	values, err = service.NewPluginService(env.rt.DB()).PluginConfig(fakePluginID)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if _, ok := values["bogus"]; ok {
		t.Error("未声明的配置键不该落库")
	}
}

// TestAdminPluginScopesAreGrantedNotDeclared 确认授权按管理员的勾选走，
// 不按 manifest 里的申请走。
//
// manifest 是插件自己写的，照着它签发凭证等于让插件改一行代码就能自我扩权。
func TestAdminPluginScopesAreGrantedNotDeclared(t *testing.T) {
	env := newAdminPluginEnv(t)
	reloadPlugins(t, env)
	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("启用失败: %d %s", rec.Code, rec.Body.String())
	}
	detail := waitPluginState(t, env, fakePluginID, plugin.StateRunning)

	// 假插件申请了 wallet:credit 与 user:read，但管理员一项都没勾。
	if len(detail.Scopes) != 2 {
		t.Errorf("manifest 应申请 2 项权限，实际 %v", detail.Scopes)
	}
	if len(detail.Granted) != 0 {
		t.Errorf("管理员未勾选时不应有已授予权限，实际 %v", detail.Granted)
	}
	// 一项都没授予时不签发凭证 —— 只管发信的插件不需要回调主程序。
	if got := activeKeyCount(t, env); got != 0 {
		t.Errorf("未授权时不应签发凭证，实际 %d 把", got)
	}

	path := "/api/admin/plugins/" + fakePluginID + "/config"
	rec := doAs(t, env.handler, http.MethodPut, path, map[string]any{
		"values": map[string]string{"host": "smtp.example.com"},
		"scopes": []string{model.PluginScopeUserRead},
	}, env.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存授权失败: %d %s", rec.Code, rec.Body.String())
	}
	var saved service.PluginDetail
	decodeJSON(t, rec, &saved)
	if len(saved.Granted) != 1 || saved.Granted[0] != model.PluginScopeUserRead {
		t.Errorf("应只授予 user:read，实际 %v", saved.Granted)
	}

	// 不认识的权限位要被拒，否则库里会存下永远不生效的字符串。
	rec = doAs(t, env.handler, http.MethodPut, path, map[string]any{
		"scopes": []string{"wallet:drain"},
	}, env.admin)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知权限位应返回 400，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 重新启用时按新授权签发凭证，且只有被授予的那一项。
	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/disable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("停用失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("重新启用失败: %d %s", rec.Code, rec.Body.String())
	}
	waitPluginState(t, env, fakePluginID, plugin.StateRunning)

	var key model.PluginKey
	if err := env.rt.DB().Where("plugin_id = ? AND status = ?", fakePluginID, model.APIKeyActive).
		First(&key).Error; err != nil {
		t.Fatalf("应签发一把凭证: %v", err)
	}
	if len(key.Scopes) != 1 || key.Scopes[0] != model.PluginScopeUserRead {
		t.Errorf("凭证权限应只有 user:read，实际 %v", key.Scopes)
	}
}

// activeKeyCount 数当前有效的插件凭证。
func activeKeyCount(t *testing.T, env adminPluginEnv) int64 {
	t.Helper()
	var count int64
	err := env.rt.DB().Model(&model.PluginKey{}).
		Where("plugin_id = ? AND status = ?", fakePluginID, model.APIKeyActive).
		Count(&count).Error
	if err != nil {
		t.Fatalf("统计凭证失败: %v", err)
	}
	return count
}

// TestAdminPluginDisableRevokesKey 确认停用插件会立刻吊销它的回调凭证。
//
// 不吊销的话，一个已被管理员停用的插件仍能拿着旧凭证继续加钱。
func TestAdminPluginDisableRevokesKey(t *testing.T) {
	env := newAdminPluginEnv(t)
	reloadPlugins(t, env)

	// 先授权再启用，这样启用时会签发凭证。
	if rec := doAs(t, env.handler, http.MethodPut, "/api/admin/plugins/"+fakePluginID+"/config",
		map[string]any{"scopes": model.AllPluginScopes()}, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("授权失败: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("启用失败: %d %s", rec.Code, rec.Body.String())
	}
	waitPluginState(t, env, fakePluginID, plugin.StateRunning)
	if got := activeKeyCount(t, env); got != 1 {
		t.Fatalf("启用后应有 1 把有效凭证，实际 %d", got)
	}

	if rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/disable", nil, env.admin); rec.Code != http.StatusOK {
		t.Fatalf("停用失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := activeKeyCount(t, env); got != 0 {
		t.Errorf("停用后凭证应全部失效，实际仍有 %d 把", got)
	}
}

// TestAdminPluginSkipsWorldWritable 确认他人可写的插件被拒绝加载，
// 且原因能在管理界面看到。
//
// 插件以主程序的身份运行，「别人能改写这个文件」等于「别人能以主程序的权限
// 执行任意代码」。
func TestAdminPluginSkipsWorldWritable(t *testing.T) {
	env := newAdminPluginEnv(t)

	exe := filepath.Join(plugin.Root(env.rt.DataDir()), fakePluginID, "plugin")
	// WriteFile 的 mode 会被 umask 削掉，必须显式再设一次才真的他人可写。
	if err := os.Chmod(exe, 0o777); err != nil {
		t.Fatalf("放宽权限失败: %v", err)
	}

	list := reloadPlugins(t, env)
	if len(list.Items) != 1 {
		t.Fatalf("被跳过的插件仍应出现在列表里，实际 %d 个", len(list.Items))
	}
	item := list.Items[0]
	if item.State != plugin.StateSkipped {
		t.Errorf("他人可写的插件应为 skipped，实际 %s", item.State)
	}
	if !strings.Contains(item.LastError, "chmod go-w") {
		t.Errorf("应给出可操作的原因，实际 %q", item.LastError)
	}

	// 不可加载的插件不能被启用。
	rec := doAs(t, env.handler, http.MethodPost,
		"/api/admin/plugins/"+fakePluginID+"/enable", nil, env.admin)
	if rec.Code == http.StatusOK {
		t.Errorf("不可加载的插件不该启用成功，响应 %s", rec.Body.String())
	}
	// 启用失败时意图要回退，否则界面会显示「已启用」而进程并不在跑。
	if enabled := enabledInDB(t, env); enabled[fakePluginID] {
		t.Error("启用失败后库里的意图应回退")
	}
}
