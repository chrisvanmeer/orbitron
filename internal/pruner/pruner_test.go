package pruner

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestRunPruneOrphanDetectionRespectsDecline(t *testing.T) {
	storage := t.TempDir()
	manifestDir := filepath.Join(storage, "manifests")
	if err := os.MkdirAll(manifestDir, 0750); err != nil {
		t.Fatal(err)
	}

	// Active manifest declares foo.role@1.0.0 only.
	manifest := []byte("roles:\n  - name: foo.role\n    version: 1.0.0\n")
	if err := os.WriteFile(filepath.Join(manifestDir, "roles_abc123_requirements.yml"), manifest, 0640); err != nil {
		t.Fatal(err)
	}

	activeDir := filepath.Join(storage, "roles", "foo.role", "1.0.0")
	orphanDir := filepath.Join(storage, "roles", "foo.role", "9.9.9")
	for _, d := range []string{activeDir, orphanDir} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}

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
	if err := p.RunPrune(); err != nil {
		t.Fatalf("RunPrune returned error: %v", err)
	}

	if _, err := os.Stat(orphanDir); err != nil {
		t.Errorf("orphan directory should remain when prune is declined: %v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("active directory must never be removed: %v", err)
	}
}

func TestKeepVersionConstraintAware(t *testing.T) {
	disk := []string{"0.9.0", "1.0.0", "1.5.0", "2.0.0"}

	tests := []struct {
		declarations []string
		candidate    string
		want         bool
	}{
		{[]string{"1.0.0"}, "1.0.0", true},
		{[]string{"1.0.0"}, "1.5.0", false},
		{[]string{">=1.0.0"}, "2.0.0", true},
		{[]string{">=1.0.0"}, "1.5.0", false},
		{[]string{"<2.0.0"}, "1.5.0", true},
		{[]string{"<2.0.0"}, "1.0.0", false},
		{[]string{"latest"}, "2.0.0", true},
		{[]string{"latest"}, "1.5.0", false},
		{[]string{""}, "2.0.0", true},
		{[]string{"main"}, "main", true},
		{[]string{"main"}, "2.0.0", false},
		{[]string{">=1.4.5,<2.0.0", "1.0.0"}, "1.0.0", true},
		{[]string{">9.0.0"}, "2.0.0", false},
	}

	for _, tt := range tests {
		if got := keepVersion(tt.declarations, disk, tt.candidate); got != tt.want {
			t.Errorf("keepVersion(%v, disk, %q) = %v, want %v", tt.declarations, tt.candidate, got, tt.want)
		}
	}
}
