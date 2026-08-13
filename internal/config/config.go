// Package config 负责 Levis 运行时配置的读写。
//
// 配置文件默认位于 ./data/config.json。该文件是否存在决定了程序处于
// 「已安装」还是「未安装」状态：不存在即未安装，此时只有安装相关接口可用。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// 支持的数据库类型。
const (
	DriverSQLite   = "sqlite"
	DriverMySQL    = "mysql"
	DriverPostgres = "postgres"
)

// Database 保存数据库连接参数。SQLite 只使用 Path，其余字段留空。
type Database struct {
	Driver   string `json:"driver"`
	Path     string `json:"path,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Config 是 config.json 的完整结构。
type Config struct {
	Database  Database `json:"database"`
	JWTSecret string   `json:"jwt_secret"`
	Listen    string   `json:"listen"`
}

// DefaultListen 是未指定监听地址时的默认值。
const DefaultListen = ":8080"

// Path 返回配置文件路径。
func Path(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// Exists 报告配置文件是否存在，即程序是否已完成安装。
func Exists(dataDir string) bool {
	info, err := os.Stat(Path(dataDir))
	return err == nil && !info.IsDir()
}

// Load 从 dataDir 读取配置。
func Load(dataDir string) (*Config, error) {
	raw, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	return &cfg, nil
}

// Save 将配置以 0600 权限原子写入 dataDir。
//
// 配置含数据库密码与 JWT 密钥，因此权限收紧到仅所有者可读写。先写临时文件
// 再 rename，避免写入中途崩溃留下半个文件。
func Save(dataDir string, cfg *Config) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	target := Path(dataDir)
	tmp, err := os.CreateTemp(dataDir, "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此调用是无害的 no-op

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置配置文件权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭配置文件失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}
	return nil
}

// GenerateSecret 生成一个 64 位十六进制随机串，用作 JWT 签名密钥。
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成密钥失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Validate 校验数据库参数是否完整。
func (d Database) Validate() error {
	switch d.Driver {
	case DriverSQLite:
		if d.Path == "" {
			return fmt.Errorf("SQLite 数据库文件路径不能为空")
		}
	case DriverMySQL, DriverPostgres:
		if d.Host == "" {
			return fmt.Errorf("数据库地址不能为空")
		}
		if d.Port <= 0 || d.Port > 65535 {
			return fmt.Errorf("数据库端口无效")
		}
		if d.User == "" {
			return fmt.Errorf("数据库用户名不能为空")
		}
		if d.Name == "" {
			return fmt.Errorf("数据库名不能为空")
		}
	default:
		return fmt.Errorf("不支持的数据库类型: %s", d.Driver)
	}
	return nil
}

// DSN 组装 GORM 所需的连接字符串。
func (d Database) DSN() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	switch d.Driver {
	case DriverSQLite:
		// 纯 Go 驱动的 pragma 通过 DSN 查询参数传递；SQLite 默认不开外键约束。
		// busy_timeout 让并发写入等待而不是立刻返回 SQLITE_BUSY。
		return d.Path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", nil
	case DriverMySQL:
		return fmt.Sprintf(
			"%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=UTC",
			d.User, d.Password, d.Host, d.Port, d.Name,
		), nil
	case DriverPostgres:
		// 用 URL 形式并对用户名/密码转义，避免密码含特殊字符时 DSN 被截断。
		u := url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(d.User, d.Password),
			Host:     fmt.Sprintf("%s:%s", d.Host, strconv.Itoa(d.Port)),
			Path:     "/" + d.Name,
			RawQuery: "sslmode=prefer&TimeZone=UTC",
		}
		return u.String(), nil
	}
	return "", fmt.Errorf("不支持的数据库类型: %s", d.Driver)
}
