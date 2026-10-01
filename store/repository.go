package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repository 提供 Item 的增删改查与游标分页。
type Repository struct {
	db *DB
	tx *sql.Tx // 非空表示处于 InTransaction 事务内
}

// NewRepository 创建仓储。
func NewRepository(db *DB) *Repository {
	return &Repository{db: db}
}

// 固定列清单：显式列名而非 SELECT *，保证表结构变更后不会读串列。
const itemColumns = `id, name, value, status, version, created_at, updated_at`

const timeLayout = time.RFC3339Nano

// executor 抽象 *sql.DB 与 *sql.Tx 的共有接口。
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *Repository) exec() executor {
	if r.tx != nil {
		return r.tx
	}
	return r.db.sql
}

// InTransaction 在事务内执行 fn。fn 返回错误则整体回滚，
// 不会留下写了一半的数据；fn 返回 nil 才提交。
// fn 收到的 *Repository 上的所有写操作都落在同一事务里。
// 不支持嵌套：在事务内的 Repository 上再调用会返回错误（避免连接池死锁）。
func (r *Repository) InTransaction(ctx context.Context, fn func(txRepo *Repository) error) error {
	if r.tx != nil {
		return errors.New("store: nested InTransaction is not supported")
	}
	tx, err := r.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	txRepo := &Repository{db: r.db, tx: tx}
	if err := fn(txRepo); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// Create 插入一条记录，返回带 ID 和初始版本号（1）的记录。
func (r *Repository) Create(ctx context.Context, item Item) (Item, error) {
	now := time.Now().UTC()
	status := item.Status
	if status == "" {
		status = "active" // 与迁移默认值一致
	}
	res, err := r.exec().ExecContext(ctx,
		`INSERT INTO items (name, value, status, version, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		item.Name, item.Value, status,
		now.Format(timeLayout), now.Format(timeLayout))
	if err != nil {
		return Item{}, fmt.Errorf("insert item: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Item{}, fmt.Errorf("last insert id: %w", err)
	}
	item.ID = id
	item.Status = status
	item.Version = 1
	item.CreatedAt = now
	item.UpdatedAt = now
	return item, nil
}

// Get 按 ID 读取，不存在返回 ErrNotFound。
func (r *Repository) Get(ctx context.Context, id int64) (Item, error) {
	row := r.exec().QueryRowContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	if err != nil {
		return Item{}, fmt.Errorf("get item: %w", err)
	}
	return item, nil
}

// Update 做乐观锁更新：
// 仅当 item.Version 与库中一致才写入，成功时版本号 +1 并追加审计行；
// 版本不一致返回 ErrVersionConflict，记录不存在返回 ErrNotFound。
// 不在事务内时自动开启事务（两次写要么都成功要么都回滚）；
// 在 InTransaction 事务内调用时直接复用外层事务。
func (r *Repository) Update(ctx context.Context, item Item) (Item, error) {
	if r.tx != nil {
		return r.update(ctx, item)
	}
	var updated Item
	err := r.InTransaction(ctx, func(txRepo *Repository) error {
		var err error
		updated, err = txRepo.update(ctx, item)
		return err
	})
	if err != nil {
		return Item{}, err
	}
	return updated, nil
}

// update 是乐观锁更新的核心逻辑，在当前执行器（连接或事务）上执行。
func (r *Repository) update(ctx context.Context, item Item) (Item, error) {
	now := time.Now().UTC()
	res, err := r.exec().ExecContext(ctx,
		`UPDATE items
		 SET name = ?, value = ?, status = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		item.Name, item.Value, item.Status,
		now.Format(timeLayout), item.ID, item.Version)
	if err != nil {
		return Item{}, fmt.Errorf("update item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Item{}, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// 区分"不存在"与"版本冲突"：再查一次。
		if _, err := r.Get(ctx, item.ID); err != nil {
			return Item{}, err // ErrNotFound
		}
		return Item{}, fmt.Errorf("%w: id=%d, expected version=%d", ErrVersionConflict, item.ID, item.Version)
	}
	if _, err := r.exec().ExecContext(ctx,
		`INSERT INTO item_revisions (item_id, version, name, value, status, changed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		item.ID, item.Version+1, item.Name, item.Value, item.Status,
		now.Format(timeLayout)); err != nil {
		return Item{}, fmt.Errorf("insert revision: %w", err)
	}
	item.Version++
	item.UpdatedAt = now
	return item, nil
}

// Delete 按 ID 删除，不存在返回 ErrNotFound。
func (r *Repository) Delete(ctx context.Context, id int64) error {
	res, err := r.exec().ExecContext(ctx, `DELETE FROM items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return nil
}

// List 按 ID 升序做 keyset 分页。cursor 为空表示第一页。
// 翻页期间的插入/删除不会造成重复或遗漏：
// 新插入的记录 ID 更大，只会出现在后续页；已删除的记录天然跳过。
// 游标非法返回 ErrCursorMalformed，过期返回 ErrCursorExpired。
func (r *Repository) List(ctx context.Context, cursor string, limit int) (Page, error) {
	if limit <= 0 {
		limit = 50
	}
	var lastID int64
	if cursor != "" {
		id, err := decodeCursor(cursor, r.db.schemaVersion)
		if err != nil {
			return Page{}, err
		}
		lastID = id
	}
	// 多取一行判断是否有下一页。
	rows, err := r.exec().QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items WHERE id > ? ORDER BY id ASC LIMIT ?`,
		lastID, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	items := make([]Item, 0, limit)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return Page{}, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate items: %w", err)
	}

	page := Page{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(items[limit-1].ID, r.db.schemaVersion)
	}
	return page, nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanItem(s scanner) (Item, error) {
	var item Item
	var createdAt, updatedAt string
	err := s.Scan(&item.ID, &item.Name, &item.Value, &item.Status,
		&item.Version, &createdAt, &updatedAt)
	if err != nil {
		return Item{}, err
	}
	if item.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
		return Item{}, fmt.Errorf("parse created_at: %w", err)
	}
	if item.UpdatedAt, err = time.Parse(timeLayout, updatedAt); err != nil {
		return Item{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return item, nil
}
