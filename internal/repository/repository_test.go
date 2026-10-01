package repository

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/feng111627/go-repository-data-layer-go/internal/domain"
)

func newTestRepo(t *testing.T) *UserRepository {
	t.Helper()
	r, err := NewUserRepository(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)

	u, err := r.Create(ctx, "alice", "alice@x.com", 31)
	if err != nil {
		t.Fatal(err)
	}
	if u.Version != 1 {
		t.Fatalf("new user version = %d, want 1", u.Version)
	}
	got, err := r.Get(ctx, u.ID)
	if err != nil || got.Name != "alice" || got.Age != 31 {
		t.Fatalf("get mismatch: %+v err=%v", got, err)
	}

	upd, err := r.Update(ctx, u.ID, "alice2", "alice2@x.com", 32, 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	t.Logf("版本号变化: 1 -> %d", upd.Version)
	if upd.Version != 2 || upd.Name != "alice2" {
		t.Fatalf("updated mismatch: %+v", upd)
	}

	if err := r.Delete(ctx, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Get(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := r.Delete(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing row must be ErrNotFound, got %v", err)
	}
}

// TestPaginationTraverse 确定性数据下完整翻页，顺序与无重复无遗漏。
func TestPaginationTraverse(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		if _, err := r.CreateAt(ctx, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@x", i), 20+i, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	cursorRaw := ""
	page := 0
	for {
		page++
		users, next, err := r.List(ctx, cursorRaw, 2)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, u := range users {
			t.Logf("第%d页 游标=%q -> id=%d created=%s v=%d", page, cursorRaw, u.ID, u.CreatedAt.Format(time.RFC3339), u.Version)
			ids = append(ids, u.ID)
		}
		cursorRaw = next
		if next == "" {
			break
		}
	}
	if len(ids) != 5 {
		t.Fatalf("traversed %d rows, want 5: %v", len(ids), ids)
	}
	if fmt.Sprint(ids) != "[1 2 3 4 5]" {
		t.Fatalf("pagination order = %v, want [1 2 3 4 5]", ids)
	}
}

// TestPaginationStableUnderChurn 翻页过程中并发地删除未读行、锚点行并插入新行：
// 不重复、不漏掉任何仍存在的旧行；排序位于锚点之后的新行本次遍历也能读到。
func TestPaginationStableUnderChurn(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	base := time.Unix(2000, 0)
	for i := 0; i < 5; i++ {
		if _, err := r.CreateAt(ctx, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@x", i), 0, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	// 第一页：id1,id2，拿到锚点 (t=2001, id=2)
	page1, cur, err := r.List(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len=%d", len(page1))
	}
	t.Logf("首页游标: %q (锚点 id=%d ts=%d)", cur, page1[1].ID, page1[1].CreatedAt.UnixNano())

	// ---- 翻页进行中，数据发生变化 ----
	if err := r.Delete(ctx, 3); err != nil { // 删除一条尚未读到的行
		t.Fatal(err)
	}
	if err := r.Delete(ctx, 2); err != nil { // 删除锚点行本身
		t.Fatal(err)
	}
	// 插入一条排在锚点“之后”的新行（ts=2003, id 较新）→ 本次遍历应读到
	mid, err := r.CreateAt(ctx, "mid", "mid@x", 0, base.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// 插入一条排在锚点“之前”的新行（ts=2000, id 较新）→ 本次遍历读不到，
	// 这是 keyset 分页的明确定义：遍历是锚点之后的快照式窗口；要看到它需从首页重新开始。
	late, err := r.CreateAt(ctx, "late-before-anchor", "late@x", 0, base)
	if err != nil {
		t.Fatal(err)
	}

	// afterChurn 只统计“拿到首页游标之后”的后续页。
	afterChurn := map[int64]int{}
	cursorRaw := cur
	page := 1
	for cursorRaw != "" {
		page++
		users, next, err := r.List(ctx, cursorRaw, 2)
		if err != nil {
			t.Fatalf("page %d after churn: %v", page, err)
		}
		t.Logf("变动后第%d页 游标=%q 行数=%d", page, cursorRaw, len(users))
		for _, u := range users {
			afterChurn[u.ID]++
		}
		cursorRaw = next
	}

	// 后续页内部不能有重复
	for id, n := range afterChurn {
		if n > 1 {
			t.Fatalf("id=%d 在后续页返回了 %d 次（分页重复）", id, n)
		}
	}
	// 后续页绝不能吐出首页已经返回过的 id（含已删除的锚点 2 和 id3）
	for _, u := range page1 {
		if afterChurn[u.ID] != 0 {
			t.Fatalf("首页已返回的 id=%d 在后续页重复出现", u.ID)
		}
	}
	if afterChurn[3] != 0 {
		t.Fatalf("删除前尚未读到的 id=3 不应出现在后续页")
	}
	// 仍然存活的旧行 4,5 全部读到（id1 已在首页）
	for _, id := range []int64{4, 5} {
		if afterChurn[id] != 1 {
			t.Fatalf("旧行 id=%d 被漏掉", id)
		}
	}
	// 锚点之后的新行能读到
	if afterChurn[mid.ID] != 1 {
		t.Fatalf("锚点之后的新行 id=%d 应在本次遍历中读到", mid.ID)
	}
	// 锚点之前的迟到插入本次读不到（重新从首页翻可以读到）
	if afterChurn[late.ID] != 0 {
		t.Fatalf("排在锚点之前的迟到行 id=%d 不应出现在后续页", late.ID)
	}
	users, _, err := r.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range users {
		if u.ID == late.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("从首页重新开始时应能看到迟到行 id=%d", late.ID)
	}
	t.Logf("分页结论: 无重复; 存活旧行全读到; 删除行消失; 锚点后新行 id=%d 读到; 锚点前迟到行 id=%d 需回首页", mid.ID, late.ID)
}

// TestListCursorErrorClassification 通过 List 验证两类游标错误可区分。
func TestListCursorErrorClassification(t *testing.T) {
	ctx := context.Background()
	base := time.Unix(3000, 0)
	cur := base
	dsn := filepath.Join(t.TempDir(), "cursor.db")
	r, err := newRepository(ctx, dsn, func() time.Time { return cur })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.CreateAt(ctx, "a", "a@x", 0, base); err != nil {
		t.Fatal(err)
	}
	_, validCursor, err := r.List(ctx, "", 1)
	if err != nil {
		t.Fatal(err)
	}

	// 非法游标
	if _, _, err := r.List(ctx, "@@garbage@@", 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("非法游标 want ErrInvalidCursor, got %v", err)
	} else {
		t.Logf("非法游标被拒绝: %v", err)
	}

	// 合法游标，但时钟前进超过 TTL
	cur = cur.Add(cursorTTL + time.Minute)
	_, _, err = r.List(ctx, validCursor, 1)
	if !errors.Is(err, ErrExpiredCursor) {
		t.Fatalf("过期游标 want ErrExpiredCursor, got %v", err)
	}
	if errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("过期游标不应同时被判为非法: %v", err)
	}
	t.Logf("过期游标被拒绝（可与非法区分）: %v", err)
}

// TestOptimisticLockNoLostUpdate 两个请求基于同一版本先后提交：
// 先提交者成功，后提交者拿到 ErrVersionConflict，且先提交者的数据不会被覆盖。
func TestOptimisticLockNoLostUpdate(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	u, err := r.Create(ctx, "orig", "orig@x", 20)
	if err != nil {
		t.Fatal(err)
	}

	// 两个请求都读到 version=1
	winner, err := r.Update(ctx, u.ID, "winner", "winner@x", 40, u.Version)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("先到请求: v%d -> v%d 成功", u.Version, winner.Version)

	_, err = r.Update(ctx, u.ID, "loser", "loser@x", 50, u.Version)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("后到的过期请求 want ErrVersionConflict, got %v", err)
	}
	t.Logf("后到请求（仍带 v%d）被拒绝: %v", u.Version, err)

	final, err := r.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Name != "winner" || final.Email != "winner@x" || final.Age != 40 {
		t.Fatalf("输家的数据覆盖了赢家! final=%+v", final)
	}
	if final.Version != 2 {
		t.Fatalf("失败更新不应推进版本, final version=%d", final.Version)
	}

	// 用最新版本重试可以成功
	retry, err := r.Update(ctx, u.ID, "retry", "retry@x", 41, final.Version)
	if err != nil {
		t.Fatalf("带最新版本重试应成功: %v", err)
	}
	t.Logf("用新版本 v%d 重试 -> v%d 成功", final.Version, retry.Version)
}

// TestUpdateWithAuditRollback 事务中途失败必须整体回滚：
// 审计 note 为空触发 NOT NULL，用户更新也不得生效，版本不前进，审计表无残留。
func TestUpdateWithAuditRollback(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	u, err := r.Create(ctx, "before", "before@x", 20)
	if err != nil {
		t.Fatal(err)
	}

	// 成功路径：用户更新 + 审计同事务提交
	ok, err := r.UpdateWithAudit(ctx, u.ID, "after", "after@x", 21, 1, "changed name")
	if err != nil {
		t.Fatalf("audit update: %v", err)
	}
	if ok.Version != 2 {
		t.Fatalf("version = %d, want 2", ok.Version)
	}
	if n := r.countAudit(t, ctx, u.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	// 失败路径：空 note 让审计插入违反 NOT NULL，整个事务回滚
	_, err = r.UpdateWithAudit(ctx, u.ID, "half-written", "half@x", 99, 2, "")
	if err == nil {
		t.Fatal("空 note 应导致事务失败")
	}
	t.Logf("事务中途失败返回错误（非乐观锁类）: %v", err)
	if errors.Is(err, ErrVersionConflict) {
		t.Fatal("约束失败不应被误分类为版本冲突")
	}

	after, err := r.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "after" || after.Age != 21 || after.Version != 2 {
		t.Fatalf("回滚不彻底，出现半成品数据: %+v", after)
	}
	if n := r.countAudit(t, ctx, u.ID); n != 1 {
		t.Fatalf("失败事务留下了审计残留，audit rows = %d, want 1", n)
	}
	t.Logf("回滚结论: 用户保持 %+v，审计仍为 1 条，版本停在 v%d", after.Name, after.Version)
}

// TestDefaultAgeUnknown 经仓储新建时 age 零值即 domain.AgeUnknown。
func TestDefaultAgeUnknown(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	u, err := r.Create(ctx, "ageless", "ageless@x", domain.AgeUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if u.Age != 0 {
		t.Fatalf("age = %d, want 0(AgeUnknown)", u.Age)
	}
}

func (r *UserRepository) countAudit(t *testing.T, ctx context.Context, userID int64) int {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users_audit WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}
