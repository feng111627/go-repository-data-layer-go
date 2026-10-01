package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateGetDelete(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, Item{Name: "alpha", Value: 10})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created id=%d version=%d status=%q", created.ID, created.Version, created.Status)
	if created.Version != 1 {
		t.Fatalf("initial version = %d, want 1", created.Version)
	}
	if created.Status != "active" {
		t.Fatalf("default status = %q, want active", created.Status)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alpha" || got.Value != 10 || got.Version != 1 {
		t.Fatalf("got %+v", got)
	}

	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again err = %v, want ErrNotFound", err)
	}
}

func TestUpdateOptimisticLock(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, Item{Name: "beta", Value: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := repo.Update(ctx, Item{ID: created.ID, Name: "beta2", Value: 2, Status: "active", Version: 1})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	t.Logf("update ok: version %d -> %d", created.Version, updated.Version)
	if updated.Version != 2 {
		t.Fatalf("version after update = %d, want 2", updated.Version)
	}

	// 用旧版本号再更新 → 版本冲突。
	_, err = repo.Update(ctx, Item{ID: created.ID, Name: "beta3", Value: 3, Status: "active", Version: 1})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update err = %v, want ErrVersionConflict", err)
	}
	t.Logf("stale update rejected: %v", err)

	// 不存在的 ID → ErrNotFound，且不能与冲突混淆。
	_, err = repo.Update(ctx, Item{ID: 99999, Name: "x", Version: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrVersionConflict) {
		t.Fatal("ErrNotFound must not match ErrVersionConflict")
	}

	// 冲突的失败更新不能覆盖已写入的数据。
	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "beta2" || got.Value != 2 || got.Version != 2 {
		t.Fatalf("data overwritten by stale update: %+v", got)
	}
}

func TestConcurrentUpdatesOnlyOneWins(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, Item{Name: "contended", Value: 0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同时放行，最大化竞争
			_, errs[i] = repo.Update(ctx, Item{
				ID: created.ID, Name: fmt.Sprintf("w%d", i),
				Value: int64(i), Status: "active", Version: created.Version,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var wins, conflicts int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			t.Logf("worker %d: update succeeded", i)
		case errors.Is(err, ErrVersionConflict):
			conflicts++
			t.Logf("worker %d: %v", i, err)
		default:
			t.Fatalf("worker %d: unexpected error %v", i, err)
		}
	}
	t.Logf("wins=%d conflicts=%d", wins, conflicts)
	if wins != 1 || conflicts != workers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1 win and %d conflicts", wins, conflicts, workers-1)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Logf("final version=%d", got.Version)
	if got.Version != 2 {
		t.Fatalf("final version = %d, want 2 (exactly one update applied)", got.Version)
	}
}

func TestTransactionRollbackLeavesNoPartialData(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()

	base, err := repo.Create(ctx, Item{Name: "base", Value: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sentinel := errors.New("boom")
	err = repo.InTransaction(ctx, func(txRepo *Repository) error {
		if _, err := txRepo.Create(ctx, Item{Name: "phantom", Value: 99}); err != nil {
			return err
		}
		if _, err := txRepo.Update(ctx, Item{ID: base.ID, Name: "changed", Value: 2, Status: "active", Version: 1}); err != nil {
			return err
		}
		return sentinel // 中途失败 → 整体回滚
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTransaction err = %v, want sentinel", err)
	}

	// 回滚后：phantom 不存在，base 未被改动，审计表也没有残留。
	page, err := repo.List(ctx, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items after rollback = %d, want 1 (phantom must be rolled back)", len(page.Items))
	}
	got, err := repo.Get(ctx, base.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Logf("after rollback: name=%q value=%d version=%d", got.Name, got.Value, got.Version)
	if got.Name != "base" || got.Value != 1 || got.Version != 1 {
		t.Fatalf("base item modified despite rollback: %+v", got)
	}
	var revCount int
	if err := repo.db.sql.QueryRow(`SELECT COUNT(1) FROM item_revisions`).Scan(&revCount); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if revCount != 0 {
		t.Fatalf("revisions after rollback = %d, want 0", revCount)
	}
}

func TestUpdateWritesRevisionAtomically(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()

	created, err := repo.Create(ctx, Item{Name: "audited", Value: 5})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Update(ctx, Item{ID: created.ID, Name: "audited2", Value: 6, Status: "active", Version: 1}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var revCount int
	if err := repo.db.sql.QueryRow(`SELECT COUNT(1) FROM item_revisions WHERE item_id = ?`, created.ID).Scan(&revCount); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if revCount != 1 {
		t.Fatalf("revisions = %d, want 1 (update + audit must commit together)", revCount)
	}
}
