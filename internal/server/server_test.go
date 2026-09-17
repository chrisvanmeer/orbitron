package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitron/internal/config"
	"orbitron/internal/fetcher"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	storage := t.TempDir()
	cfg := &config.Config{
		StoragePath: storage,
		TokensFile:  writeTestTokensFile(t, "valid-admin-token"),
	}
	s := &Server{cfg: cfg}
	s.fetcher = fetcher.NewFetcher(storage, cfg.MaxConcurrency)
	return s, storage
}

func TestGenerateRoleIDStable(t *testing.T) {
	a := generateRoleID("geerlingguy.nginx")
	b := generateRoleID("geerlingguy.nginx")
	if a != b {
		t.Errorf("expected stable role ID, got %q and %q", a, b)
	}
	if a == "" {
		t.Error("expected non-empty role ID")
	}
	if a == generateRoleID("geerlingguy.docker") {
		t.Error("expected distinct role IDs for distinct names")
	}
}

func TestRequestBaseURL(t *testing.T) {
	s := &Server{}

	httpReq := httptest.NewRequest(http.MethodGet, "http://mirror.example.com/api/", nil)
	if got := s.requestBaseURL(httpReq); got != "http://mirror.example.com" {
		t.Errorf("plain http: got %q", got)
	}

	tlsReq := httptest.NewRequest(http.MethodGet, "https://mirror.example.com/api/", nil)
	if got := s.requestBaseURL(tlsReq); got != "https://mirror.example.com" {
		t.Errorf("direct tls: got %q", got)
	}

	proxied := httptest.NewRequest(http.MethodGet, "http://mirror.example.com/api/", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if got := s.requestBaseURL(proxied); got != "https://mirror.example.com" {
		t.Errorf("forwarded proto: got %q", got)
	}
}

func TestListCollectionVersionsUsesForwardedScheme(t *testing.T) {
	s, storage := newTestServer(t)

	nsDir := filepath.Join(storage, "collections", "community")
	if err := os.MkdirAll(nsDir, 0750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"community-general-8.4.0.tar.gz", "community-general-8.5.0.tar.gz"} {
		if err := os.WriteFile(filepath.Join(nsDir, f), []byte("x"), 0640); err != nil {
			t.Fatal(err)
		}
	}

	results := s.listCollectionVersions("https://mirror.example.com", "/api/v3/", "community", "general")
	if len(results) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(results))
	}

	want := "https://mirror.example.com/api/v3/artifacts/community-general-8.5.0.tar.gz"
	found := false
	for _, r := range results {
		if r.DownloadURL == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected download URL %q, got %+v", want, results)
	}
}

func TestHandleGalaxyV1RoleVersionsScheme(t *testing.T) {
	s, storage := newTestServer(t)

	roleDir := filepath.Join(storage, "roles", "geerlingguy.nginx")
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := os.MkdirAll(filepath.Join(roleDir, v), 0750); err != nil {
			t.Fatal(err)
		}
	}

	roleID := generateRoleID("geerlingguy.nginx")
	req := httptest.NewRequest(http.MethodGet, "http://mirror.example.com/api/v1/roles/"+roleID+"/versions/", nil)
	req.Host = "mirror.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	s.HandleGalaxyV1RolesRouter(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body struct {
		Results []struct {
			Name        string `json:"name"`
			DownloadURL string `json:"download_url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Results) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(body.Results))
	}
	for _, r := range body.Results {
		if !strings.HasPrefix(r.DownloadURL, "https://mirror.example.com/") {
			t.Errorf("expected https download URL, got %q", r.DownloadURL)
		}
	}
}

func TestHandleRoleDownloadStreamsTarball(t *testing.T) {
	s, storage := newTestServer(t)

	roleDir := filepath.Join(storage, "roles", "acme.sample", "1.2.3", "meta")
	if err := os.MkdirAll(roleDir, 0750); err != nil {
		t.Fatal(err)
	}
	wantContent := "galaxy_info:\n  role_name: sample\n"
	if err := os.WriteFile(filepath.Join(roleDir, "main.yml"), []byte(wantContent), 0640); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/roles/download/acme.sample/1.2.3.tar.gz", nil)
	rec := httptest.NewRecorder()

	s.HandleRoleDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = gz.Close() }()

	found := false
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}

		if hdr.Name == "meta/main.yml" {
			data, _ := io.ReadAll(tr)
			if string(data) != wantContent {
				t.Errorf("unexpected file content: %q", string(data))
			}
			found = true
		}
	}
	if !found {
		t.Error("expected meta/main.yml inside streamed tarball")
	}
}

// registerDeleteMux builds a http.ServeMux containing the two admin DELETE
// routes exactly as Server.Start registers them (method-scoped patterns wrapped
// in AuthMiddleware). This is the honest way to test these endpoints: path
// values are only populated by the mux, and real admin-token auth is applied.
func registerDeleteMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/storage/roles/{role}/{version}", s.AuthMiddleware(s.HandleRoleDelete))
	mux.HandleFunc("DELETE /api/v1/storage/collections/{collection}/{version}", s.AuthMiddleware(s.HandleCollectionDelete))
	return mux
}

// writeTestTokensFile writes a single valid token into a temp tokens store
// in the exact shape auth.LoadTokens parses: {"tokens":{"<token>":<unix>}}.
func writeTestTokensFile(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	data := fmt.Sprintf(`{"tokens":{"%s":%d}}`, token, time.Now().Add(time.Hour).Unix())
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandleRoleDeleteAPI(t *testing.T) {
	s, storage := newTestServer(t)

	// Seed a real fetcher + cache layout: two version dirs, a git role layout
	// for namespace.name style.
	nm := "geerlingguy.nginx"
	roleBase := filepath.Join(storage, "roles", nm)
	for _, v := range []string{"1.2.3", "2.0.0"} {
		if err := os.MkdirAll(filepath.Join(roleBase, v), 0750); err != nil {
			t.Fatal(err)
		}
	}

	// Seed a stored requirements manifest that pins the role exactly at 1.2.3.
	manifestRoleDir := filepath.Join(storage, "manifests")
	if err := os.MkdirAll(manifestRoleDir, 0750); err != nil {
		t.Fatal(err)
	}
	pinned := []byte("roles:\n  - name: geerlingguy.nginx\n    version: 1.2.3\n")
	if err := s.fetcher.SaveManifest("roles", pinned); err != nil {
		t.Fatal(err)
	}

	mux := registerDeleteMux(s)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, ts.URL+"/api/v1/storage/roles/"+nm+"/1.2.3", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// No token -> 401.
	if rec := send(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no token, got %d", rec.Code)
	}

	// With valid token -> 200, the requested version dir and its manifest pin
	// are both removed; the sibling version dir is untouched.
	rec := send("valid-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(roleBase, "1.2.3")); !os.IsNotExist(err) {
		t.Errorf("expected 1.2.3 dir to be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(roleBase, "2.0.0")); err != nil {
		t.Errorf("expected 2.0.0 dir to remain, got err=%v", err)
	}

	// The stored manifest must no longer pin 1.2.3.
	entries, err := os.ReadDir(manifestRoleDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(manifestRoleDir, e.Name()))
		if bytes.Contains(data, []byte("version: 1.2.3")) {
			t.Errorf("manifest %s still pins deleted version 1.2.3\n%s", e.Name(), data)
		}
	}
}

func TestHandleCollectionVersionDeleteAPI(t *testing.T) {
	s, storage := newTestServer(t)

	colDir := filepath.Join(storage, "collections", "community", "dev.mycol")
	if err := os.MkdirAll(colDir, 0750); err != nil {
		t.Fatal(err)
	}
	arrow := filepath.Join(colDir, "community-dev.mycol-8.5.0.tar.gz")
	if err := os.WriteFile(arrow, []byte("artifact"), 0640); err != nil {
		t.Fatal(err)
	}

	mux := registerDeleteMux(s)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req := httptest.NewRequest(http.MethodDelete, ts.URL+"/api/v1/storage/collections/community.dev.mycol/8.5.0", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestHandleRoleDownloadMissingReturns404(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/roles/download/nope.missing/9.9.9.tar.gz", nil)
	rec := httptest.NewRecorder()

	s.HandleRoleDownload(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}
