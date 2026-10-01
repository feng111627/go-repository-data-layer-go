package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	ctx := context.Background()

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if v := db1.SchemaVersion(); v != latestSchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, latestSchemaVersion)
	}
	repo1 := NewRepository(db1)
	created, err := repo1.Create(ctx, Item{Name: "persist", Value: 7})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重复打开 = 重复执行迁移，必须幂等：不报错、数据不丢。
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second open (re-run migrations): %v", err)
	}
	defer db2.Close()
	if v := db2.SchemaVersion(); v != latestSchemaVersion {
		t.Fatalf("schema version after re-open = %d, want %d", v, latestSchemaVersion)
	}
	var migrationRows int
	if err := db2.sql.QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&migrationRows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	t.Logf("applied migrations = %d (each version recorded exactly once)", migrationRows)
	if migrationRows != latestSchemaVersion {
		t.Fatalf("migration rows = %d, want %d (no duplicate applications)", migrationRows, latestSchemaVersion)
	}
	got, err := NewRepository(db2).Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after re-open: %v", err)
	}
	if got.Name != "persist" || got.Value != 7 {
		t.Fatalf("data lost across re-open: %+v", got)
	}
}

// 手工构造一个只有 v1 结构（无 status 列）的旧库，验证迁移后老数据能正确读出。
func TestMigrationFromV1KeepsOldDataReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	// 构造 v1 库。
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		value INTEGER NOT NULL DEFAULT 0,
		version INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create v1 table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO items (name, value, version, created_at, updated_at)
		VALUES ('legacy', 42, 3, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// 用当前代码打开：应迁移到最新版本，老数据按新结构读出。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	if v := db.SchemaVersion(); v != latestSchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, latestSchemaVersion)
	}

	got, err := NewRepository(db).Get(ctx, 1)
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	t.Logf("legacy row after migration: name=%q value=%d status=%q version=%d",
		got.Name, got.Value, got.Status, got.Version)
	if got.Name != "legacy" || got.Value != 42 || got.Version != 3 {
		t.Fatalf("legacy row columns misaligned after migration: %+v", got)
	}
	// 新字段的稳定默认含义：老数据 status 一律为 'active'。
	if got.Status != "active" {
		t.Fatalf("legacy row status = %q, want default 'active'", got.Status)
	}
}
