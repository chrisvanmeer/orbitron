package fetcher

import (
	"os"
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
	f := NewFetcher(dir, 0)

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
	f := NewFetcher(t.TempDir(), 2)
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
