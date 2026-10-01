package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/feng111627/go-repository-data-layer-go/internal/domain"

	_ "modernc.org/sqlite"
)

// UserRepository 封装 users 表的全部数据访问。
type UserRepository struct {
	db  *sql.DB
	now func() time.Time
}

// NewUserRepository 打开/创建 SQLite 数据库并执行迁移，返回就绪的仓储。
func NewUserRepository(ctx context.Context, dsn string) (*UserRepository, error) {
	return newRepository(ctx, dsn, time.Now)
}

// newRepository 允许注入时钟（测试用于确定性分页与游标过期断言）。
func newRepository(ctx context.Context, dsn string, clock func() time.Time) (*UserRepository, error) {
	// _txlock=immediate 让写事务一开始就拿写锁，配合写连接串行化（见下方 SetMaxOpenConns），
	// 避免 SQLite 写事务在并发下中途拿锁升级失败（SQLITE_BUSY）。
	db, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite 同一时刻只允许一个 writer；连接池里只放一个连接，
	// 所有写事务在驱动层排队，仓储层用乐观锁决定谁的更新生效。
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := ensureMigrations(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &UserRepository{db: db, now: clock}, nil
}

// Close 释放底层连接。
func (r *UserRepository) Close() error { return r.db.Close() }

// userColumns 必须与 scanUser 的列顺序一致；age 为 v2 迁移后才存在的列。
const userColumns = "id, name, email, age, version, created_at, updated_at"

// scanUser 从任意 rows/row 扫描一个用户，集中处理“行 -> 领域对象”的映射，
// 避免不同查询按不同列顺序扫描导致读串列。
func scanUser(s interface {
	Scan(dest ...any) error
}) (*domain.User, error) {
	var u domain.User
	var createdAt, updatedAt int64
	if err := s.Scan(&u.ID, &u.Name, &u.Email, &u.Age, &u.Version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(0, createdAt)
	u.UpdatedAt = time.Unix(0, updatedAt)
	return &u, nil
}

// Create 插入一条用户，返回带 id/版本/时间戳的完整对象。
func (r *UserRepository) Create(ctx context.Context, name, email string, age int) (*domain.User, error) {
	return r.CreateAt(ctx, name, email, age, r.now())
}

// CreateAt 是仅供测试/演示使用的辅助方法：在指定创建时间插入用户，
// 便于构造确定性的分页与“迁移前老数据”场景。
func (r *UserRepository) CreateAt(ctx context.Context, name, email string, age int, createdAt time.Time) (*domain.User, error) {
	updatedAt := createdAt.UnixNano()
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO users(name, email, age, version, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		name, email, age, createdAt.UnixNano(), updatedAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create user last id: %w", err)
	}
	return r.Get(ctx, id)
}

// Get 按主键读取。
func (r *UserRepository) Get(ctx context.Context, id int64) (*domain.User, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE id = ?", id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: user id=%d", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// List 按 (created_at, id) 升序做游标分页。
// cursorRaw 为空表示第一页；最多返回 limit 条，并返回下一页游标（无更多数据时为空串）。
func (r *UserRepository) List(ctx context.Context, cursorRaw string, limit int) ([]*domain.User, string, error) {
	if limit <= 0 {
		limit = 20
	}
	c, firstPage, err := decodeCursor(cursorRaw, r.now())
	if err != nil {
		return nil, "", err
	}

	query := "SELECT " + userColumns + " FROM users"
	args := []any{}
	if !firstPage {
		// keyset 翻页：严格取锚点之后的行。
		// 即使锚点行在翻页期间被删除，条件仍然有效（比较的是新行自身的键），
		// 因此不会重复也不会漏行。
		query += " WHERE created_at > ? OR (created_at = ? AND id > ?)"
		args = append(args, c.CreatedAt, c.CreatedAt, c.ID)
	}
	query += " ORDER BY created_at ASC, id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := make([]*domain.User, 0, limit)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan user row: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate users: %w", err)
	}

	next := ""
	if len(users) == limit {
		last := users[len(users)-1]
		next, err = encodeCursor(cursor{
			CreatedAt: last.CreatedAt.UnixNano(),
			ID:        last.ID,
			IssuedAt:  r.now().UnixNano(),
		})
		if err != nil {
			return nil, "", err
		}
	}
	return users, next, nil
}

// Delete 删除一条记录；记录不存在返回 ErrNotFound。
func (r *UserRepository) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: user id=%d", ErrNotFound, id)
	}
	return nil
}

// Update 在单个事务内带乐观锁更新用户。
// expectVersion 为调用方读取到的版本号：仅当库中版本等于它时更新才生效，
// 成功后新版本号为 expectVersion+1；否则返回 ErrVersionConflict。
func (r *UserRepository) Update(ctx context.Context, id int64, name, email string, age, expectVersion int) (*domain.User, error) {
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		return r.updateLocked(ctx, tx, id, name, email, age, expectVersion)
	})
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

// updateLocked 执行条件更新：WHERE version = ? 是乐观锁的核心。
// 0 行受影响时区分“记录不存在(ErrNotFound)”与“版本不匹配(ErrVersionConflict)”。
func (r *UserRepository) updateLocked(ctx context.Context, tx *sql.Tx, id int64, name, email string, age, expectVersion int) error {
	ts := r.now().UnixNano()
	res, err := tx.ExecContext(ctx,
		`UPDATE users
		    SET name = ?, email = ?, age = ?, version = version + 1, updated_at = ?
		  WHERE id = ? AND version = ?`,
		name, email, age, ts, id, expectVersion)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update rows affected: %w", err)
	}
	if n == 1 {
		return nil
	}
	// 区分冲突原因：行还在但版本已前进 -> 冲突；行没了 -> 不存在。
	var current int
	err = tx.QueryRowContext(ctx, `SELECT version FROM users WHERE id = ?`, id).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: user id=%d was deleted", ErrNotFound, id)
	case err != nil:
		return fmt.Errorf("recheck user version: %w", err)
	default:
		return fmt.Errorf("%w: user id=%d expected version %d but current is %d",
			ErrVersionConflict, id, expectVersion, current)
	}
}

// UpdateWithAudit 在同一个事务里更新用户并追加一条审计记录。
// 当 auditNote 为空字符串时，审计插入会故意触发 NOT NULL 约束失败，
// 用于演示“事务中途失败整体回滚，不留半成品”：
// 此时用户更新与审计插入都不会留下任何痕迹，版本号也不前进。
func (r *UserRepository) UpdateWithAudit(ctx context.Context, id int64, name, email string, age, expectVersion int, auditNote string) (*domain.User, error) {
	err := r.inTx(ctx, func(tx *sql.Tx) error {
		if err := r.updateLocked(ctx, tx, id, name, email, age, expectVersion); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users_audit(user_id, old_version, new_version, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			id, expectVersion, expectVersion+1, auditNote, r.now().UnixNano())
		if err != nil {
			return fmt.Errorf("insert audit (note 为空违反 CHECK 约束): %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

// inTx 执行事务模板：panic/错误一律回滚，nil 才提交。
func (r *UserRepository) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Commit 成功后 Rollback 返回 sql.ErrTxDone，被显式忽略。
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// CreateLegacy 以 v1 的列集合插入一行（age 走列默认值 0），
// 专用于演示“结构升级后老数据如何兼容读取”。在已迁移到 v2 的库上，
// 效果等同于插入 age=0，但这里显式省略 age 列以模拟历史写入路径。
func (r *UserRepository) CreateLegacy(ctx context.Context) (*domain.User, error) {
	ts := r.now().UnixNano()
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO users(name, email, version, created_at, updated_at)
		 VALUES ('legacy-user', 'legacy@example.com', 1, ?, ?)`, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create legacy user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}
