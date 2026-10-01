package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/feng111627/go-repository-data-layer-go/internal/domain"

	_ "modernc.org/sqlite"
)

// openRaw 以 v1 结构打开一个全新库，模拟“表结构变更前”的老系统。
func openRawAt(t *testing.T, targetVersion int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mig.db"))
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if err := ensureMigrationsUpTo(context.Background(), db, targetVersion); err != nil {
		t.Fatalf("migrate to v%d: %v", targetVersion, err)
	}
	return db
}

// TestMigrationLegacyRowsReadableAfterV2 是迁移兼容性的核心结论：
// v1 时代写入的老数据没有 age 列；升级到 v2 后必须能读出，且 age 稳定地等于
// domain.AgeUnknown(0)，不会读串列；再重复执行迁移也必须幂等。
func TestMigrationLegacyRowsReadableAfterV2(t *testing.T) {
	ctx := context.Background()
	rawDB := openRawAt(t, 1)

	// 以 v1 的列结构直接插两条“老数据”（没有 age）。
	oldTS := int64(1000)
	for i, name := range []string{"old-a", "old-b"} {
		_, err := rawDB.ExecContext(ctx,
			`INSERT INTO users(name, email, version, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
			name, fmt.Sprintf("%s@legacy", name), oldTS+int64(i), oldTS+int64(i))
		if err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}
	if err := ensureMigrations(ctx, rawDB); err != nil {
		t.Fatalf("upgrade to latest: %v", err)
	}

	// 幂等：立刻再跑一遍完整迁移，必须无副作用、无错误。
	if err := ensureMigrations(ctx, rawDB); err != nil {
		t.Fatalf("re-run migrations must be idempotent, got: %v", err)
	}

	rows, err := rawDB.QueryContext(ctx,
		`SELECT id, name, email, age, version, created_at, updated_at FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("query after migration: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var u domain.User
		var ca, ua int64
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.Age, &u.Version, &ca, &ua); err != nil {
			t.Fatalf("scan legacy row: %v", err)
		}
		if u.Age != domain.AgeUnknown {
			t.Fatalf("legacy row %s age = %d, want %d (AgeUnknown)", u.Name, u.Age, domain.AgeUnknown)
		}
		if u.Name == "" || u.Email == "" || u.Version != 1 {
			t.Fatalf("legacy row columns shifted/corrupted: %+v", u)
		}
		t.Logf("老数据 %s 在新结构下可读: age=%d(未知) version=%d", u.Name, u.Age, u.Version)
		count++
	}
	if count != 2 {
		t.Fatalf("want 2 legacy rows, got %d", count)
	}

	// 审计表必须已建成可用。
	if _, err := rawDB.ExecContext(ctx,
		`INSERT INTO users_audit(user_id, old_version, new_version, note, created_at)
		 VALUES (1, 1, 2, 'ok', 1)`); err != nil {
		t.Fatalf("audit table must be usable after v2: %v", err)
	}
}

// TestNewInsertUsesExplicitAge 验证升级后新写入的显式年龄不被默认值吞掉。
func TestNewInsertUsesExplicitAge(t *testing.T) {
	ctx := context.Background()
	db := openRawAt(t, 2)
	t.Cleanup(func() { db.Close() })
	_, err := db.ExecContext(ctx,
		`INSERT INTO users(name, email, age, version, created_at, updated_at) VALUES (?, ?, ?, 1, 1, 1)`,
		"new", "n@x", 27)
	if err != nil {
		t.Fatalf("insert with age: %v", err)
	}
	var age int
	if err := db.QueryRowContext(ctx, `SELECT age FROM users WHERE name='new'`).Scan(&age); err != nil {
		t.Fatalf("read age: %v", err)
	}
	if age != 27 {
		t.Fatalf("explicit age = %d, want 27", age)
	}
}

// TestMigrationLedgerVersions 台账中应恰好登记 v1/v2 各一次。
func TestMigrationLedgerVersions(t *testing.T) {
	ctx := context.Background()
	db := openRawAt(t, 2)
	t.Cleanup(func() { db.Close() })
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("migration ledger = %v, want [1 2]", got)
	}
}

// TestEnsureMigrationsRejectsBadState 损坏库时返回包装了 ErrMigration 的错误。
func TestEnsureMigrationsRejectsBadState(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	// 缺 description/applied_at 列，INSERT 台账应失败且归类为 ErrMigration。
	err = ensureMigrations(context.Background(), db)
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("want ErrMigration, got %v", err)
	}
}
