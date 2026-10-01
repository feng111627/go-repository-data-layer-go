package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func seedItems(t *testing.T, repo *Repository, n int) []Item {
	t.Helper()
	ctx := context.Background()
	items := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		item, err := repo.Create(ctx, Item{Name: fmt.Sprintf("item-%02d", i), Value: int64(i)})
		if err != nil {
			t.Fatalf("seed create: %v", err)
		}
		items = append(items, item)
	}
	return items
}

func collectAll(t *testing.T, repo *Repository, pageSize int) []Item {
	t.Helper()
	ctx := context.Background()
	var all []Item
	cursor := ""
	for page := 0; ; page++ {
		p, err := repo.List(ctx, cursor, pageSize)
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		t.Logf("page %d: %d items, cursor %q -> next %q", page, len(p.Items), cursor, p.NextCursor)
		all = append(all, p.Items...)
		if p.NextCursor == "" {
			return all
		}
		cursor = p.NextCursor
	}
}

func ids(items []Item) map[int64]bool {
	m := make(map[int64]bool, len(items))
	for _, it := range items {
		m[it.ID] = true
	}
	return m
}

func TestPaginationCoversAllExactlyOnce(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	seeded := seedItems(t, repo, 25)

	all := collectAll(t, repo, 10)
	if len(all) != 25 {
		t.Fatalf("collected %d items, want 25", len(all))
	}
	seen := make(map[int64]int)
	for _, it := range all {
		seen[it.ID]++
	}
	for _, s := range seeded {
		if seen[s.ID] != 1 {
			t.Fatalf("id=%d returned %d times, want exactly 1", s.ID, seen[s.ID])
		}
	}
	// 顺序必须是 ID 升序。
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatalf("order broken at %d: id %d then %d", i, all[i-1].ID, all[i].ID)
		}
	}
}

func TestPaginationWithConcurrentInsert(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()
	seedItems(t, repo, 10)

	// 翻第一页后插入新数据，再继续翻页。
	p1, err := repo.List(ctx, "", 4)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	t.Logf("page 1: %d items, next cursor %q", len(p1.Items), p1.NextCursor)

	for i := 0; i < 3; i++ {
		if _, err := repo.Create(ctx, Item{Name: fmt.Sprintf("late-%d", i)}); err != nil {
			t.Fatalf("late create: %v", err)
		}
	}

	rest := []Item{}
	cursor := p1.NextCursor
	for cursor != "" {
		p, err := repo.List(ctx, cursor, 4)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		t.Logf("page: %d items, cursor %q -> next %q", len(p.Items), cursor, p.NextCursor)
		rest = append(rest, p.Items...)
		cursor = p.NextCursor
	}

	all := append(p1.Items, rest...)
	if len(all) != 13 {
		t.Fatalf("collected %d items, want 13 (10 + 3 late)", len(all))
	}
	seen := make(map[int64]int)
	for _, it := range all {
		seen[it.ID]++
		if seen[it.ID] > 1 {
			t.Fatalf("id=%d returned %d times (duplicate across pages)", it.ID, seen[it.ID])
		}
	}
}

func TestPaginationWithConcurrentDelete(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()
	seeded := seedItems(t, repo, 10)

	p1, err := repo.List(ctx, "", 4)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	t.Logf("page 1: %d items, next cursor %q", len(p1.Items), p1.NextCursor)

	// 删掉一条已翻过的（旧数据）和一条还没翻到的。
	if err := repo.Delete(ctx, seeded[0].ID); err != nil {
		t.Fatalf("delete seen: %v", err)
	}
	if err := repo.Delete(ctx, seeded[7].ID); err != nil {
		t.Fatalf("delete unseen: %v", err)
	}

	rest := []Item{}
	cursor := p1.NextCursor
	for cursor != "" {
		p, err := repo.List(ctx, cursor, 4)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		t.Logf("page: %d items, cursor %q -> next %q", len(p.Items), cursor, p.NextCursor)
		rest = append(rest, p.Items...)
		cursor = p.NextCursor
	}

	// 已翻过的页不重查，所以最终结果 = 第一页 4 条 + 剩余未删的 4 条（id 5,6,7,9 减去删掉的第 8 条）。
	all := append(p1.Items, rest...)
	seen := make(map[int64]int)
	for _, it := range all {
		seen[it.ID]++
		if seen[it.ID] > 1 {
			t.Fatalf("id=%d duplicated across pages", it.ID)
		}
	}
	if seen[seeded[7].ID] != 0 {
		t.Fatalf("deleted id=%d must not appear in later pages", seeded[7].ID)
	}
	// 未删除且未翻过的记录必须全部出现，不能漏。
	for _, s := range seeded[4:] {
		if s.ID == seeded[7].ID {
			continue
		}
		if seen[s.ID] != 1 {
			t.Fatalf("id=%d missing from pagination result", s.ID)
		}
	}
}

func TestCursorErrorsAreClassified(t *testing.T) {
	repo := NewRepository(openTestDB(t))
	ctx := context.Background()
	seedItems(t, repo, 3)

	malformed := []string{
		"not-base64!!!", // 不是合法 base64url
		"aGVsbG8",       // base64 合法但内容格式不对
	}
	for _, c := range malformed {
		_, err := repo.List(ctx, c, 2)
		if !errors.Is(err, ErrCursorMalformed) {
			t.Fatalf("cursor %q err = %v, want ErrCursorMalformed", c, err)
		}
		t.Logf("malformed cursor %q -> %v", c, err)
		if errors.Is(err, ErrCursorExpired) {
			t.Fatalf("cursor %q must not be classified as expired", c)
		}
	}

	// 过期：游标签发时的 schema 版本落后于当前。
	stale := encodeCursor(1, repo.db.schemaVersion-1)
	_, err := repo.List(ctx, stale, 2)
	if !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("stale cursor err = %v, want ErrCursorExpired", err)
	}
	t.Logf("expired cursor -> %v", err)
	if errors.Is(err, ErrCursorMalformed) {
		t.Fatal("expired cursor must not be classified as malformed")
	}

	// 合法游标正常工作。
	p, err := repo.List(ctx, encodeCursor(1, repo.db.schemaVersion), 2)
	if err != nil {
		t.Fatalf("valid cursor: %v", err)
	}
	if len(p.Items) == 0 {
		t.Fatal("valid cursor returned no items")
	}
}
