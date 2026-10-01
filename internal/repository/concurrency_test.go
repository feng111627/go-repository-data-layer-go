package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentUpdateExactlyOneWinner 是“并发更新 + 乐观锁”的核心结论：
// N 个 goroutine 读取同一版本后同时提交，恰好 1 个成功，其余 N-1 个拿到
// ErrVersionConflict；最终数据等于唯一赢家的数据，版本只前进一步。
func TestConcurrentUpdateExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	u, err := r.Create(ctx, "shared", "shared@x", 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("初始: id=%d version=%d", u.ID, u.Version)

	const n = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, conflicts, others := 0, 0, 0
	winnerName := ""

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同时放行，最大化竞争
			name := fmt.Sprintf("writer-%02d", i)
			updated, err := r.Update(ctx, u.ID, name, name+"@x", 20+i, u.Version)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
				winnerName = name
				t.Logf("writer-%02d 赢: v%d -> v%d", i, u.Version, updated.Version)
			case errors.Is(err, ErrVersionConflict):
				conflicts++
				t.Logf("writer-%02d 输: %v", i, err)
			default:
				others++
				t.Logf("writer-%02d 其它错误: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	t.Logf("并发结论: 成功=%d 版本冲突=%d 其它=%d", winners, conflicts, others)
	if winners != 1 {
		t.Fatalf("恰好应有 1 个成功者, 实际 %d", winners)
	}
	if conflicts != n-1 {
		t.Fatalf("其余 %d 个请求都应是 ErrVersionConflict, 实际冲突 %d 个", n-1, conflicts)
	}
	if others != 0 {
		t.Fatalf("不应出现非冲突类错误, 有 %d 个", others)
	}

	final, err := r.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Version != 2 {
		t.Fatalf("并发写后版本应为 2, 实际 %d（说明有更新被覆盖/重放）", final.Version)
	}
	if final.Name != winnerName || final.Email != winnerName+"@x" {
		t.Fatalf("最终数据不是唯一赢家的: want name=%s got %+v", winnerName, final)
	}
	t.Logf("最终记录: name=%s version=%d（输家无一覆盖赢家）", final.Name, final.Version)
}

// TestConcurrentUpdateOnlyOneCanAdvanceEachVersion 自旋式读写：
// 8 个 worker 不断“读最新版本 -> 条件更新”，冲突就重读重试；
// 全局最多放行 targetWins 次写尝试，因此版本恰好前进 targetWins 次。
// 结论：每次尝试只有基于最新版本的那一个能赢，每个版本号只可能产生一次。
func TestConcurrentUpdateOnlyOneCanAdvanceEachVersion(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	u, err := r.Create(ctx, "spin", "spin@x", 0)
	if err != nil {
		t.Fatal(err)
	}

	const targetWins = 10
	const writers = 8
	tokens := make(chan struct{}, targetWins)
	for i := 0; i < targetWins; i++ {
		tokens <- struct{}{}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	start := make(chan struct{})

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for {
				// 领取一次写尝试配额；领完即退场
				select {
				case <-tokens:
				default:
					return
				}
				cur, err := r.Get(ctx, u.ID)
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				name := fmt.Sprintf("w%d-from-v%d", w, cur.Version)
				_, err = r.Update(ctx, u.ID, name, name+"@x", cur.Age+1, cur.Version)
				mu.Lock()
				switch {
				case err == nil:
					wins++
					t.Logf("writer%d 基于 v%d 抢写成功 -> v%d (age=%d)", w, cur.Version, cur.Version+1, cur.Age+1)
				case errors.Is(err, ErrVersionConflict):
					conflicts++
				default:
					t.Errorf("writer%d unexpected err: %v", w, err)
				}
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	wg.Wait()

	final, _ := r.Get(ctx, u.ID)
	t.Logf("多轮结论: 总成功=%d 总冲突=%d 最终版本=%d 最终 age=%d", wins, conflicts, final.Version, final.Age)
	if final.Version != 1+wins {
		t.Fatalf("版本应精确跟随成功次数: v=%d wins=%d", final.Version, wins)
	}
	if wins == 0 {
		t.Fatal("至少应有一次成功")
	}
	if final.Age != wins {
		t.Fatalf("age=%d 应等于成功写次数 %d（存在丢失更新）", final.Age, wins)
	}
	t.Logf("每次成功都使版本严格 +1 且无一丢失更新")
}
