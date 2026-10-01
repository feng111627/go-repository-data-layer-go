package store

import "errors"

// 错误分类：调用方可以用 errors.Is 区分。
var (
	// ErrNotFound 记录不存在。
	ErrNotFound = errors.New("store: record not found")
	// ErrVersionConflict 乐观锁版本冲突：别人已经改过这条记录。
	ErrVersionConflict = errors.New("store: version conflict")
	// ErrCursorMalformed 游标格式非法（无法解码或字段不对）。
	ErrCursorMalformed = errors.New("store: cursor malformed")
	// ErrCursorExpired 游标已过期（签发时的 schema 版本与当前不一致）。
	ErrCursorExpired = errors.New("store: cursor expired")
)
