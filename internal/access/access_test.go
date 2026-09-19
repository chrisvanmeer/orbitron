package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTouchPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	rec := New(dir)

	k := RoleKey("geerlingguy.nginx", "3.3.1")
	rec.Touch(k)

	path := filepath.Join(dir, ".access.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected .access.json to be created on disk: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty .access.json")
	}

	reloaded := New(dir)
	got, ok := reloaded.LastAccessed(k)
	if !ok {
		t.Fatalf("expected key %q to be recorded after reload", k)
	}
	if time.Since(got) > time.Minute {
		t.Fatalf("expected fresh timestamp, got %v", got)
	}
}

func TestNewCreatesStorageDir(t *testing.T) {
	base := t.TempDir()
	storage := filepath.Join(base, "does", "not", "exist")

	rec := New(storage)
	rec.Touch(RoleKey("a.b", "1.0.0"))

	if _, err := os.Stat(filepath.Join(storage, ".access.json")); err != nil {
		t.Fatalf("expected recorder to create nested storage dir and write file: %v", err)
	}
}

func TestSnapshotExcludesFutureAndZero(t *testing.T) {
	dir := t.TempDir()
	rec := New(dir)
	rec.Touch(RoleKey("ok", "1.0.0"))

	entries := []Entry{
		{Key: RoleKey("future", "1.0.0"), LastAccess: time.Now().Add(24 * time.Hour)},
		{Key: RoleKey("zero", "1.0.0")},
	}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, ".access.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	reloaded := New(dir)
	snap := reloaded.Snapshot()
	if _, ok := snap[RoleKey("ok", "1.0.0")]; ok {
		t.Fatal("expected previously touched key to survive reload")
	}
	if _, ok := snap[RoleKey("future", "1.0.0")]; ok {
		t.Fatal("expected future-dated entry to be dropped")
	}
	if _, ok := snap[RoleKey("zero", "1.0.0")]; ok {
		t.Fatal("expected zero-time entry to be dropped")
	}
}
