package repository

import "errors"

// 可被 errors.Is 判别的哨兵错误。仓储层所有失败都归为明确的一类，
// 调用方据此区分“没找到 / 版本冲突 / 游标非法 / 游标过期 / 迁移失败”。
var (
	// ErrNotFound 目标记录不存在（可能已被删除）。
	ErrNotFound = errors.New("repository: record not found")
	// ErrVersionConflict 乐观锁冲突：提交版本与库中当前版本不一致。
	ErrVersionConflict = errors.New("repository: version conflict")
	// ErrInvalidCursor 游标格式非法：无法解码、字段缺失或游标版本不支持。
	ErrInvalidCursor = errors.New("repository: invalid cursor")
	// ErrExpiredCursor 游标合法但已超过有效期，调用方应改用首页重新开始。
	ErrExpiredCursor = errors.New("repository: expired cursor")
	// ErrMigration 迁移执行失败或校验不通过。
	ErrMigration = errors.New("repository: migration failed")
)
