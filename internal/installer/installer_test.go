package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileReplacesExisting(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(src, []byte("new-binary"), 0755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old-content"), 0644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := copyFile(src, dst, 0755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new-binary" {
		t.Errorf("dst content = %q, want %q", got, "new-binary")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("dst mode = %v, want 0755", info.Mode().Perm())
	}

	matches, err := filepath.Glob(filepath.Join(dir, "dst.tmp-*"))
	if err != nil {
		t.Fatalf("glob for temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}
}

func TestCopyFileCreatesMissingDestination(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	dst := filepath.Join(filepath.Join(dir, "nested"), "deep", "dst")

	if err := os.WriteFile(src, []byte("payload"), 0755); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// The parent directory must exist for the atomic temp+rename to apply.
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := copyFile(src, dst, 0750); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("dst content = %q, want %q", got, "payload")
	}
}