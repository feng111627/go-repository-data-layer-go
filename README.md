# go-repository-data-layer-go

基于本地 SQLite（`modernc.org/sqlite`，纯 Go 无 cgo）的数据访问层，提供：

- 增删改查 + 游标（keyset）分页
- 事务内乐观锁更新，并发写同一条记录只有一个成功
- 版本化 schema 迁移，幂等且兼容读取老数据

代码在 `store/` 包：`db.go`（连接）、`migrate.go`（迁移）、`repository.go`（仓储）、`cursor.go`（游标）、`errors.go`（错误分类）、`models.go`（模型）。

## 事务边界

- `Repository.Update` 内部自动开启一个事务：更新 `items` 行与追加 `item_revisions` 审计行**在同一事务**中，要么都提交要么都回滚，不会留下写了一半的数据。
- 需要多步写操作保持原子性时，使用 `InTransaction`：

  ```go
  err := repo.InTransaction(ctx, func(txRepo *store.Repository) error {
      if _, err := txRepo.Create(ctx, a); err != nil { return err }
      if _, err := txRepo.Update(ctx, b); err != nil { return err }
      return nil // 返回 error 则整体回滚
  })
  ```

  `fn` 返回 `nil` 才提交；返回任何 `error` 即整体回滚。事务内的 `Repository` 上再调 `InTransaction` 会直接报错（不支持嵌套，避免连接池死锁）；`Update` 在事务内被调用时自动复用外层事务。

## 乐观锁规则

- 每条记录带 `version` 列，创建时为 1，每次成功更新 +1。
- 更新必须带上读到的版本号：`UPDATE ... WHERE id = ? AND version = ?`。
- 影响行数为 0 时再查一次以区分两类错误：
  - 记录不存在 → `ErrNotFound`
  - 记录存在但版本不一致 → `ErrVersionConflict`（别人的改动绝不会被覆盖）
- 错误可用 `errors.Is` 区分。冲突方应重新读取最新记录后重试。

## 分页语义

- `List(ctx, cursor, limit)` 按 `id` 升序做 **keyset 分页**（`WHERE id > lastID ORDER BY id LIMIT limit+1`），不用 OFFSET。
- 翻页期间的并发增删不会导致重复或遗漏：
  - 新插入的记录 `id` 更大，只会出现在后续页，不会让已翻过的记录重复出现；
  - 已删除的记录天然被跳过；已翻过的页不会被重查。
- 游标是不透明字符串（`base64url("sv<schemaVersion>:<lastID>")`），调用方不应解析。
- 游标错误分两类，均可 `errors.Is` 判断：
  - `ErrCursorMalformed`：无法解码或格式非法；
  - `ErrCursorExpired`：游标签发时的 schema 版本落后于当前版本（表结构迁移后旧游标一律拒绝，避免按旧结构翻页）。

## 迁移与兼容读取

- 迁移按版本号顺序执行（v1 建表 → v2 加 `status` 列 → v3 建审计表），每步在事务内执行并记录到 `schema_migrations`；已应用的版本跳过，**重复执行幂等**。
- 新字段 `status` 的稳定默认含义是 `'active'`：`ALTER TABLE ... ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`，老数据无需回填即可读出确定值。
- 读取使用显式列清单（不用 `SELECT *`），表结构变更后不会读串列。

## 如何验证

```bash
go test ./... -race -count=1 -v
```

重点用例（`-v` 下会打印版本号与游标变化）：

| 测试 | 验证点 |
|---|---|
| `TestConcurrentUpdatesOnlyOneWins` | 16 个 goroutine 并发改同一条记录，恰好 1 个成功、15 个拿到 `ErrVersionConflict`，最终版本号只 +1 |
| `TestUpdateOptimisticLock` | 旧版本号更新被拒且不会覆盖已写入数据；`ErrNotFound` 与 `ErrVersionConflict` 可区分 |
| `TestTransactionRollbackLeavesNoPartialData` | 事务中途失败回滚后，新插入的行、已修改的行、审计行都不留痕 |
| `TestPaginationCoversAllExactlyOnce` | 25 条记录翻 3 页，每条恰好返回一次且按 id 升序 |
| `TestPaginationWithConcurrentInsert/Delete` | 翻页中途插入/删除，不重复、不遗漏 |
| `TestCursorErrorsAreClassified` | 非法游标 → `ErrCursorMalformed`，过期游标 → `ErrCursorExpired`，互不混淆 |
| `TestMigrationIsIdempotent` | 重复打开同一库文件，迁移不重复应用、数据不丢 |
| `TestMigrationFromV1KeepsOldDataReadable` | 手工构造 v1 旧库，迁移后老数据读出正确、`status` 为默认值 `'active'` |
