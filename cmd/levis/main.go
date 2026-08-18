// Command levis 启动 Levis 云服务管理程序。
//
// 前端产物已嵌入本二进制，运行后直接访问监听地址即可使用；首次运行会引导
// 进入安装流程。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/database"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/pluginhost"
	"github.com/SakuraOpenSource/levis/internal/runtime"
	"github.com/SakuraOpenSource/levis/internal/server"
	"github.com/SakuraOpenSource/levis/internal/web"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	var (
		dataDir     = flag.String("data", "data", "数据目录，存放 config.json 与 SQLite 文件")
		listen      = flag.String("listen", "", "监听地址，覆盖配置文件中的设置")
		debug       = flag.Bool("debug", false, "开启调试模式")
		showVersion = flag.Bool("version", false, "打印版本号后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("levis %s\n", version)
		return
	}

	if err := run(*dataDir, *listen, *debug); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func run(dataDir, listenOverride string, debug bool) error {
	rt := runtime.New(dataDir)
	addr := config.DefaultListen

	// config.json 存在即视为已安装，此时直接连库并激活运行时；
	// 不存在则以未安装态启动，等用户走完 /install 再激活。
	if config.Exists(dataDir) {
		cfg, err := config.Load(dataDir)
		if err != nil {
			return err
		}
		db, err := database.Open(cfg.Database)
		if err != nil {
			return err
		}
		if err := database.Migrate(db); err != nil {
			return err
		}
		rt.Activate(cfg, db)
		addr = cfg.Listen
		log.Printf("已加载配置，数据库类型: %s", cfg.Database.Driver)
	} else {
		log.Printf("未检测到配置文件，请在浏览器中完成安装")
	}

	if listenOverride != "" {
		addr = listenOverride
	}
	if !web.Available() {
		log.Printf("警告: 未嵌入前端产物，仅 API 可用（执行 make build 可构建完整程序）")
	}

	// 插件目录必须在此刻就建出来：管理员要先看到 data/plugins 才知道该往哪里
	// 放插件，不能等到第一次点「重新扫描」时才凭空出现。
	if err := plugin.EnsureRoot(dataDir); err != nil {
		return err
	}
	plugins := pluginhost.New(rt, apiBase(addr), func(format string, args ...any) {
		log.Printf(format, args...)
	})
	// 用 defer 而不是在信号分支里显式调用：defer 在 return 求值之后才跑，
	// 因此插件必然是在 srv.Shutdown 收完在途请求之后才被停掉 —— 反过来的话，
	// 一个正在等支付链接的请求会突然发现插件没了。ListenAndServe 直接报错
	// 退出的那条路径同样会走到这里，不至于漏掉收尾。
	defer plugins.Close()

	// 未安装时不碰插件：启用状态与配置都在库里，此刻无库可读。安装流程走完后
	// 由 /api/admin/plugins/reload 手动加载 —— 刚装好的站点还没有插件可跑。
	if rt.Installed() {
		startPlugins(rt, plugins)
	}

	handler, closeHandler := server.New(rt, plugins, debug)
	// 与 plugins.Close 同理，defer 保证它在 srv.Shutdown 之后才跑。顺序上先停
	// 通知队列再停插件：队列里的信要靠插件发出去，反过来会让最后几封信必然失败。
	defer closeHandler()

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 监听终止信号，给在途请求留出收尾时间。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("levis %s 正在监听 %s", version, addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Printf("收到终止信号，正在关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// startPlugins 清理残留凭证并拉起此前启用的插件。
//
// 任何一步失败都只记日志：插件是可选组件，一个装坏了的插件不该让整个站点起
// 不来。管理员能在插件页看到状态与原因。
func startPlugins(rt *runtime.Runtime, plugins *plugin.Manager) {
	// 先清残留凭证再拉插件。上次若是被 kill -9 收走，库里还留着已经没有对应
	// 进程的 Key；先清一遍能保证有效凭证不早于本次进程，也避免刚签发的新 Key
	// 被这次清理顺手带走。
	if err := pluginhost.RevokeStaleKeys(rt.DB()); err != nil {
		log.Printf("清理插件回调凭证失败: %v", err)
	}
	if err := plugins.Reload(context.Background()); err != nil {
		log.Printf("扫描插件目录失败: %v", err)
		return
	}
	enabled, err := pluginhost.EnabledPlugins(rt.DB())
	if err != nil {
		log.Printf("读取插件启用状态失败: %v", err)
		return
	}
	plugins.StartEnabled(context.Background(), enabled)
}

// apiBase 拼出插件回调主程序的基址。
//
// addr 形如 ":8080" 或 "127.0.0.1:8080"。插件与主程序同机，所以一律回落到
// 127.0.0.1：监听地址里的 0.0.0.0 或空主机名不是一个能拨号的目标，而绕外网
// 地址转一圈只是把本机流量推出去再回来。
func apiBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 解析不了就不猜，让插件在没有基址的情况下运行（不签发凭证）。
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/plugin/v1"
}
