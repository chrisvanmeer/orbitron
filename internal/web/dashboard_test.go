package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitron/internal/config"
)

// TestHandleStorageCollapsesMultiVersionItems verifies that roles and
// collections with more than one cached version are rendered as a single
// collapsed group row (with an expand caret and version count) followed by one
// hidden sub-row per version, while single-version items stay plain rows.
func TestHandleStorageCollapsesMultiVersionItems(t *testing.T) {
	storage := t.TempDir()

	mustMkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(storage, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Multi-version role.
	mustMkdir("roles/geerlingguy.nginx/2.0.1")
	mustMkdir("roles/geerlingguy.nginx/1.9.9")
	// Single-version role.
	mustMkdir("roles/single.role/1.0.0")

	// Multi-version and single-version collections.
	mustMkdir("collections/community")
	for _, f := range []string{"community-general-8.5.0.tar.gz", "community-general-8.4.0.tar.gz", "community-single-1.0.0.tar.gz"} {
		if err := os.WriteFile(filepath.Join(storage, "collections/community", f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d := NewDashboard(&config.Config{StoragePath: storage})
	rec := httptest.NewRecorder()
	d.handleStorage(rec, httptest.NewRequest("GET", "/ui/storage", nil))
	body := rec.Body.String()

	if got := strings.Count(body, `class="group-row"`); got != 2 {
		t.Fatalf("expected 2 collapsed group rows (role + collection), got %d\n%s", got, body)
	}
	if got := strings.Count(body, `class="version-row"`); got != 4 {
		t.Fatalf("expected 4 hidden version sub-rows, got %d\n%s", got, body)
	}
	if got := strings.Count(body, `style="display:none"`); got != 4 {
		t.Fatalf("expected 4 version sub-rows served hidden, got %d\n%s", got, body)
	}
	if got := strings.Count(body, `class="expand-caret"`); got != 2 {
		t.Fatalf("expected 2 expand carets, got %d\n%s", got, body)
	}

	roleGroup := `data-group="role:geerlingguy.nginx"`
	if got := strings.Count(body, roleGroup); got != 3 {
		t.Fatalf("expected 3 role:geerlingguy.nginx rows (1 group + 2 versions), got %d\n%s", got, body)
	}
	if !strings.Contains(body, ">2 VERSIONS<") {
		t.Fatalf("expected '2 VERSIONS' in the role group version cell\n%s", body)
	}
	if !strings.Contains(body, `class="group-row" data-search="role geerlingguy.nginx 2.0.1 1.9.9"`) {
		t.Fatalf("expected role group row search to cover every version\n%s", body)
	}

	colGroup := `data-group="collection:community.general"`
	if got := strings.Count(body, colGroup); got != 3 {
		t.Fatalf("expected 3 collection:community.general rows (1 group + 2 versions), got %d\n%s", got, body)
	}

	// Single-version items must stay plain rows (no group, no caret).
	for _, want := range []string{`data-search="role single.role 1.0.0"`, `data-search="collection community.single 1.0.0"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected plain single-version row %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `data-group="role:single.role"`) || strings.Contains(body, `data-group="collection:community.single"`) {
		t.Fatalf("single-version items must not get a collapsible group row\n%s", body)
	}
}
