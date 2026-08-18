// Package pluginhost 把插件管理器接到运行时与数据库上。
//
// 单独成包是为了让依赖方向保持单一：plugin 包只认两个窄接口
// （ConfigProvider / KeyIssuer），不知道数据库的存在；service 不知道插件进程
// 的存在。两边在这里汇合。
package pluginhost

import (
	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// provider 按请求现取数据库句柄，实现 plugin.ConfigProvider 与 plugin.KeyIssuer。
//
// 必须惰性取 db：安装是在进程运行期间完成的，启动时 rt.DB() 还是 nil，
// 把它存下来会让安装后的插件永远读不到配置。
type provider struct {
	rt *runtime.Runtime
}

// PluginConfig 返回插件配置，未安装时返回空配置。
func (p provider) PluginConfig(id string) (map[string]string, error) {
	db := p.rt.DB()
	if db == nil {
		return map[string]string{}, nil
	}
	return service.NewPluginService(db).PluginConfig(id)
}

// PluginScopes 返回管理员授予插件的权限位，未安装时为空。
func (p provider) PluginScopes(id string) ([]string, error) {
	db := p.rt.DB()
	if db == nil {
		return nil, nil
	}
	return service.NewPluginService(db).PluginScopes(id)
}

// IssueKey 签发插件回调凭证。
func (p provider) IssueKey(id string, scopes []string) (string, error) {
	db := p.rt.DB()
	if db == nil {
		return "", plugin.ErrUnavailable
	}
	return service.NewPluginKeyService(db).IssueKey(id, scopes)
}

// RevokeKeys 吊销插件的全部凭证。
func (p provider) RevokeKeys(id string) error {
	db := p.rt.DB()
	if db == nil {
		return nil
	}
	return service.NewPluginKeyService(db).RevokeKeys(id)
}

// New 构造插件管理器。apiBase 是插件回调主程序的基址。
func New(rt *runtime.Runtime, apiBase string, logf func(string, ...any)) *plugin.Manager {
	p := provider{rt: rt}
	return plugin.NewManager(rt.DataDir(), apiBase, p, p, logf)
}

// RevokeStaleKeys 吊销库里残留的全部插件凭证。
//
// 上次进程若是被 kill -9 收走，来不及吊销的凭证仍然留在库里，而那些插件进程
// 早已不在。启动时统一清一遍，保证有效凭证不早于本次进程。
func RevokeStaleKeys(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	return service.NewPluginKeyService(db).RevokeAll()
}

// EnabledPlugins 返回管理员此前启用的插件。
func EnabledPlugins(db *gorm.DB) (map[string]bool, error) {
	if db == nil {
		return nil, nil
	}
	return service.NewPluginService(db).EnabledPlugins()
}
