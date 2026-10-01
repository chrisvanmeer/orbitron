package seed

import (
	"errors"
	"os"
	"path/filepath"

	"strings"
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

	res, err := SeedCache(storage, nil)
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
	again, err := SeedCache(storage, nil)
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

// TestSeedCacheAppliesOwner is the regression test for the defect that made every
// cache dump fail on a freshly installed host: the installer runs as root and
// seeded the requirements manifest, so os.WriteFile handed it to root, and only
// the tar.gz artifact was chowned to the service user afterwards. The manifest
// stayed root-owned and 0640, so the daemon could not open it — and the dump,
// the only code path that reads the whole tree, died halfway with "permission
// denied" and shipped a truncated archive.
//
// The assertion is on which paths the seed claims, not on the resulting uid:
// that is the actual defect (the manifest was missing from the chown set), and
// it is observable without the privileges to change a file's owner.
func TestSeedCacheAppliesOwner(t *testing.T) {
	storage := t.TempDir()
	claimed := chownRecorder(t, nil)

	first, err := SeedCache(storage, &Owner{UID: 4242, GID: 4242})
	if err != nil {
		t.Fatalf("SeedCache: %v", err)
	}
	if len(*claimed) == 0 {
		t.Fatal("seed claimed ownership of nothing")
	}
	// The manifest is the file the regression is about.
	if !wasClaimed(*claimed, first.ManifestPath) {
		t.Errorf("requirements manifest %s was not claimed by the seed; claimed: %v", first.ManifestPath, *claimed)
	}
	if !wasClaimed(*claimed, first.Path) {
		t.Errorf("artifact %s was not claimed by the seed; claimed: %v", first.Path, *claimed)
	}
	// The directory chain must be traversable, or a 0750 root-owned parent
	// blocks the service user from reaching any of it.
	for _, dir := range []string{
		storage,
		filepath.Join(storage, "collections"),
		filepath.Join(storage, "collections", "chrisvanmeer"),
		filepath.Join(storage, "manifests"),
	} {
		if !wasClaimed(*claimed, dir) {
			t.Errorf("directory %s was not claimed by the seed; claimed: %v", dir, *claimed)
		}
	}
	for path, ids := range *claimed {
		if ids != [2]int{4242, 4242} {
			t.Errorf("seed claimed %s with uid/gid %v, want 4242/4242", path, ids)
		}
	}

	// Re-seeding adopts the existing manifest and claims it again, so an
	// installer run on an already damaged cache repairs the ownership.
	repaired := chownRecorder(t, nil)
	second, err := SeedCache(storage, &Owner{UID: 7777, GID: 7777})
	if err != nil {
		t.Fatalf("SeedCache (second): %v", err)
	}
	if second.Seeded {
		t.Errorf("adopting seed re-seeded the artifact: %+v", second)
	}
	if !second.ManifestAdopted {
		t.Errorf("expected the existing manifest to be adopted, got %+v", second)
	}
	if !wasClaimed(*repaired, second.ManifestPath) {
		t.Errorf("adopting seed did not claim the existing manifest %s; claimed: %v", second.ManifestPath, *repaired)
	}
	if ids := (*repaired)[second.ManifestPath]; ids != [2]int{7777, 7777} {
		t.Errorf("manifest claimed with %v, want 7777/7777", ids)
	}
}

// TestSeedCacheNilOwnerLeavesOwnership verifies the daemon's startup seed, which
// already runs as the service user and passes no Owner, does not try to chown
// anything: doing so would fail on a tree it does not own.
func TestSeedCacheNilOwnerLeavesOwnership(t *testing.T) {
	storage := t.TempDir()
	claimed := chownRecorder(t, nil)

	if _, err := SeedCache(storage, nil); err != nil {
		t.Fatalf("SeedCache: %v", err)
	}
	if len(*claimed) != 0 {
		t.Errorf("seed without an owner claimed %v, want nothing", *claimed)
	}
}

// TestSeedCacheReportsChownFailure verifies a failed ownership change is surfaced
// instead of being silently swallowed, which is how a root-owned manifest could
// go unnoticed for an entire release cycle.
func TestSeedCacheReportsChownFailure(t *testing.T) {
	storage := t.TempDir()
	chownRecorder(t, func(string, int, int) error { return os.ErrPermission })

	_, err := SeedCache(storage, &Owner{UID: 1, GID: 1})
	if err == nil {
		t.Fatal("SeedCache reported success although setting ownership failed")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want it to wrap os.ErrPermission", err)
	}
	if !strings.Contains(err.Error(), storage) {
		t.Errorf("error %q should name the path whose ownership failed", err)
	}
}

// chownRecorder replaces the ownership change for the duration of the test,
// recording every path the seed asks to claim. delegate may be nil to skip the
// real chown, which would need privileges a test runner may not have.
func chownRecorder(t *testing.T, delegate func(string, int, int) error) *map[string][2]int {
	t.Helper()
	recorded := map[string][2]int{}
	prev := chown
	chown = func(path string, uid, gid int) error {
		recorded[path] = [2]int{uid, gid}
		if delegate != nil {
			return delegate(path, uid, gid)
		}
		return nil
	}
	t.Cleanup(func() { chown = prev })
	return &recorded
}

// wasClaimed reports whether path was passed to chown.
func wasClaimed(recorded map[string][2]int, path string) bool {
	_, ok := recorded[path]
	return ok
}

// TestSeedCacheRepairsMissingManifest covers the interrupted seed: the artifact
// is present, so shouldSeed skips, but the manifest never made it. Without the
// declare step the bundled collection would stay undeclared and a later prune
// could remove it, with no retry ever scheduled.
func TestSeedCacheRepairsMissingManifest(t *testing.T) {
	storage := t.TempDir()

	first, err := SeedCache(storage, nil)
	if err != nil {
		t.Fatalf("SeedCache (first): %v", err)
	}
	if !first.Seeded {
		t.Fatalf("first SeedCache did not seed; %+v", first)
	}
	if err := os.Remove(first.ManifestPath); err != nil {
		t.Fatalf("remove manifest to simulate an interrupted seed: %v", err)
	}

	second, err := SeedCache(storage, nil)
	if err != nil {
		t.Fatalf("SeedCache (repair): %v", err)
	}
	if second.Seeded {
		t.Errorf("repair re-seeded the artifact: %+v", second)
	}
	if second.ManifestAdopted {
		t.Errorf("manifest was reported as adopted although it had been removed: %+v", second)
	}
	if _, err := os.Stat(second.ManifestPath); err != nil {
		t.Fatalf("manifest was not restored: %v", err)
	}
}

// TestSeedCacheLeavesNewerCacheAlone verifies the downgrade guard still holds
// once the manifest declaration became unconditional for a cached bundled
// version: a cache holding only a newer version must not gain a manifest
// declaring a collection that is not in it.
func TestSeedCacheLeavesNewerCacheAlone(t *testing.T) {
	storage := t.TempDir()

	meta, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	colsDir := filepath.Join(storage, "collections", "chrisvanmeer")
	if err := os.MkdirAll(colsDir, 0750); err != nil {
		t.Fatalf("create collections dir: %v", err)
	}
	newer := "99.0.0"
	futurePath := filepath.Join(colsDir, ArtifactName(meta.Namespace, meta.Name, newer))
	if err := os.WriteFile(futurePath, []byte("newer"), 0640); err != nil {
		t.Fatalf("write newer artifact: %v", err)
	}

	res, err := SeedCache(storage, nil)
	if err != nil {
		t.Fatalf("SeedCache: %v", err)
	}
	if res.Seeded {
		t.Errorf("downgraded a newer cache: %+v", res)
	}
	if !res.Skipped {
		t.Errorf("expected Skipped, got %+v", res)
	}
	if res.ManifestPath != "" {
		t.Errorf("declared a manifest for a version that is not cached: %q", res.ManifestPath)
	}
	if _, err := os.Stat(filepath.Join(storage, "manifests")); err == nil {
		t.Error("a manifests dir was created for a cache that holds no bundled version")
	}
}

func TestArtifactNameHelper(t *testing.T) {
	got := ArtifactName("a", "b", "1.0.0-rc.1")
	want := "a-b-1.0.0-rc.1.tar.gz"
	if got != want {
		t.Errorf("ArtifactName = %q, want %q", got, want)
	}
}
