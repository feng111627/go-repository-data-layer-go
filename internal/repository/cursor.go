package repository

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// cursorTTL 是游标有效期。游标内嵌签发时间，过期后拒绝使用并返回 ErrExpiredCursor。
const cursorTTL = 5 * time.Minute

// cursorFormatVersion 是当前游标格式版本；解码到更高版本一律视为非法。
const cursorFormatVersion = 1

// cursor 是翻页游标的内部表示，线上传输使用不透明的 base64 字符串。
// 排序键固定为 (created_at ASC, id ASC)，游标记录上一页最后一行的这两个键。
type cursor struct {
	Ver       int   `json:"v"`   // 游标格式版本，当前为 1
	CreatedAt int64 `json:"ts"`  // 锚点行 created_at（UnixNano）
	ID        int64 `json:"id"`  // 锚点行 id
	IssuedAt  int64 `json:"iat"` // 签发时间（UnixNano），用于过期判定
}

// encodeCursor 把游标编码为不透明字符串（JSON 再做 URL-safe base64）。
func encodeCursor(c cursor) (string, error) {
	c.Ver = cursorFormatVersion
	raw, err := json.Marshal(c)
	if err != nil {
		// 实践中不可达：cursor 字段全部可序列化。
		return "", fmt.Errorf("%w: marshal cursor: %v", ErrInvalidCursor, err)
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

// decodeCursor 解析外部游标。
// 空串返回 (零值, true, nil)，true 表示“第一页、无锚点”；
// 无法 base64 解码 / JSON 解析 / 版本不支持，返回带说明的 ErrInvalidCursor；
// 游标可解析但超过 cursorTTL，返回带说明的 ErrExpiredCursor。
func decodeCursor(raw string, now time.Time) (cursor, bool, error) {
	if raw == "" {
		return cursor{}, true, nil
	}
	jsonBytes, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, false, fmt.Errorf("%w: base64 decode failed (%v)", ErrInvalidCursor, err)
	}
	var c cursor
	if err := json.Unmarshal(jsonBytes, &c); err != nil {
		return cursor{}, false, fmt.Errorf("%w: json parse failed (%v)", ErrInvalidCursor, err)
	}
	if c.Ver != cursorFormatVersion {
		return cursor{}, false, fmt.Errorf("%w: unsupported cursor version %d", ErrInvalidCursor, c.Ver)
	}
	if c.CreatedAt < 0 || c.ID <= 0 || c.IssuedAt <= 0 {
		return cursor{}, false, fmt.Errorf("%w: cursor fields out of range (ts=%d id=%d iat=%d)",
			ErrInvalidCursor, c.CreatedAt, c.ID, c.IssuedAt)
	}
	issued := time.Unix(0, c.IssuedAt)
	age := now.Sub(issued)
	if age < 0 {
		// 签发时间在未来，通常来自时钟错乱的客户端，同样拒绝。
		return cursor{}, false, fmt.Errorf("%w: cursor issued_at is in the future", ErrInvalidCursor)
	}
	if age > cursorTTL {
		return c, false, fmt.Errorf("%w: cursor age %s exceeds ttl %s", ErrExpiredCursor, age.Truncate(time.Second), cursorTTL)
	}
	return c, false, nil
}
