package fetcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"orbitron/internal/config"
)

// buildTaggedBareRepo creates a bare git repository offline advertising the
// given lightweight tags, used to exercise tag resolution without network.
func buildTaggedBareRepo(t *testing.T, tags []string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run(work, "init", "-q")
	run(work, "config", "user.email", "test@example.invalid")
	run(work, "config", "user.name", "orbitron test")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "one")
	for _, tg := range tags {
		run(work, "tag", tg)
	}
	bare := filepath.Join(t.TempDir(), "repo.git")
	run(work, "clone", "-q", "--bare", work, bare)
	return bare
}

func TestRoleVersionStringPrefersTagName(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"v1.0.1", "1.0.1", "v1.0.1"},
		{"3.3.1", "3.3.1", "3.3.1"},
		{"", "1.0.1", "1.0.1"},
		{"v1.0.1", "", "v1.0.1"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := roleVersionString(tt.name, tt.version); got != tt.want {
			t.Errorf("roleVersionString(%q, %q) = %q, want %q", tt.name, tt.version, got, tt.want)
		}
	}
}

func TestResolveGitTag(t *testing.T) {
	vRepo := buildTaggedBareRepo(t, []string{"v1.0.0", "v1.0.1"})
	plainRepo := buildTaggedBareRepo(t, []string{"3.3.0", "3.3.1"})

	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})

	tests := []struct {
		repo    string
		version string
		want    string
		wantErr bool
	}{
		{vRepo, "1.0.1", "v1.0.1", false},
		{vRepo, "v1.0.1", "v1.0.1", false},
		{vRepo, "", "", false},
		{vRepo, "2.0.0", "", true},
		{plainRepo, "3.3.0", "3.3.0", false},
		{plainRepo, "v3.3.0", "3.3.0", false},
		{plainRepo, "9.9.9", "", true},
	}

	for _, tt := range tests {
		got, err := f.resolveGitTag(tt.repo, tt.version)
		if tt.wantErr {
			if err == nil {
				t.Errorf("resolveGitTag(%s, %q): expected error, got %q", tt.repo, tt.version, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveGitTag(%s, %q): unexpected error: %v", tt.repo, tt.version, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolveGitTag(%s, %q) = %q, want %q", tt.repo, tt.version, got, tt.want)
		}
	}
}

func TestSyncGitRepoClonesResolvedTag(t *testing.T) {
	repo := buildTaggedBareRepo(t, []string{"v1.0.1"})
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})

	target := filepath.Join(f.storagePath, "roles", "chrisvanmeer.containerlab", "v1.0.1")
	if err := f.SyncGitRepo(repo, "1.0.1", target); err != nil {
		t.Fatalf("SyncGitRepo with v-prefixed tag failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "f")); err != nil {
		t.Fatalf("cloned tag content missing: %v", err)
	}

	// Re-running the same version on an existing checkout must keep working.
	if err := f.SyncGitRepo(repo, "1.0.1", target); err != nil {
		t.Fatalf("SyncGitRepo on existing checkout failed: %v", err)
	}
}

func TestMirrorAllRoleVersionsSkipsMissingTags(t *testing.T) {
	repo := buildTaggedBareRepo(t, []string{"v1.0.0", "v1.0.1"})
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})

	published := []string{"v1.0.0", "9.9.9", "v1.0.1"}
	if err := f.mirrorAllRoleVersions(repo, "chrisvanmeer.containerlab", published); err != nil {
		t.Fatalf("mirrorAllRoleVersions with one stale version should still succeed: %v", err)
	}

	for _, want := range []string{"v1.0.0", "v1.0.1"} {
		if _, err := os.Stat(filepath.Join(f.storagePath, "roles", "chrisvanmeer.containerlab", want)); err != nil {
			t.Errorf("expected version %s to be mirrored: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.storagePath, "roles", "chrisvanmeer.containerlab", "9.9.9")); err == nil {
		t.Error("stale version should have been skipped")
	}

	if err := f.mirrorAllRoleVersions(repo, "nothing.matching", []string{"9.9.9"}); err == nil {
		t.Error("expected error when no published version can be mirrored")
	}
}

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
	f := NewFetcher(dir, 0, ProxyConfig{}, config.TLSConfig{})

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
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})
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
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})
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
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})
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
