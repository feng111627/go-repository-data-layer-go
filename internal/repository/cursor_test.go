package repository

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	raw, err := encodeCursor(cursor{CreatedAt: 123456, ID: 7, IssuedAt: now.UnixNano()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	c, first, err := decodeCursor(raw, now)
	if err != nil || first {
		t.Fatalf("decode: first=%v err=%v", first, err)
	}
	if c.ID != 7 || c.CreatedAt != 123456 || c.Ver != 1 {
		t.Fatalf("decoded mismatch: %+v", c)
	}
}

func TestDecodeCursorFirstPage(t *testing.T) {
	_, first, err := decodeCursor("", time.Now())
	if err != nil || !first {
		t.Fatalf("empty cursor must mean first page, got first=%v err=%v", first, err)
	}
}

func TestDecodeCursorInvalid(t *testing.T) {
	now := time.Now()
	cases := map[string]string{
		"not base64":       "!!!not-base64!!!",
		"base64 not json":  base64.URLEncoding.EncodeToString([]byte("{xxx")),
		"wrong version":    encodeRaw(t, 9, 1, 1, now.UnixNano()),
		"zero id":          mustEncode(t, cursor{Ver: 1, ID: 0, IssuedAt: now.UnixNano(), CreatedAt: 1}),
		"negative created": mustEncode(t, cursor{Ver: 1, ID: 1, IssuedAt: now.UnixNano(), CreatedAt: -1}),
		"issued in future": mustEncode(t, cursor{Ver: 1, ID: 1, IssuedAt: now.Add(time.Hour).UnixNano(), CreatedAt: 1}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeCursor(raw, now)
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("want ErrInvalidCursor, got %v", err)
			}
			if errors.Is(err, ErrExpiredCursor) {
				t.Fatalf("invalid cursor must not also be classified as expired: %v", err)
			}
			if !strings.Contains(err.Error(), "invalid cursor") {
				t.Fatalf("error should explain the category, got %q", err.Error())
			}
		})
	}
}

func TestDecodeCursorExpired(t *testing.T) {
	now := time.Now()
	issued := now.Add(-(cursorTTL + time.Minute))
	raw := mustEncode(t, cursor{Ver: 1, ID: 42, CreatedAt: 999, IssuedAt: issued.UnixNano()})
	_, _, err := decodeCursor(raw, now)
	if !errors.Is(err, ErrExpiredCursor) {
		t.Fatalf("want ErrExpiredCursor, got %v", err)
	}
	if errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expired cursor must not be classified invalid: %v", err)
	}
	t.Logf("expired error: %v", err)
}

func TestDecodeCursorWithinTTL(t *testing.T) {
	now := time.Now()
	issued := now.Add(-(cursorTTL - time.Second))
	raw := mustEncode(t, cursor{Ver: 1, ID: 42, CreatedAt: 999, IssuedAt: issued.UnixNano()})
	if _, _, err := decodeCursor(raw, now); err != nil {
		t.Fatalf("cursor just inside TTL must be accepted: %v", err)
	}
}

func mustEncode(t *testing.T, c cursor) string {
	t.Helper()
	raw, err := encodeCursor(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// encodeRaw 手工构造任意 Ver 的游标串，绕过 encodeCursor 的版本归一化。
func encodeRaw(t *testing.T, ver int, id int64, createdAt int64, iat int64) string {
	t.Helper()
	c := cursor{Ver: ver, ID: id, CreatedAt: createdAt, IssuedAt: iat}
	raw, err := encodeCursor(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if ver == cursorFormatVersion {
		return raw
	}
	jsonStr := fmt.Appendf(nil, `{"v":%d,"ts":%d,"id":%d,"iat":%d}`, ver, createdAt, id, iat)
	return base64.URLEncoding.EncodeToString(jsonStr)
}
