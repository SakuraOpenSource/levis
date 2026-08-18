// Package plugin 负责发现、启动并调用外部插件进程。
//
// 插件是独立的可执行程序，放在 <dataDir>/plugins/<id>/plugin 下，与主程序
// 通过 gRPC 通信（契约见 proto/plugin.proto）。之所以不用 Go 的 plugin 包做
// 动态库加载：那要求 CGO，而本项目的交付方式是 CGO_ENABLED=0 交叉编译出的
// 单文件二进制；动态库还要求插件与主程序的 Go 版本及全部共同依赖版本严格
// 一致，对第三方作者不现实。
//
// 本包不依赖 service 与 handler，避免形成循环引用：需要读写数据库的能力由
// 插件通过 HTTP 回调 /api/plugin/v1 获得，而不是由本包直连数据库。
package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// DirName 是插件根目录在数据目录下的名字。
const DirName = "plugins"

// execName 是插件可执行文件的固定名字。
//
// 固定下来而不是「目录里唯一的可执行文件」：插件目录里可能还有配置样例、
// 证书、README，靠猜会选错，而选错的后果是执行了不该执行的东西。
func execName() string {
	if runtime.GOOS == "windows" {
		return "plugin.exe"
	}
	return "plugin"
}

// maxIDLen 是插件 ID 的长度上限。ID 会进数据库的设置键名，留出余量。
const maxIDLen = 48

// Found 是一个在磁盘上发现的插件。
type Found struct {
	// ID 是插件目录名，同时作为设置键名的一部分。
	ID string
	// Path 是可执行文件的绝对路径。
	Path string
	// DataDir 是插件的私有可写目录，已创建。
	DataDir string
	// HasFrontend 表示插件带有可供管理员打开的前端入口。
	HasFrontend bool
	// Skipped 非空表示该插件不可加载，值为面向管理员的原因说明。
	//
	// 不可加载的插件仍然返回而不是默默丢掉：管理员把文件放进去了却看不到
	// 任何反馈，只会以为是程序坏了。
	Skipped string
}

// Root 返回插件根目录。
func Root(dataDir string) string {
	return filepath.Join(dataDir, DirName)
}

// RootName 返回某个插件目录在插件根目录下的名字。
func RootName(id string) string { return filepath.Join(DirName, id) }

// EnsureRoot 创建插件根目录。
//
// 权限与 data/uploads 同口径 0700：插件目录里放的是会被执行的二进制，
// 比数据文件更需要收紧。
func EnsureRoot(dataDir string) error {
	if err := os.MkdirAll(Root(dataDir), 0o700); err != nil {
		return fmt.Errorf("创建插件目录失败: %w", err)
	}
	if err := os.Chmod(Root(dataDir), 0o700); err != nil {
		return fmt.Errorf("设置插件目录权限失败: %w", err)
	}
	return nil
}

// Discover 扫描插件根目录，返回其中的插件。
//
// 目录不存在时返回空列表而不是错误 —— 没装插件是正常状态。
func Discover(dataDir string) ([]Found, error) {
	root := Root(dataDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取插件目录失败: %w", err)
	}

	out := make([]Found, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			// 根目录下的散装文件一律忽略：约定是一个插件一个目录，
			// 这里多半是用户误放的压缩包或说明文件。
			continue
		}
		id := entry.Name()
		if err := ValidID(id); err != nil {
			out = append(out, Found{ID: id, Skipped: err.Error()})
			continue
		}
		out = append(out, inspect(root, id))
	}

	// 按 ID 排序，保证管理界面的顺序稳定 —— ReadDir 的顺序依赖文件系统。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// inspect 检查单个插件目录是否可加载。
func inspect(root, id string) Found {
	found := Found{ID: id}
	dir := filepath.Join(root, id)
	exe := filepath.Join(dir, execName())

	dirInfo, err := os.Stat(dir)
	if err != nil {
		found.Skipped = "无法读取插件目录"
		return found
	}
	// 目录可被他人写入时，别人就能把 plugin 换成任意程序。
	if reason := checkWritable(dirInfo.Mode(), "插件目录"); reason != "" {
		found.Skipped = reason
		return found
	}

	info, err := os.Stat(exe)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			found.Skipped = fmt.Sprintf("目录中缺少可执行文件 %s", execName())
			return found
		}
		found.Skipped = "无法读取插件文件"
		return found
	}
	if !info.Mode().IsRegular() {
		found.Skipped = "插件文件不是常规文件"
		return found
	}
	if reason := checkWritable(info.Mode(), "插件文件"); reason != "" {
		found.Skipped = reason
		return found
	}
	// Windows 没有 Unix 权限位，可执行性由扩展名决定，跳过这项检查。
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		found.Skipped = "插件文件没有可执行权限（chmod +x）"
		return found
	}

	abs, err := filepath.Abs(exe)
	if err != nil {
		found.Skipped = "无法解析插件路径"
		return found
	}
	found.Path = abs

	// 私有数据目录：插件要存 token 缓存、对账文件之类的东西，给它一个
	// 明确的位置，免得它往主程序的数据目录里乱写。
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		found.Skipped = "无法创建插件数据目录"
		return found
	}
	found.DataDir = dataDir
	frontend := filepath.Join(dir, "frontend", "index.html")
	if info, err := os.Stat(frontend); err == nil && info.Mode().IsRegular() {
		found.HasFrontend = true
	}
	return found
}

// checkWritable 检查权限位是否允许同组或其他用户写入。
//
// 插件以主程序的身份运行，因此「别人能改写这个文件」等价于「别人能以主程序
// 的权限执行任意代码」。这是本地提权，必须拦住。Windows 上权限模型不同，
// 由调用方跳过。
func checkWritable(mode fs.FileMode, what string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if mode.Perm()&0o022 != 0 {
		return fmt.Sprintf("%s可被其他用户写入（当前 %#o），请执行 chmod go-w", what, mode.Perm())
	}
	return ""
}

// ValidID 校验插件 ID。
//
// 只允许小写字母、数字与短横线，且不以短横线开头结尾。规则与 storage 包的
// 分类名一致：ID 会拼进数据库设置键名与日志前缀，收紧字符集省去后续所有
// 转义问题，也顺手挡掉 ".." 这类路径穿越。
func ValidID(id string) error {
	if id == "" {
		return errors.New("插件目录名为空")
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("插件目录名过长（上限 %d 个字符）", maxIDLen)
	}
	if id[0] == '-' || id[len(id)-1] == '-' {
		return errors.New("插件目录名不能以短横线开头或结尾")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return errors.New("插件目录名只能包含小写字母、数字与短横线")
		}
	}
	return nil
}
