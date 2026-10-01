package repository

// nowNanos 返回当前墙钟的 UnixNano，用于不参与业务断言的场景（迁移台账时间）。
func nowNanos() int64 {
	return nowFn().UnixNano()
}
