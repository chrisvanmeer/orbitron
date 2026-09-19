package pruner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"orbitron/internal/access"
)

// seedAccess writes a synthetic access index into <storage>/.access.json so
// tests can exercise the pruner against both freshly-accessed and stale
// role/collection versions without depending on wall-clock timing.
func seedAccess(t *testing.T, storage string, entries []access.Entry) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, ".access.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRunPruneDryRunReportsWithoutDeleting(t *testing.T) {
	storage := t.TempDir()
	activeDir := filepath.Join(storage, "roles", "foo.role", "1.0.0")
	orphanDir := filepath.Join(storage, "roles", "foo.role", "9.9.9")
	for _, d := range []string{activeDir, orphanDir} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}

	seedAccess(t, storage, []access.Entry{
		{Key: access.RoleKey("foo.role", "1.0.0"), LastAccess: time.Now()},
		{Key: access.RoleKey("foo.role", "9.9.9"), LastAccess: time.Now().Add(-400 * 24 * time.Hour)},
	})

	p := NewPruner(storage)
	items, err := p.RunPruneDryRun(90)
	if err != nil {
		t.Fatalf("RunPruneDryRun: %v", err)
	}
	if len(items) != 1 || items[0] != orphanDir {
		t.Fatalf("expected exactly orphan dir %s, got %v", orphanDir, items)
	}

	// A dry run must never touch the filesystem.
	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("dry run must not delete: %v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("freshly accessed version must be preserved: %v", err)
	}
}

func TestRunPruneAPIDeletesUnreferenced(t *testing.T) {
	storage := t.TempDir()
	activeDir := filepath.Join(storage, "roles", "foo.role", "1.0.0")
	orphanDir := filepath.Join(storage, "roles", "foo.role", "9.9.9")
	for _, d := range []string{activeDir, orphanDir} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}

	seedAccess(t, storage, []access.Entry{
		{Key: access.RoleKey("foo.role", "1.0.0"), LastAccess: time.Now()},
		{Key: access.RoleKey("foo.role", "9.9.9"), LastAccess: time.Now().Add(-400 * 24 * time.Hour)},
	})

	p := NewPruner(storage)
	result, err := p.RunPruneAPI(false, 90)
	if err != nil {
		t.Fatalf("RunPruneAPI: %v", err)
	}
	if !result.Executed {
		t.Error("Executed should be true for a non-dry-run invocation")
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 pruned item, got %d", len(result.Items))
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan should be deleted, got err=%v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("active version must be preserved: %v", err)
	}
}

func TestRunPruneAPIDryRunSkippedDelete(t *testing.T) {
	storage := t.TempDir()
	activeDir := filepath.Join(storage, "roles", "foo.role", "1.0.0")
	orphanDir := filepath.Join(storage, "roles", "foo.role", "9.9.9")
	for _, d := range []string{activeDir, orphanDir} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}

	seedAccess(t, storage, []access.Entry{
		{Key: access.RoleKey("foo.role", "1.0.0"), LastAccess: time.Now()},
		{Key: access.RoleKey("foo.role", "9.9.9"), LastAccess: time.Now().Add(-400 * 24 * time.Hour)},
	})

	p := NewPruner(storage)
	result, err := p.RunPruneAPI(true, 90)
	if err != nil {
		t.Fatalf("RunPruneAPI(dry): %v", err)
	}
	if result.Executed {
		t.Error("Executed should be false for a dry run")
	}
	if result.FreedBytes != 0 {
		t.Errorf("dry run should not report freed bytes, got %d", result.FreedBytes)
	}
	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("dry run must not delete the orphan: %v", err)
	}
}

func TestRunPruneOrphanDetectionRespectsDecline(t *testing.T) {
	storage := t.TempDir()
	activeDir := filepath.Join(storage, "roles", "foo.role", "1.0.0")
	orphanDir := filepath.Join(storage, "roles", "foo.role", "9.9.9")
	for _, d := range []string{activeDir, orphanDir} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}

	seedAccess(t, storage, []access.Entry{
		{Key: access.RoleKey("foo.role", "1.0.0"), LastAccess: time.Now()},
		{Key: access.RoleKey("foo.role", "9.9.9"), LastAccess: time.Now().Add(-400 * 24 * time.Hour)},
	})

	// Answer "n" to the confirmation prompt.
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	p := NewPruner(storage)
	if err := p.RunPrune(90); err != nil {
		t.Fatalf("RunPrune returned error: %v", err)
	}

	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("orphan directory should remain when prune is declined: %v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("active directory must never be removed: %v", err)
	}
}

func TestParseCollectionArtifact(t *testing.T) {
	tests := []struct {
		namespace string
		filename  string
		wantName  string
		wantVer   string
		wantOK    bool
	}{
		{"community", "community-general-8.5.0.tar.gz", "general", "8.5.0", true},
		{"community", "community-general.tar.gz", "", "", false},
		{"community", "other-general-1.0.0.tar.gz", "", "", false},
		{"community", "community-general-8.5.0.zip", "", "", false},
		{"foo", "foo-bar-baz-1.0.0.tar.gz", "bar-baz", "1.0.0", true},
	}

	for _, tt := range tests {
		name, ver, ok := parseCollectionArtifact(tt.namespace, tt.filename)
		if ok != tt.wantOK {
			t.Errorf("%s: ok=%v, want %v", tt.filename, ok, tt.wantOK)
			continue
		}
		if name != tt.wantName || ver != tt.wantVer {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tt.filename, name, ver, tt.wantName, tt.wantVer)
		}
	}
}
