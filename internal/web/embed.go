// Package web 把前端构建产物嵌入二进制并提供 SPA 静态服务。
//
// dist 目录下的真实产物由构建流程注入（见根目录 Makefile 的 frontend 目标），
// 仓库中只提交一个 .gitkeep 占位文件。因此纯后端编译也能通过，只是访问首页
// 时会提示前端未构建。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// all: 前缀确保以下划线或点开头的文件（如 Vite 产物中的 _plugin-vue 等）
// 也一并嵌入，否则这些文件会被 embed 默认规则忽略。
//
//go:embed all:dist
var distFS embed.FS

// Assets 返回以 dist 为根的文件系统。
func Assets() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Available 报告前端产物是否已嵌入（即 index.html 是否存在）。
func Available() bool {
	assets, err := Assets()
	if err != nil {
		return false
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return false
	}
	return true
}

// notBuiltNotice 是未嵌入前端产物时返回的提示。
const notBuiltNotice = `Levis 后端已启动，但未找到前端产物。

请先构建前端并注入后端：

    make frontend    # 构建 ../levis-frontend 并拷入 internal/web/dist
    make build       # 重新编译二进制

后端 API 仍可正常使用（前缀 /api）。
`

// Handler 返回前端静态资源的处理器。
//
// 行为：命中真实文件则直接返回；否则回退到 index.html，以支持前端 history
// 模式下深层路由的直接访问与刷新。/api 前缀不会走到这里（路由已分流）。
func Handler() http.HandlerFunc {
	assets, err := Assets()
	if err != nil || !Available() {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(notBuiltNotice))
		}
	}

	fileServer := http.FileServer(http.FS(assets))
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			serveIndex(w, r, assets)
			return
		}

		info, statErr := fs.Stat(assets, name)
		if statErr != nil || info.IsDir() {
			// 未命中真实文件：交给前端路由处理。
			serveIndex(w, r, assets)
			return
		}

		// 带内容哈希的构建产物可长期缓存；index.html 不缓存，保证发版后
		// 客户端能立刻拿到新的资源引用。
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	}
}

// serveIndex 返回 index.html，并禁止缓存。
func serveIndex(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	raw, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "前端产物缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}
