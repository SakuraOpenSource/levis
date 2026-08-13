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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/database"
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

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(rt, debug),
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
