package repository

import (
	"context"
	"database/sql"
	"fmt"
)

// migration 描述一个幂等的结构变更步骤。
type migration struct {
	Version     int
	Description string
	// Up 在事务内执行 DDL；必须幂等（重复执行不报错、不重复加列）。
	Up func(ctx context.Context, tx *sql.Tx) error
}

// migrations 按版本号升序排列，每个版本只执行一次并登记到 schema_migrations。
//
//	v1: users 基础表（注意：此时没有 age 列）。
//	v2: 新增 age 列（NOT NULL DEFAULT 0，0 = domain.AgeUnknown 表示未知），
//	    并新增 users_audit 审计表。ALTER 带列存在性检查，台账 + 检查双保险保证幂等。
var migrations = []migration{
	{
		Version:     1,
		Description: "create users table",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS users (
					id         INTEGER PRIMARY KEY AUTOINCREMENT,
					name       TEXT    NOT NULL,
					email      TEXT    NOT NULL,
					version    INTEGER NOT NULL DEFAULT 1,
					created_at INTEGER NOT NULL,
					updated_at INTEGER NOT NULL
				)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email)`,
				`CREATE INDEX IF NOT EXISTS idx_users_created_id ON users(created_at, id)`,
			}
			for _, s := range stmts {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("v1 ddl: %w", err)
				}
			}
			return nil
		},
	},
	{
		Version:     2,
		Description: "add users.age and create users_audit",
		Up: func(ctx context.Context, tx *sql.Tx) error {
			// SQLite 的 ADD COLUMN 重复执行会报错，因此先查列是否存在。
			// 正常路径下台账已保证只执行一次，这里让 DDL 本身也具备幂等性。
			exists, err := columnExists(ctx, tx, "users", "age")
			if err != nil {
				return err
			}
			if !exists {
				// NOT NULL + 常量 DEFAULT：SQLite 会给所有老行直接填 0，
				// 老数据因此在新结构下可读，0 的稳定含义是“年龄未知”。
				if _, err := tx.ExecContext(ctx,
					`ALTER TABLE users ADD COLUMN age INTEGER NOT NULL DEFAULT 0`); err != nil {
					return fmt.Errorf("v2 add age column: %w", err)
				}
			}
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS users_audit (
					id         INTEGER PRIMARY KEY AUTOINCREMENT,
					user_id    INTEGER NOT NULL,
					old_version INTEGER NOT NULL,
					new_version INTEGER NOT NULL,
					note       TEXT    NOT NULL CHECK(length(note) > 0),
					created_at INTEGER NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS idx_users_audit_user ON users_audit(user_id)`,
			}
			for _, s := range stmts {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					return fmt.Errorf("v2 audit ddl: %w", err)
				}
			}
			return nil
		},
	},
}

// columnExists 检查某张表是否已经包含指定列。
func columnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("pragma table_info %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ensureMigrations 建台账并按顺序执行所有未应用的迁移；重复调用是幂等的。
func ensureMigrations(ctx context.Context, db *sql.DB) error {
	return ensureMigrationsUpTo(ctx, db, migrations[len(migrations)-1].Version)
}

// ensureMigrationsUpTo 只应用版本号 <= target 的迁移，供测试构造
// “停留在旧结构、灌入老数据、再升级”的场景。
func ensureMigrationsUpTo(ctx context.Context, db *sql.DB, target int) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			description TEXT    NOT NULL,
			applied_at  INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("%w: create migration ledger: %v", ErrMigration, err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("%w: read migration ledger: %v", ErrMigration, err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("%w: scan migration ledger: %v", ErrMigration, err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: iterate migration ledger: %v", ErrMigration, err)
	}

	for _, m := range migrations {
		if m.Version > target {
			break
		}
		if applied[m.Version] {
			continue // 已应用：跳过，保证整段迁移可安全重复执行
		}
		if err := runMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// runMigration 在单个事务内执行 DDL 并登记版本；任一步失败则整体回滚。
func runMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: begin v%d: %v", ErrMigration, m.Version, err)
	}
	defer tx.Rollback() // 提交后再 Rollback 是 no-op
	if err := m.Up(ctx, tx); err != nil {
		return fmt.Errorf("%w: v%d %q: %v", ErrMigration, m.Version, m.Description, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, description, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Description, nowNanos()); err != nil {
		return fmt.Errorf("%w: record v%d: %v", ErrMigration, m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit v%d: %v", ErrMigration, m.Version, err)
	}
	return nil
}
