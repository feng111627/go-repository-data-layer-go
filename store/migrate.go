package store

import (
	"database/sql"
	"fmt"
	"time"
)

// latestSchemaVersion 是当前代码支持的最新 schema 版本。
// v1: items 基础表；v2: items 增加 status 列（默认 'active'）；v3: 审计表。
const latestSchemaVersion = 3

// migration 是一步迁移：在事务内执行，成功后记录版本号。
type migration struct {
	version int
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS items (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				name       TEXT NOT NULL,
				value      INTEGER NOT NULL DEFAULT 0,
				version    INTEGER NOT NULL DEFAULT 1,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
	},
	{
		version: 2,
		// 新字段 status 的稳定默认含义：'active'。
		// NOT NULL + DEFAULT 保证老数据在新结构下读出确定值，不会读串列。
		stmts: []string{
			`ALTER TABLE items ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`,
		},
	},
	{
		version: 3,
		// 审计表：每次乐观锁更新成功时在同一事务里追加一行，
		// 用来验证"事务要么全写要么全不写"。
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS item_revisions (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				item_id    INTEGER NOT NULL REFERENCES items(id),
				version    INTEGER NOT NULL,
				name       TEXT NOT NULL,
				value      INTEGER NOT NULL,
				status     TEXT NOT NULL,
				changed_at TEXT NOT NULL
			)`,
		},
	},
}

// migrate 把数据库从当前版本迁移到最新版本。
// 通过 schema_migrations 表记录已应用的版本，重复执行幂等。
func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied int
		err := d.sql.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if applied > 0 {
			continue // 幂等：已应用过就跳过
		}

		tx, err := d.sql.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if err := applyMigration(tx, m); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	d.schemaVersion = latestSchemaVersion
	return nil
}

func applyMigration(tx *sql.Tx, m migration) error {
	for _, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
