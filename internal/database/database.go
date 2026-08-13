// Package database 负责按配置打开数据库连接并执行结构迁移。
//
// SQLite 使用 github.com/glebarez/sqlite（底层 modernc.org/sqlite），这是纯 Go
// 实现，不需要 cgo。这一点是硬性要求：官方 gorm.io/driver/sqlite 依赖
// mattn/go-sqlite3，在 CGO_ENABLED=0 下无法编译，也就做不到单二进制交叉编译。
package database

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/SakuraOpenSource/levis/internal/config"
	"github.com/SakuraOpenSource/levis/internal/model"
)

// Open 按 db 的配置建立连接并验证其可用性。
func Open(db config.Database) (*gorm.DB, error) {
	dsn, err := db.DSN()
	if err != nil {
		return nil, err
	}

	var dialector gorm.Dialector
	switch db.Driver {
	case config.DriverSQLite:
		// SQLite 是文件数据库，父目录必须先存在。
		if dir := filepath.Dir(db.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("创建数据库目录失败: %w", err)
			}
		}
		dialector = sqlite.Open(dsn)
	case config.DriverMySQL:
		dialector = mysql.Open(dsn)
	case config.DriverPostgres:
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("不支持的数据库类型: %s", db.Driver)
	}

	conn, err := gorm.Open(dialector, &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	pool, err := conn.DB()
	if err != nil {
		return nil, fmt.Errorf("获取连接池失败: %w", err)
	}
	if db.Driver == config.DriverSQLite {
		// SQLite 单文件写入不支持真正的并发写，限制为单连接可避免
		// "database is locked" 类错误。
		pool.SetMaxOpenConns(1)
	} else {
		pool.SetMaxOpenConns(25)
		pool.SetMaxIdleConns(5)
		pool.SetConnMaxLifetime(time.Hour)
	}
	if err := pool.Ping(); err != nil {
		return nil, fmt.Errorf("数据库无响应: %w", err)
	}
	return conn, nil
}

// TestConnection 仅验证连接可用，随后立即关闭。用于安装页的「测试连接」，
// 不会建表也不写入任何配置。
func TestConnection(db config.Database) error {
	conn, err := Open(db)
	if err != nil {
		return err
	}
	pool, err := conn.DB()
	if err != nil {
		return err
	}
	return pool.Close()
}

// Migrate 建立或更新全部表结构。
func Migrate(conn *gorm.DB) error {
	if err := conn.AutoMigrate(model.AllModels()...); err != nil {
		return fmt.Errorf("数据库迁移失败: %w", err)
	}
	return nil
}
