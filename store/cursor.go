package store

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// 游标格式：base64url("sv<schemaVersion>:<lastID>")。
// 不透明字符串，调用方不应解析。schemaVersion 用于识别"过期"：
// 表结构迁移后旧游标一律拒绝，避免按旧结构翻页读串。

// encodeCursor 生成不透明游标。
func encodeCursor(lastID int64, schemaVersion int) string {
	raw := fmt.Sprintf("sv%d:%d", schemaVersion, lastID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 解码并校验游标：
//   - 无法 base64 解码、格式不符、ID 非数字 → ErrCursorMalformed
//   - 签发时的 schema 版本落后于当前 → ErrCursorExpired
func decodeCursor(s string, schemaVersion int) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("%w: not valid base64url", ErrCursorMalformed)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "sv") {
		return 0, fmt.Errorf("%w: unexpected format", ErrCursorMalformed)
	}
	sv, err := strconv.Atoi(strings.TrimPrefix(parts[0], "sv"))
	if err != nil {
		return 0, fmt.Errorf("%w: bad schema version", ErrCursorMalformed)
	}
	lastID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad last id", ErrCursorMalformed)
	}
	if sv != schemaVersion {
		return 0, fmt.Errorf("%w: issued at schema v%d, current v%d", ErrCursorExpired, sv, schemaVersion)
	}
	return lastID, nil
}
