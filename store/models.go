package store

import "time"

// Item 是仓储管理的记录。Version 用于乐观锁，Status 是迁移 v2 新增字段。
type Item struct {
	ID        int64
	Name      string
	Value     int64
	Status    string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Page 是一页结果。
type Page struct {
	Items      []Item
	NextCursor string // 空表示没有下一页
}
