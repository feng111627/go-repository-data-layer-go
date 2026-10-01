# Go 数据访问层（SQLite 本地库）

一个自包含的数据访问层实现，把**读写、游标分页、事务与乐观锁并发更新、表结构迁移**串在一起，
所有结论都可以用单测和 demo 稳定复现。底层使用纯 Go 的 [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)，
**无需 CGO**。

## 目录结构

```
internal/domain/user.go           User 领域类型；AgeUnknown=0 的默认含义
internal/repository/errors.go     哨兵错误：找不到 / 版本冲突 / 非法游标 / 过期游标 / 迁移失败
internal/repository/cursor.go     不透明游标的编解码、TTL 与错误分类
internal/repository/migration.go  幂等迁移（schema_migrations 台账 + DDL 自身幂等）
internal/repository/repository.go 仓储：CRUD、游标分页、事务模板、乐观锁、审计同事务
internal/repository/*_test.go     并发更新 / 分页边界 / 回滚 / 迁移兼容测试
cmd/demo/main.go                  端到端可运行演示
```

## 快速验证

```bash
go test ./... -race -v       # 全量测试（带竞态检测），日志会打印版本号与游标变化
go run ./cmd/demo            # 端到端演示：分页 churn / 并发更新 / 回滚 / 迁移老数据
```

## 事务边界

- 所有写操作都通过 `inTx` 模板执行：`BeginTx` → 业务回调 → `Commit`；
  回调返回错误或发生 panic 时一律 `Rollback`，**绝不允许只写一半的数据落库**。
- 迁移也是“一个版本一个事务”：DDL 与台账登记在同一事务内提交，失败整体回滚，
  不会出现“表改了但版本没记”的中间态。
- 连接池 `SetMaxOpenConns(1)` 且 DSN 带 `_txlock=immediate`：SQLite 同一时刻只允许一个
  writer，写事务在驱动层串行排队；业务层的“谁的更新生效”完全由乐观锁条件决定，
  而不是靠调用顺序。

`UpdateWithAudit` 演示多语句事务：用户更新与审计插入同生共死。审计表
`note` 列带 `CHECK(length(note) > 0)`，传入空 note 会在第二语句确定性失败，
此时第一语句的用户更新也一并回滚（版本号不前进、审计表无残留），由
`TestUpdateWithAuditRollback` 断言。

## 乐观锁规则

- `users` 表有单调递增的整数列 `version`，新行 `version = 1`。
- 更新必须携带调用方读到的版本号，SQL 为条件更新：

  ```sql
  UPDATE users SET ..., version = version + 1, updated_at = ?
   WHERE id = ? AND version = ?   -- ? = 调用方期望的版本
  ```

- 受影响行数为 0 时再查一次当前状态，把失败**明确分成两类**：
  - 行已不存在 → `ErrNotFound`
  - 行还在但版本已前进 → `ErrVersionConflict`（错误信息附带期望版本与当前版本）
- 一次成功的更新恰好让 `version + 1`；冲突的更新不改任何数据、不推进版本。
  因此并发写同一行时**永远只有一个请求成功，输家的数据不可能覆盖赢家**（no lost update）。
- 冲突方的正确处理是：重新 `Get` 最新版本（必要时合并字段），带新版本号重试。

并发结论由两个测试复现：

| 测试 | 场景 | 结论 |
|---|---|---|
| `TestConcurrentUpdateExactlyOneWinner` | 16 个 goroutine 同读 v1 同时提交 | 成功 1、冲突 15、最终 v2 且数据属于唯一赢家 |
| `TestConcurrentUpdateOnlyOneCanAdvanceEachVersion` | 8 个 worker 自旋“读最新版→抢写” | 每次版本前进一步只对应一个成功者，age 累加次数 == 成功次数 |

## 分页语义（keyset / 游标分页）

- 排序键固定为 `(created_at ASC, id ASC)`，二者组成唯一全序；不使用 `OFFSET`。
- 游标是不透明字符串：`base64(JSON{v,ts,id,iat})`，记录上一页最后一行的两个排序键。
  下一页查询：

  ```sql
  WHERE created_at > ? OR (created_at = ? AND id > ?)
  ORDER BY created_at ASC, id ASC LIMIT ?
  ```

- **翻页期间的数据变动**（由 `TestPaginationStableUnderChurn` 复现）：
  - 删除**尚未读到**的行：后续页直接看不到它，不漏不重。
  - 删除**锚点行本身**：比较针对的是新行自身的键值，条件依然成立，不报错、不跳行。
  - 插入排序位置在**锚点之后**的新行：本次遍历的后续页会读到它（不会漏）。
  - 插入排序位置在**锚点之前**的迟到行：本次遍历读不到——这是 keyset 的明确定义
    （遍历是“锚点之后”的移动窗口，不是时间点快照）；从第一页重新开始即可看到。
  - 任何行都不会在不同页重复出现。
- 取满 `limit` 行才返回下一页游标，否则返回空游标表示遍历结束。

### 游标错误的两个类别（可区分）

| 类别 | 触发条件 | 哨兵错误 |
|---|---|---|
| 非法游标 | 非 base64、JSON 解析失败、格式版本 `v` 不支持、字段越界（id≤0 等）、签发时间在未来 | `ErrInvalidCursor` |
| 过期游标 | 游标可解析，但距 `iat` 超过 `cursorTTL`（5 分钟） | `ErrExpiredCursor` |

调用方用 `errors.Is` 区分：非法游标说明客户端传错/伪造，过期游标说明遍历放太久，
应回第一页重新开始。空串永远合法，表示第一页。过期行为通过测试注入时钟复现
（`TestListCursorErrorClassification`），无需 sleep。

## 表结构迁移与兼容读取

迁移台账表：

```sql
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL
);
```

| 版本 | 内容 |
|---|---|
| v1 | `users` 基础表（id/name/email/version/created_at/updated_at）+ email 唯一索引 + 分页复合索引 |
| v2 | 新增 `users.age INTEGER NOT NULL DEFAULT 0`；新建 `users_audit` 审计表 |

规则：

1. **幂等**：每个版本只在台账中登记一次，已登记则跳过；`ADD COLUMN` 额外先做
   `PRAGMA table_info` 列存在性检查，`CREATE` 全部带 `IF NOT EXISTS`。
   整个迁移过程重复执行无副作用（`TestMigrationLegacyRowsReadableAfterV2` 连续跑两遍）。
2. **新字段有稳定默认含义**：`age` 为 `NOT NULL DEFAULT 0`，`0 = domain.AgeUnknown`
   表示“未知/未填写”。SQLite 加列时为存量行直接填默认值，老数据在新结构下立即可读，
   无需回填脚本。
3. **不读串列**：行扫描统一走 `scanUser`，列清单 `id, name, email, age, version,
   created_at, updated_at` 与扫描顺序强绑定。
4. 测试支持把库**停在 v1**、灌入没有 age 列的老数据，再升级到 v2 验证读取结论
   （`ensureMigrationsUpTo`）。

## 错误处理约定

所有仓储错误都用 `fmt.Errorf("%w: 上下文", sentinel)` 包装，调用方一律用
`errors.Is` 判别类别，错误正文说明具体原因（哪个 id、期望版本/当前版本、游标什么问题）：

```go
switch {
case errors.Is(err, repository.ErrVersionConflict): // 重读最新版本后重试
case errors.Is(err, repository.ErrNotFound):        // 记录已被删除
case errors.Is(err, repository.ErrInvalidCursor):   // 游标伪造/损坏
case errors.Is(err, repository.ErrExpiredCursor):   // 回第一页重新遍历
}
```

## 测试清单

```bash
go test ./internal/repository -run TestConcurrent -v          # 并发更新（打印版本号变化）
go test ./internal/repository -run TestPagination -v          # 分页边界（打印每页游标）
go test ./internal/repository -run TestListCursor -v          # 非法/过期游标分类
go test ./internal/repository -run TestUpdateWithAudit -v     # 事务失败回滚
go test ./internal/repository -run TestMigration -v           # 迁移幂等与老数据兼容
go test ./... -race -count=3                                  # 重复 + 竞态检测
```

## 设计取舍备注

- 选 SQLite 是为了“本地数据库、零外部依赖、结论可复现”；乐观锁与游标分页的写法对
  PostgreSQL/MySQL 同样适用（条件 UPDATE 与 keyset WHERE 均为标准 SQL）。
- 单连接连接池是 SQLite 专用配置；换到客户端/服务端数据库时应去掉，由数据库自身的
  行锁配合本乐观锁工作。
