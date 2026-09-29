package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitron/internal/config"
	"orbitron/internal/fetcher"
)

// registerDumpMux mirrors the route exactly as Start() registers it, so the
// test exercises the real method/path matching and the auth middleware.
func registerDumpMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/dump", s.AuthMiddleware(s.HandleDump))
	return mux
}

// seedDumpStorage lays out a representative cache: a role tree, a collection
// artifact, a stored manifest and the access index, plus a token store *inside*
// the storage path (the container default) that must never be archived.
func seedDumpStorage(t *testing.T, storage string) {
	t.Helper()

	write := func(rel string, content []byte) string {
		p := filepath.Join(storage, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o640); err != nil {
			t.Fatal(err)
		}
		return p
	}

	write(filepath.Join("roles", "geerlingguy.nginx", "1.2.3", "meta", "main.yml"), []byte("galaxy_info:\n  role_name: nginx\n"))
	write(filepath.Join("roles", "geerlingguy.nginx", "1.2.3", "tasks", "main.yml"), []byte("- name: install\n"))
	write(filepath.Join("roles", "geerlingguy.nginx", "2.0.0", "meta", "main.yml"), []byte("galaxy_info:\n  role_name: nginx\n"))
	write(filepath.Join("collections", "community", "community-general-8.5.0.tar.gz"), []byte("pretend-collection"))
	write(filepath.Join("collections", "git", "myrepo", "README.md"), []byte("# repo\n"))
	write(filepath.Join("manifests", "roles_abc123_requirements.yml"), []byte("roles:\n  - name: geerlingguy.nginx\n"))
	write(".access.json", []byte(`{"role:geerlingguy.nginx@1.2.3":1700000000}`))
	// The secret that must be filtered out.
	write("tokens.json", []byte(`{"tokens":{"deadbeef":{"created_at":1,"expires_at":0,"label":"admin"}}}`))
}

// readDumpEntries decompresses the recorded body into an ordered entry list.
func readDumpEntries(t *testing.T, body io.Reader) []*tar.Header {
	t.Helper()
	gr, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	var headers []*tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		headers = append(headers, hdr)
	}
	return headers
}

func dumpEntryNames(headers []*tar.Header) map[string]string {
	names := make(map[string]string, len(headers))
	for _, hdr := range headers {
		names[hdr.Name] = hdr.Linkname
	}
	return names
}

func requestDump(t *testing.T, mux *http.ServeMux, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dump", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleDumpRequiresAuth(t *testing.T) {
	s, storage := newTestServer(t)
	seedDumpStorage(t, storage)
	mux := registerDumpMux(s)

	if rec := requestDump(t, mux, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}
	if rec := requestDump(t, mux, "wrong-token"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: got %d, want 401", rec.Code)
	}
	if rec := requestDump(t, mux, "valid-admin-token"); rec.Code != http.StatusOK {
		t.Errorf("valid token: got %d, want 200", rec.Code)
	}
}

func TestHandleDumpRejectsNonGet(t *testing.T) {
	s, storage := newTestServer(t)
	seedDumpStorage(t, storage)
	mux := registerDumpMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/dump", nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// A POST does not match the GET pattern, so ServeMux answers 405 itself.
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d, want 405", rec.Code)
	}
}

func TestHandleDumpHeaders(t *testing.T) {
	s, storage := newTestServer(t)
	seedDumpStorage(t, storage)
	mux := registerDumpMux(s)

	rec := requestDump(t, mux, "valid-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "orbitron-dump-") || !strings.Contains(cd, ".tar.gz") {
		t.Errorf("Content-Disposition = %q, want an orbitron-dump-*.tar.gz attachment", cd)
	}
	if f := rec.Header().Get("X-Orbitron-Dump-Format"); f != "1" {
		t.Errorf("X-Orbitron-Dump-Format = %q, want 1", f)
	}
	if ts := rec.Header().Get("X-Orbitron-Dump-Created"); ts == "" {
		t.Error("X-Orbitron-Dump-Created is empty")
	}
	if sync := rec.Header().Get("X-Orbitron-Dump-Sync"); sync != "idle" {
		t.Errorf("X-Orbitron-Dump-Sync = %q, want idle", sync)
	}
	if b := rec.Header().Get("X-Orbitron-Dump-Bytes"); b == "" || b == "0" {
		t.Errorf("X-Orbitron-Dump-Bytes = %q, want a positive estimate", b)
	}
}

func TestHandleDumpContentAndExclusions(t *testing.T) {
	s, storage := newTestServer(t)
	seedDumpStorage(t, storage)
	mux := registerDumpMux(s)

	rec := requestDump(t, mux, "valid-admin-token")
	headers := readDumpEntries(t, bytes.NewReader(rec.Body.Bytes()))
	entries := dumpEntryNames(headers)

	for _, want := range []string{
		"orbitron/roles",
		"orbitron/roles/geerlingguy.nginx/1.2.3/meta/main.yml",
		"orbitron/roles/geerlingguy.nginx/1.2.3/tasks/main.yml",
		"orbitron/roles/geerlingguy.nginx/2.0.0/meta/main.yml",
		"orbitron/collections/community/community-general-8.5.0.tar.gz",
		"orbitron/collections/git/myrepo/README.md",
		"orbitron/manifests/roles_abc123_requirements.yml",
		"orbitron/.access.json",
		"orbitron/dump.json",
	} {
		if _, ok := entries[want]; !ok {
			t.Errorf("missing archive entry %q (have %v)", want, sortedKeys(entries))
		}
	}

	for name := range entries {
		if strings.HasSuffix(name, "tokens.json") {
			t.Errorf("token store leaked into the dump as %q", name)
		}
		if strings.Contains(name, "..") {
			t.Errorf("archive entry %q contains a traversal component", name)
		}
	}
}

func TestHandleDumpManifest(t *testing.T) {
	s, storage := newTestServer(t)
	seedDumpStorage(t, storage)
	mux := registerDumpMux(s)

	rec := requestDump(t, mux, "valid-admin-token")
	body := rec.Body.Bytes()

	// dump.json must be the LAST entry so the checksums cover a single pass.
	headers := readDumpEntries(t, bytes.NewReader(body))
	if len(headers) == 0 {
		t.Fatal("empty archive")
	}
	last := headers[len(headers)-1]
	if last.Name != "orbitron/dump.json" {
		t.Fatalf("last entry = %q, want orbitron/dump.json", last.Name)
	}

	// Re-read the manifest payload itself.
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gr)
	var manifest dumpManifest
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != "orbitron/dump.json" {
			continue
		}
		found = true
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			t.Fatalf("decode dump.json: %v", err)
		}
	}
	if !found {
		t.Fatal("dump.json not found while decoding")
	}

	if manifest.Format != "orbitron-dump" || manifest.Version != 1 {
		t.Errorf("manifest format/version = %q/%d", manifest.Format, manifest.Version)
	}
	if manifest.Roles != 1 || manifest.RoleVersions != 2 {
		t.Errorf("manifest roles = %d/%d, want 1/2", manifest.Roles, manifest.RoleVersions)
	}
	if manifest.Collections != 1 || manifest.CollectionVersions != 1 {
		t.Errorf("manifest collections = %d/%d, want 1/1", manifest.Collections, manifest.CollectionVersions)
	}
	if manifest.SyncRunning {
		t.Error("manifest reports a running sync in an idle test server")
	}
	if manifest.Bytes <= 0 {
		t.Errorf("manifest Bytes = %d, want > 0", manifest.Bytes)
	}
	if !containsString(manifest.Excluded, "tokens.json") {
		t.Errorf("manifest Excluded = %v, want it to list tokens.json", manifest.Excluded)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("manifest carries no file checksums")
	}

	var total int64
	for _, f := range manifest.Files {
		if f.SHA256 == "" {
			t.Errorf("file %q has no sha256", f.Path)
		}
		if strings.HasSuffix(f.Path, "dump.json") {
			t.Errorf("manifest lists itself: %q", f.Path)
		}
		total += f.Size
	}
	if total != manifest.Bytes {
		t.Errorf("manifest file sizes sum to %d but Bytes = %d", total, manifest.Bytes)
	}
}

func TestHandleDumpEmptyStorage(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerDumpMux(s)

	rec := requestDump(t, mux, "valid-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	headers := readDumpEntries(t, bytes.NewReader(rec.Body.Bytes()))
	entries := dumpEntryNames(headers)
	if _, ok := entries["orbitron/dump.json"]; !ok {
		t.Errorf("missing orbitron/dump.json, have %v", sortedKeys(entries))
	}
	// An empty cache still archives the (empty) top-level directories the
	// fetcher creates, but no regular files besides the manifest itself.
	var files []string
	for _, hdr := range headers {
		if hdr.Typeflag == tar.TypeReg {
			files = append(files, hdr.Name)
		}
	}
	if len(files) != 1 || files[0] != "orbitron/dump.json" {
		t.Errorf("regular archive entries = %v, want only orbitron/dump.json", files)
	}
}

func TestHandleDumpArchivesSymlink(t *testing.T) {
	s, storage := newTestServer(t)
	target := filepath.Join(storage, "roles", "geerlingguy.nginx", "1.2.3", "meta", "main.yml")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("real\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(storage, "roles", "geerlingguy.nginx", "1.2.3", "meta", "link.yml")
	if err := os.Symlink("main.yml", link); err != nil {
		t.Fatal(err)
	}

	mux := registerDumpMux(s)
	rec := requestDump(t, mux, "valid-admin-token")
	entries := dumpEntryNames(readDumpEntries(t, bytes.NewReader(rec.Body.Bytes())))

	linkName := "orbitron/roles/geerlingguy.nginx/1.2.3/meta/link.yml"
	target2, ok := entries[linkName]
	if !ok {
		t.Fatalf("symlink %q missing, have %v", linkName, sortedKeys(entries))
	}
	if target2 != "main.yml" {
		t.Errorf("symlink target = %q, want main.yml (the real link target, not the file name)", target2)
	}
}

func TestHandleDumpExcludesTokenStoreOutsideStorage(t *testing.T) {
	// When the token store lives outside the storage path it is absent from the
	// dump anyway; Excluded must then not pretend it was filtered.
	storage := t.TempDir()
	tokens := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(tokens, []byte(`{"tokens":{"valid-admin-token":{"created_at":1,"expires_at":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storage, "roles", "r", "1.0.0"), 0o750); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &config.Config{StoragePath: storage, TokensFile: tokens}}
	s.fetcher = fetcher.NewFetcher(storage, 1, fetcher.ProxyConfig{}, config.TLSConfig{})

	mux := registerDumpMux(s)
	rec := requestDump(t, mux, "valid-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	entries := dumpEntryNames(readDumpEntries(t, bytes.NewReader(rec.Body.Bytes())))
	if _, ok := entries["orbitron/roles/r/1.0.0"]; !ok {
		t.Errorf("missing the seeded role dir, have %v", sortedKeys(entries))
	}
}

func TestHandleDumpCompressParameter(t *testing.T) {
	s, storage := newTestServer(t)
	p := filepath.Join(storage, "roles", "big.role", "1.0.0", "data.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte("orbitron "), 5000), 0o640); err != nil {
		t.Fatal(err)
	}

	fetch := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/dump"+query, nil)
		req.Header.Set("Authorization", "Bearer valid-admin-token")
		rec := httptest.NewRecorder()
		registerDumpMux(s).ServeHTTP(rec, req)
		return rec
	}

	// fast produces a valid archive with the same payload as the default.
	for _, query := range []string{"", "?compress=default", "?compress=fast"} {
		rec := fetch(query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: got %d, want 200", query, rec.Code)
		}
		entries := dumpEntryNames(readDumpEntries(t, bytes.NewReader(rec.Body.Bytes())))
		if _, ok := entries["orbitron/roles/big.role/1.0.0/data.txt"]; !ok {
			t.Errorf("%q: payload file missing, have %v", query, sortedKeys(entries))
		}
	}

	// An unknown level is rejected before any archive bytes are written.
	if rec := fetch("?compress=turbo"); rec.Code != http.StatusBadRequest {
		t.Errorf("compress=turbo: got %d, want 400", rec.Code)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Small maps in tests; a simple insertion sort keeps imports lean.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want || strings.HasSuffix(s, "/"+want) {
			return true
		}
	}
	return false
}
