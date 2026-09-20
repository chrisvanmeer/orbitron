package telemetry

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"orbitron/internal/config"
)

func testConfig(t *testing.T, storage string) *config.Config {
	t.Helper()
	return &config.Config{
		StoragePath: storage,
		TokensFile:  filepath.Join(t.TempDir(), "tokens.json"),
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func metricValue(t *testing.T, body, name string) int {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (\d+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("metric %q not found in:\n%s", name, body)
	}
	var v int
	if _, err := fmt.Sscanf(m[1], "%d", &v); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return v
}

func TestMetricsHandler(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "roles", "geerlingguy.nginx", "3.3.1", "content.bin"), []byte("nginx"))
	writeFile(t, filepath.Join(root, "roles", "geerlingguy.nginx", "3.2.0", "content.bin"), []byte("nginx"))
	writeFile(t, filepath.Join(root, "collections", "community", "community-general-8.5.0.tar.gz"), []byte("coll"))
	writeFile(t, filepath.Join(root, "collections", "gluster", "gluster-gluster-1.0.2.tar.gz"), []byte("coll"))

	cfg := testConfig(t, root)
	handler := NewMetrics().Handler(cfg)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()

	// Aggregate gauges must match the on-disk layout (not the raw top-level
	// directory listing that produced a bogus 0 before).
	if got := metricValue(t, body, "orbitron_roles_total"); got != 1 {
		t.Errorf("orbitron_roles_total = %d, want 1", got)
	}
	if got := metricValue(t, body, "orbitron_role_versions_total"); got != 2 {
		t.Errorf("orbitron_role_versions_total = %d, want 2", got)
	}
	if got := metricValue(t, body, "orbitron_collections_total"); got != 2 {
		t.Errorf("orbitron_collections_total = %d, want 2", got)
	}
	if got := metricValue(t, body, "orbitron_collection_versions_total"); got != 2 {
		t.Errorf("orbitron_collection_versions_total = %d, want 2", got)
	}
	if got := metricValue(t, body, "orbitron_cached_versions_total"); got != 4 {
		t.Errorf("orbitron_cached_versions_total = %d, want 4", got)
	}
	if got := metricValue(t, body, "orbitron_namespaces_total"); got != 2 {
		t.Errorf("orbitron_namespaces_total = %d, want 2", got)
	}

	rolesBytes := metricValue(t, body, "orbitron_roles_bytes")
	collectionsBytes := metricValue(t, body, "orbitron_collections_bytes")
	storageBytes := metricValue(t, body, "orbitron_storage_bytes")
	if rolesBytes == 0 || collectionsBytes == 0 {
		t.Errorf("bytes too small: roles=%d collections=%d", rolesBytes, collectionsBytes)
	}
	if storageBytes != rolesBytes+collectionsBytes {
		t.Errorf("storage (%d) != roles (%d) + collections (%d)", storageBytes, rolesBytes, collectionsBytes)
	}

	// Per-item labeled series, one line per item and per version.
	for _, want := range []string{
		`orbitron_cached_item_bytes{type="role",name="geerlingguy.nginx"} `,
		`orbitron_cached_item_bytes{type="collection",name="community.general"} `,
		`orbitron_cached_version_bytes{type="role",name="geerlingguy.nginx",version="3.3.1"} `,
		`orbitron_cached_version_bytes{type="role",name="geerlingguy.nginx",version="3.2.0"} `,
		`orbitron_cached_version_bytes{type="collection",name="gluster.gluster",version="1.0.2"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing expected exposition line prefix %q in:\n%s", want, body)
		}
	}

	// The outdated manifest metric must be gone.
	if strings.Contains(body, "orbitron_manifests_total") {
		t.Errorf("stale orbitron_manifests_total still exposed:\n%s", body)
	}
}

func TestMetricsHandlerActiveTokens(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	tokens := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(tokens, []byte(`{"tokens":{"deadbeef":{"created_at":100,"expires_at":0,"label":"ci"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TokensFile = tokens

	handler := NewMetrics().Handler(cfg)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/metrics", nil))

	if got := metricValue(t, rec.Body.String(), "orbitron_active_tokens_total"); got != 1 {
		t.Errorf("orbitron_active_tokens_total = %d, want 1", got)
	}
}
