package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestScanCountsAcrossLayout(t *testing.T) {
	root := t.TempDir()

	// One role with two cached versions.
	writeFile(t, filepath.Join(root, "roles", "geerlingguy.nginx", "3.3.1", "content.bin"), []byte("nginx v3.3.1"))
	writeFile(t, filepath.Join(root, "roles", "geerlingguy.nginx", "3.2.0", "content.bin"), []byte("nginx v3.2.0"))

	// An empty role directory must not count.
	if err := os.MkdirAll(filepath.Join(root, "roles", "empty.role"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Two collections in two namespaces, one with multiple versions.
	writeFile(t, filepath.Join(root, "collections", "community", "community-general-8.5.0.tar.gz"), []byte("coll 8.5.0"))
	writeFile(t, filepath.Join(root, "collections", "gluster", "gluster-gluster-1.0.2.tar.gz"), []byte("coll 1.0.2"))
	writeFile(t, filepath.Join(root, "collections", "gluster", "gluster-gluster-1.0.1.tar.gz"), []byte("coll 1.0.1"))

	// A git-backed checkout directory must contribute nothing.
	writeFile(t, filepath.Join(root, "collections", "git", "some-repo", "README.md"), []byte("git-backed"))

	// A file directly under containers (not an archive) must be ignored.
	writeFile(t, filepath.Join(root, "collections", "containers", "notes.txt"), []byte("not an archive"))

	inv := Scan(root)

	if got := inv.RoleCount(); got != 1 {
		t.Errorf("RoleCount = %d, want 1", got)
	}
	if got := inv.RoleVersionCount(); got != 2 {
		t.Errorf("RoleVersionCount = %d, want 2", got)
	}
	if got := inv.CollectionCount(); got != 2 {
		t.Errorf("CollectionCount = %d, want 2", got)
	}
	if got := inv.CollectionVersionCount(); got != 3 {
		t.Errorf("CollectionVersionCount = %d, want 3", got)
	}
	if got := inv.NamespaceCount(); got != 2 {
		t.Errorf("NamespaceCount = %d, want 2", got)
	}

	if len(inv.Roles) != 1 {
		t.Fatalf("Roles = %v, want exactly 1", inv.Roles)
	}
	if inv.Roles[0].Name != "geerlingguy.nginx" {
		t.Errorf("role name = %q, want geerlingguy.nginx", inv.Roles[0].Name)
	}
	if inv.Roles[0].Versions[0].Version != "3.3.1" {
		t.Errorf("first role version = %q, want 3.3.1 (desc)", inv.Roles[0].Versions[0].Version)
	}

	var collectionNames []string
	for _, it := range inv.Collections {
		collectionNames = append(collectionNames, it.Name)
	}
	if len(collectionNames) != 2 {
		t.Fatalf("Collections = %v, want exactly 2", inv.Collections)
	}
	if collectionNames[0] != "community.general" || collectionNames[1] != "gluster.gluster" {
		t.Errorf("collection names = %v, want sorted community.general, gluster.gluster", collectionNames)
	}
	if inv.Collections[1].Namespace != "gluster" {
		t.Errorf("gluster namespace = %q, want gluster", inv.Collections[1].Namespace)
	}
	if inv.Collections[1].Versions[0].Version != "1.0.2" {
		t.Errorf("first gluster version = %q, want 1.0.2 (desc)", inv.Collections[1].Versions[0].Version)
	}
}

func TestScanBytes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "roles", "geerlingguy.nginx", "3.3.1", "content.bin"), []byte("0123456789ABCDEF"))
	writeFile(t, filepath.Join(root, "collections", "community", "community-general-8.5.0.tar.gz"), []byte("0123456789ABCDEF"))

	inv := Scan(root)

	for _, it := range inv.Roles {
		for _, v := range it.Versions {
			if v.SizeBytes <= 0 {
				t.Errorf("role %s/%s SizeBytes = %d, want > 0", it.Name, v.Version, v.SizeBytes)
			}
		}
	}
	for _, it := range inv.Collections {
		for _, v := range it.Versions {
			if v.SizeBytes <= 0 {
				t.Errorf("collection %s/%s SizeBytes = %d, want > 0", it.Name, v.Version, v.SizeBytes)
			}
		}
	}

	if got, want := inv.RoleBytes()+inv.CollectionBytes(), inv.StorageBytes(); got != want {
		t.Errorf("RoleBytes + CollectionBytes = %d, StorageBytes = %d", got, want)
	}
	if inv.RoleBytes() < 16 || inv.CollectionBytes() < 16 {
		t.Errorf("bytes too small: roles=%d collections=%d (each has a 16-byte file)", inv.RoleBytes(), inv.CollectionBytes())
	}
}

func TestScanMissingRoot(t *testing.T) {
	inv := Scan(filepath.Join(t.TempDir(), "does-not-exist"))
	if inv.RoleCount() != 0 || inv.CollectionCount() != 0 || inv.RoleVersionCount() != 0 || inv.CollectionVersionCount() != 0 {
		t.Errorf("expected empty inventory, got %+v", inv)
	}
	if inv.StorageBytes() != 0 {
		t.Errorf("StorageBytes = %d, want 0", inv.StorageBytes())
	}
}
