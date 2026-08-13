// Package runtime 持有程序的运行时状态（数据库连接与配置）。
//
// 安装流程会在进程运行期间从「未安装」切换到「已安装」，此时需要把整个
// 运行时状态原子替换掉，因此这里用读写锁保护。所有 handler 通过 Runtime
// 取数据库句柄，而不是持有 *gorm.DB 的副本 —— 否则安装完成后旧句柄仍是 nil。
package runtime

import (
	"sync"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/config"
)

// Runtime 是并发安全的运行时状态容器。
type Runtime struct {
	mu      sync.RWMutex
	dataDir string
	cfg     *config.Config
	db      *gorm.DB
}

// New 创建一个未安装状态的 Runtime。
func New(dataDir string) *Runtime {
	return &Runtime{dataDir: dataDir}
}

// DataDir 返回数据目录。该值在进程生命周期内不变。
func (r *Runtime) DataDir() string { return r.dataDir }

// Installed 报告程序是否已完成安装。
func (r *Runtime) Installed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.db != nil
}

// DB 返回数据库句柄；未安装时返回 nil。
func (r *Runtime) DB() *gorm.DB {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.db
}

// Config 返回当前配置；未安装时返回 nil。
func (r *Runtime) Config() *config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// JWTSecret 返回 JWT 签名密钥；未安装时返回空串。
func (r *Runtime) JWTSecret() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cfg == nil {
		return ""
	}
	return r.cfg.JWTSecret
}

// Activate 用新的配置与数据库连接替换运行时状态，使程序进入已安装态。
func (r *Runtime) Activate(cfg *config.Config, db *gorm.DB) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	r.db = db
}
