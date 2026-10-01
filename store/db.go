package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB 包装数据库连接与当前 schema 版本。
type DB struct {
	sql           *sql.DB
	schemaVersion int
}

// Open 打开（必要时创建）数据库并执行迁移到最新版本。
// path 传 ":memory:" 可得到纯内存库。
func Open(path string) (*DB, error) {
	// _pragma：WAL 提升读写并发，busy_timeout 避免并发写直接报 SQLITE_BUSY，
	// foreign_keys 保持惯例开启。单连接池避免内存库多连接不一致。
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	d := &DB{sql: sqlDB}
	if err := d.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close 关闭连接。
func (d *DB) Close() error {
	return d.sql.Close()
}

// SchemaVersion 返回迁移完成后的 schema 版本。
func (d *DB) SchemaVersion() int {
	return d.schemaVersion
}
