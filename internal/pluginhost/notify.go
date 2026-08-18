package pluginhost

import (
	"github.com/SakuraOpenSource/levis/internal/notify"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/service"
)

// resolver 按需从数据库取发信要用的信息，实现 notify.Resolver。
//
// 与 provider 同样惰性取 db：通知投递发生在 worker 线程里，而安装是在进程运行
// 期间完成的，把句柄存下来会让安装前构造的 Notifier 永远读不到库。
type resolver struct {
	rt *runtime.Runtime
}

// Recipient 按用户 ID 取收件人。
func (r resolver) Recipient(userID uint) (notify.Recipient, error) {
	db := r.rt.DB()
	if db == nil {
		return notify.Recipient{}, plugin.ErrUnavailable
	}
	user, err := service.NewPluginCallbackService(db).User(userID)
	if err != nil {
		return notify.Recipient{}, err
	}
	return notify.Recipient{Name: user.Username, Email: user.Email}, nil
}

// SiteName 返回站点名称，读不到时回落到 "Levis"。
func (r resolver) SiteName() string {
	db := r.rt.DB()
	if db == nil {
		return "Levis"
	}
	return service.SiteName(db)
}

// NewNotifier 构造通知投递器。
//
// plugins 为 nil 时返回 nil —— 没有插件系统就没有发信能力，而 nil 的 *Notifier
// 上调用任何方法都是空操作，调用点不必判空。
func NewNotifier(rt *runtime.Runtime, plugins *plugin.Manager, logf func(string, ...any)) *notify.Notifier {
	if plugins == nil {
		return nil
	}
	return notify.New(plugins, resolver{rt: rt}, logf)
}
