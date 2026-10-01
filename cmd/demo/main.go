// Command demo 在一个本地 SQLite 文件上端到端演示数据访问层的四件事：
//  1. 游标分页（并在翻页过程中插入/删除，观察不重不漏）
//  2. 并发更新同一条记录（恰好一个成功，其余版本冲突）
//  3. 事务中途失败整体回滚（不留半成品）
//  4. 旧结构老数据迁移到新结构后兼容读取
//
// 用法: go run ./cmd/demo [db文件路径]，默认 /tmp/go-repo-demo.db
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/feng111627/go-repository-data-layer-go/internal/domain"
	"github.com/feng111627/go-repository-data-layer-go/internal/repository"
)

func main() {
	path := "/tmp/go-repo-demo.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.Remove(path) // 演示需要可重复复现：每次重建

	ctx := context.Background()
	repo, err := repository.NewUserRepository(ctx, path)
	must(err)
	defer repo.Close()

	demoPagination(ctx, repo)
	demoConcurrentUpdate(ctx, repo)
	demoRollback(ctx, repo)
	demoMigration(ctx, repo)

	fmt.Println("\n全部演示完成。数据库文件:", path)
}

func demoPagination(ctx context.Context, repo *repository.UserRepository) {
	fmt.Println("\n==== 1. 游标分页（翻页中插入/删除） ====")
	base := time.Unix(1_000_000, 0)
	for i := 0; i < 6; i++ {
		_, err := repo.CreateAt(ctx, fmt.Sprintf("u%d", i), fmt.Sprintf("u%d@x.com", i), 20+i,
			base.Add(time.Duration(i)*time.Second))
		must(err)
	}

	cursorRaw := ""
	page := 0
	churned := false
	for {
		page++
		users, next, err := repo.List(ctx, cursorRaw, 2)
		must(err)
		for _, u := range users {
			fmt.Printf("  第%d页 id=%-2d name=%s v=%d\n", page, u.ID, u.Name, u.Version)
		}

		// 在第 1 页与第 2 页之间制造变动：删掉未读的 id=3，再插一条新行。
		if page == 1 && !churned {
			churned = true
			must(repo.Delete(ctx, 3))
			_, err := repo.CreateAt(ctx, "inserted-during-paging", "new@x.com", 0,
				base.Add(90*time.Second))
			must(err)
			fmt.Println("  -- 翻页中途: 删除 id=3，插入新行 inserted-during-paging --")
		}
		cursorRaw = next
		if next == "" {
			break
		}
	}
	fmt.Println("  结论: 同一次遍历中每条存活记录只出现一次，删除行消失，锚点后的新行可见。")
}

func demoConcurrentUpdate(ctx context.Context, repo *repository.UserRepository) {
	fmt.Println("\n==== 2. 并发更新同一条记录（乐观锁） ====")
	u, err := repo.Create(ctx, "hot-row", "hot@x.com", 0)
	must(err)
	fmt.Printf("  初始: id=%d version=%d\n", u.ID, u.Version)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("writer-%d", i)
			_, err := repo.Update(ctx, u.ID, name, name+"@x.com", i, u.Version) // 都基于读到的 v=1
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				fmt.Printf("  writer-%d: 成功 v%d -> v%d\n", i, u.Version, u.Version+1)
			} else if errors.Is(err, repository.ErrVersionConflict) {
				conflicts++
			} else {
				panic(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	final, err := repo.Get(ctx, u.ID)
	must(err)
	fmt.Printf("  结果: 成功=%d 版本冲突=%d；最终 name=%s version=%d（输家无一覆盖赢家）\n",
		wins, conflicts, final.Name, final.Version)
}

func demoRollback(ctx context.Context, repo *repository.UserRepository) {
	fmt.Println("\n==== 3. 事务中途失败整体回滚 ====")
	u, err := repo.Create(ctx, "tx", "tx@x.com", 20)
	must(err)

	_, err = repo.UpdateWithAudit(ctx, u.ID, "updated", "updated@x.com", 21, u.Version, "改名")
	must(err)
	fmt.Printf("  正常: 更新 + 审计同事务提交，version -> 2\n")

	_, err = repo.UpdateWithAudit(ctx, u.ID, "should-not-persist", "x@x.com", 99, 2, "") // 空 note 违反 CHECK
	if err == nil {
		panic("空 note 应当失败")
	}
	fmt.Printf("  注入失败: %v\n", err)

	got, err := repo.Get(ctx, u.ID)
	must(err)
	fmt.Printf("  回滚后: name=%s age=%d version=%d（更新未留下，版本未前进）\n",
		got.Name, got.Age, got.Version)
}

func demoMigration(ctx context.Context, repo *repository.UserRepository) {
	fmt.Println("\n==== 4. 迁移兼容读取（老数据的新字段默认含义） ====")
	// 当前库已由 NewUserRepository 迁移到最新版本；v1 时代插入的行 age 列为默认 0。
	u, err := repo.CreateLegacy(ctx)
	must(err)
	got, err := repo.Get(ctx, u.ID)
	must(err)
	meaning := "未知/未填写"
	if got.Age != domain.AgeUnknown {
		meaning = "异常!"
	}
	fmt.Printf("  老结构行在新结构下可读: id=%d name=%s age=%d（含义: %s）\n",
		got.ID, got.Name, got.Age, meaning)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
