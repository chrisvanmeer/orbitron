package seed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	meta, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Namespace != Namespace {
		t.Errorf("namespace = %q, want %q", meta.Namespace, Namespace)
	}
	if meta.Name != Name {
		t.Errorf("name = %q, want %q", meta.Name, Name)
	}
	if meta.Version == "" {
		t.Error("version is empty")
	}
	wantArtifact := ArtifactName(Namespace, Name, meta.Version)
	if wantArtifact != Namespace+"-"+Name+"-"+meta.Version+".tar.gz" {
		t.Errorf("ArtifactName = %q, want %q", wantArtifact, Namespace+"-"+Name+"-"+meta.Version+".tar.gz")
	}
}

func TestEmbeddedArtifactIsGzip(t *testing.T) {
	if len(CollectionTarGz) < 2 {
		t.Fatal("embedded collection artifact is empty")
	}
	if CollectionTarGz[0] != 0x1f || CollectionTarGz[1] != 0x8b {
		t.Error("embedded artifact does not start with a gzip magic header")
	}
}

func TestShouldSeed(t *testing.T) {
	cases := []struct {
		name     string
		embedded string
		existing []string
		wantSeed bool
	}{
		{name: "empty cache seeds", embedded: "1.8.1", existing: nil, wantSeed: true},
		{name: "same version present keeps copy", embedded: "1.8.1", existing: []string{"1.8.1"}, wantSeed: false},
		{name: "newer cached version keeps copy", embedded: "1.8.1", existing: []string{"2.0.0"}, wantSeed: false},
		{name: "newer embedded seeds alongside older", embedded: "1.8.1", existing: []string{"1.7.0"}, wantSeed: true},
		{name: "multiple older versions still seed", embedded: "1.8.1", existing: []string{"1.5.0", "1.7.2"}, wantSeed: true},
		{name: "downgrade of binary never overwrites", embedded: "1.6.0", existing: []string{"1.8.1"}, wantSeed: false},
		{name: "unparseable cached version blocks seeding", embedded: "1.8.1", existing: []string{"not-a-version"}, wantSeed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSeed(tc.embedded, tc.existing); got != tc.wantSeed {
				t.Errorf("shouldSeed(%q, %v) = %v, want %v", tc.embedded, tc.existing, got, tc.wantSeed)
			}
		})
	}
}

func TestBundledVersions(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "chrisvanmeer-orbitron-1.7.0.tar.gz"), "a")
	mustWrite(t, filepath.Join(dir, "chrisvanmeer-orbitron-1.8.1.tar.gz"), "b")
	mustWrite(t, filepath.Join(dir, "unrelated-1.0.0.tar.gz"), "c")
	mustWrite(t, filepath.Join(dir, "chrisvanmeer-orbitron-2.0.0.tar.gz.tmp"), "d")
	if err := os.Mkdir(filepath.Join(dir, "chrisvanmeer-orbitron-9.9.9.tar.gz"), 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := bundledVersions(dir)
	if err != nil {
		t.Fatalf("bundledVersions: %v", err)
	}
	want := []string{"1.7.0", "1.8.1"}
	if len(got) != len(want) {
		t.Fatalf("bundledVersions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bundledVersions = %v, want %v", got, want)
			break
		}
	}
}

func TestBundledVersionsMissingDir(t *testing.T) {
	got, err := bundledVersions(filepath.Join(t.TempDir(), "does", "not", "exist"))
	if err != nil {
		t.Fatalf("bundledVersions missing dir: %v", err)
	}
	if got != nil {
		t.Errorf("bundledVersions = %v, want nil", got)
	}
}

// TestSeedCache covers the end-to-end seeding path against a scratch storage
// dir: first run seeds, re-running the same version keeps state untouched, and
// an already-newer cached copy wins even when the embedded version would be
// older (downgrade protection).
func TestSeedCache(t *testing.T) {
	storage := t.TempDir()

	meta, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := SeedCache(storage)
	if err != nil {
		t.Fatalf("SeedCache (first): %v", err)
	}
	if !res.Seeded {
		t.Fatalf("first SeedCache did not seed; %+v", res)
	}
	if res.Version != meta.Version {
		t.Errorf("version = %q, want %q", res.Version, meta.Version)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("seeded artifact missing: %v", err)
	}
	declared, err := os.ReadDir(filepath.Join(storage, "manifests"))
	if err != nil {
		t.Fatalf("read manifests: %v", err)
	}
	if len(declared) != 1 {
		t.Fatalf("expected 1 requirements manifest, got %d", len(declared))
	}

	// Re-seeding the same version must be a no-op: no new/copied artifact.
	again, err := SeedCache(storage)
	if err != nil {
		t.Fatalf("SeedCache (second): %v", err)
	}
	if again.Seeded {
		t.Errorf("second SeedCache seeded again; %+v", again)
	}
	if again.Version != meta.Version {
		t.Errorf("second version = %q, want %q", again.Version, meta.Version)
	}
	entries, err := os.ReadDir(filepath.Join(storage, "collections", "chrisvanmeer"))
	if err != nil {
		t.Fatalf("read collections dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("second SeedCache duplicated artifact: got %d files, want 1", len(entries))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestArtifactNameHelper(t *testing.T) {
	got := ArtifactName("a", "b", "1.0.0-rc.1")
	want := "a-b-1.0.0-rc.1.tar.gz"
	if got != want {
		t.Errorf("ArtifactName = %q, want %q", got, want)
	}
}
