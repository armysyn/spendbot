package accounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := Open(ctx, filepath.Join(dir, "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	if ok, err := r.Bootstrap(ctx, filepath.Join(dir, "spend.db"), "Me", "hash1", now); !ok || err != nil {
		t.Fatalf("bootstrap: %v %v", ok, err)
	}
	if ok, _ := r.Bootstrap(ctx, "other.db", "Again", "", now); ok {
		t.Error("bootstrap runs once")
	}
	first, _ := r.Get(ctx, 1)
	if !first.Primary() || first.Hash != "hash1" || first.Name != "Me" {
		t.Fatalf("first: %+v", first)
	}
	second, err := r.Create(ctx, "Anna", "hash2", now)
	if err != nil || second.ID != 2 || filepath.Base(second.DBPath) != "account-2.db" || filepath.Dir(second.DBPath) != filepath.Join(dir, "accounts") {
		t.Fatalf("create: %+v %v", second, err)
	}
	r.SetHolder(ctx, 2, "Anna A. B.")
	r.SetName(ctx, 2, "Anna B.")
	if a, _ := r.Get(ctx, 2); a.Holder != "Anna A. B." || a.Name != "Anna B." {
		t.Errorf("updates: %+v", a)
	}

	r.CreateSession(ctx, "s1", 2, "test", now, now.Add(time.Hour))
	if acc, ok, _ := r.SessionAccount(ctx, "s1", now); !ok || acc != 2 {
		t.Errorf("session: %d %v", acc, ok)
	}
	if _, ok, _ := r.SessionAccount(ctx, "s1", now.Add(2*time.Hour)); ok {
		t.Error("expired session")
	}
	// deleting moves the data aside and signs out
	os.WriteFile(second.DBPath, []byte("x"), 0o600)
	kept, err := r.Delete(ctx, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("data kept aside: %v", err)
	}
	if _, ok, _ := r.SessionAccount(ctx, "s1", now); ok {
		t.Error("sessions of a deleted account")
	}
	if _, err := r.Delete(ctx, 1, now); err == nil {
		t.Error("the first account stays")
	}
}

func TestSameHolder(t *testing.T) {
	if !SameHolder("Иванов  Иван Иванович", "ИВАНОВ иван иванович") || !SameHolder("Семёнов С.", "Семенов С.") {
		t.Error("same")
	}
	if SameHolder("Иванов Иван", "Петров Пётр") {
		t.Error("different")
	}
}
