package domain

import "time"

// User 是 users 表对应的领域对象。
// Age 为 v2 迁移引入的字段：零值 0（AgeUnknown）表示“未知/未填写”。
type User struct {
	ID        int64
	Name      string
	Email     string
	Age       int
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AgeUnknown 是 Age 列稳定且明确的默认含义：老数据迁移后年龄未知。
const AgeUnknown = 0
