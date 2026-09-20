package fetcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRequirements(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantRoles   int
		wantCols    int
		wantErr     bool
		wantRoleVer string
	}{
		{
			name:        "wrapped roles map",
			input:       "roles:\n  - name: geerlingguy.nginx\n    version: 3.2.0\n",
			wantRoles:   1,
			wantRoleVer: "3.2.0",
		},
		{
			name:      "wrapped collections map",
			input:     "collections:\n  - name: community.general\n    version: 8.5.0\n",
			wantCols:  1,
			wantRoles: 0,
		},
		{
			name:        "bare role list",
			input:       "- name: geerlingguy.docker\n  version: 7.1.0\n",
			wantRoles:   1,
			wantRoleVer: "7.1.0",
		},
		{
			name:    "invalid yaml",
			input:   "roles: [this is not: valid",
			wantErr: true,
		},
		{
			name:      "empty document",
			input:     "",
			wantRoles: 0,
			wantCols:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs, err := ParseRequirements([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(reqs.Roles) != tt.wantRoles {
				t.Errorf("roles: got %d, want %d", len(reqs.Roles), tt.wantRoles)
			}
			if len(reqs.Collections) != tt.wantCols {
				t.Errorf("collections: got %d, want %d", len(reqs.Collections), tt.wantCols)
			}
			if tt.wantRoleVer != "" && reqs.Roles[0].Version != tt.wantRoleVer {
				t.Errorf("role version: got %q, want %q", reqs.Roles[0].Version, tt.wantRoleVer)
			}
		})
	}
}

func TestNewFetcherCreatesStorageTree(t *testing.T) {
	dir := t.TempDir()
	f := NewFetcher(dir, 0, ProxyConfig{})

	if f.maxConcurrency != 4 {
		t.Errorf("expected default maxConcurrency 4, got %d", f.maxConcurrency)
	}

	for _, sub := range []string{"manifests", "collections", "roles"} {
		if _, err := os.Stat(f.storagePath + "/" + sub); err != nil {
			t.Errorf("expected %s to exist: %v", sub, err)
		}
	}
}

func TestSaveManifestDeduplicatesByContent(t *testing.T) {
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{})
	data := []byte("roles:\n  - name: geerlingguy.nginx\n    version: 3.2.0\n")

	for i := 0; i < 3; i++ {
		if err := f.SaveManifest("roles", data); err != nil {
			t.Fatalf("SaveManifest failed: %v", err)
		}
	}

	files, err := os.ReadDir(f.manifestPath)
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected identical manifests to dedupe to 1 file, got %d", len(files))
	}
	if !strings.HasSuffix(files[0].Name(), "_requirements.yml") {
		t.Errorf("expected manifest name to end with _requirements.yml, got %s", files[0].Name())
	}

	// A distinct manifest must be stored alongside the first.
	other := []byte("roles:\n  - name: geerlingguy.docker\n    version: 7.1.0\n")
	if err := f.SaveManifest("roles", other); err != nil {
		t.Fatalf("SaveManifest failed: %v", err)
	}
	files, _ = os.ReadDir(f.manifestPath)
	if len(files) != 2 {
		t.Fatalf("expected 2 distinct manifests, got %d", len(files))
	}
}

func TestDirExistsNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if dirExistsNonEmpty(dir) {
		t.Error("empty directory reported as non-empty")
	}
	if dirExistsNonEmpty(filepath.Join(dir, "missing")) {
		t.Error("missing directory reported as non-empty")
	}

	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !dirExistsNonEmpty(sub) {
		t.Error("directory with content reported as empty")
	}
}

// TestSyncGitRepoSkipsExistingVersion proves the no-re-download guarantee
// without needing git: a version directory that already exists is skipped.
func TestSyncGitRepoSkipsExistingVersion(t *testing.T) {
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{})
	target := filepath.Join(f.storagePath, "roles", "geerlingguy.nginx", "3.3.1")
	if err := os.MkdirAll(target, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "content"), []byte("already mirrored"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := f.SyncGitRepo("https://example.invalid/repo.git", "3.3.1", target); err != nil {
		t.Fatalf("SyncGitRepo on cached version returned error: %v", err)
	}
}

// TestDownloadCollectionArtifactSkipsCached proves the no-re-download guarantee
// for galaxy collections: an existing artifact file is trusted and never fetched
// again, even with a fake galaxy host.
func TestDownloadCollectionArtifactSkipsCached(t *testing.T) {
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{})
	targetDir := filepath.Join(f.storagePath, "collections", "community")
	targetFile := filepath.Join(targetDir, "community-general-8.5.0.tar.gz")
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(targetFile, []byte("fake artifact"), 0640); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := f.downloadCollectionArtifact("community", "general", "8.5.0"); err != nil {
		t.Fatalf("downloadCollectionArtifact on cached file returned error: %v", err)
	}
}
