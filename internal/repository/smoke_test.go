package repository

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSmokeOpenAndCreate(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "smoke.db")
	r, err := NewUserRepository(context.Background(), db)
	if err != nil {
		t.Fatalf("open/migrate: %v", err)
	}
	defer r.Close()
	u, err := r.Create(context.Background(), "alice", "a@example.com", 30)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("created id=%d version=%d age=%d", u.ID, u.Version, u.Age)
}
