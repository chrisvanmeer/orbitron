package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
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
	"orbitron/internal/logger"
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

// registerAdminMux mirrors the new-era admin API routes registered in Start():
// token lifecycle, sync status, and the (placeholder) prune endpoint.
func registerAdminMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tokens", s.AuthMiddleware(s.HandleTokenCreate))
	mux.HandleFunc("GET /api/v1/tokens", s.AuthMiddleware(s.HandleTokenList))
	mux.HandleFunc("DELETE /api/v1/tokens/{token}", s.AuthMiddleware(s.HandleTokenRevoke))
	mux.HandleFunc("POST /api/v1/tokens/{token}/rotate", s.AuthMiddleware(s.HandleTokenRotate))
	mux.HandleFunc("GET /api/v1/sync/status", s.AuthMiddleware(s.HandleSyncStatus))
	mux.HandleFunc("POST /api/v1/prune", s.AuthMiddleware(s.HandlePrune))
	mux.HandleFunc("GET /api/v1/manifests", s.AuthMiddleware(s.HandleManifests))
	mux.HandleFunc("GET /api/v1/storage", s.AuthMiddleware(s.HandleStorageInventory))
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

func TestHealthzOK(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.HandleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Status          string `json:"status"`
		StorageWritable bool   `json:"storage_writable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || !body.StorageWritable {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestHealthzDegradedWhenStorageUnwritable(t *testing.T) {
	s := &Server{cfg: &config.Config{
		StoragePath: filepath.Join(t.TempDir(), "does-not-exist"),
	}}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.HandleHealthz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestLoggingMiddlewareSuppressesRoutinePaths(t *testing.T) {
	s, _ := newTestServer(t)
	handler := s.LoggingMiddleware(http.NewServeMux())

	var buf bytes.Buffer
	logger.SetOutput(&buf)
	defer logger.SetOutput(os.Stdout)

	for _, suppressed := range []string{"/healthz", "/favicon.ico", "/ui/"} {
		buf.Reset()
		req := httptest.NewRequest(http.MethodGet, suppressed, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if strings.Contains(buf.String(), suppressed) {
			t.Errorf("expected %q to be suppressed from log output, got: %s", suppressed, buf.String())
		}
	}

	buf.Reset()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !strings.Contains(buf.String(), "/api/v1/sync/status") {
		t.Errorf("expected ordinary API path to be logged, got: %s", buf.String())
	}
}

func TestSyncStatusEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerAdminMux(s)

	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	rec := send("valid-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var snap struct {
		Current any   `json:"current"`
		History []any `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Current != nil {
		t.Errorf("expected null current job, got %v", snap.Current)
	}
	if snap.History == nil {
		t.Error("expected history array to be present")
	}
}

func TestTokenCRUDFlow(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerAdminMux(s)

	// Create with a request-scoped TTL and label.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"ttl_days":1,"label":"ci"}`))
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create token: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var created tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if created.Label != "ci" {
		t.Errorf("label = %q, want ci", created.Label)
	}
	if created.ExpiresAt-created.CreatedAt != 24*60*60 {
		t.Errorf("expected 1 day TTL, got %d", created.ExpiresAt-created.CreatedAt)
	}

	// List hides secrets by default.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tokens?full=false", nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens: expected 200, got %d", rec.Code)
	}
	var list struct {
		Tokens []tokenResponse `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tokens) < 2 {
		t.Fatalf("expected at least 2 tokens listed, got %d", len(list.Tokens))
	}
	for _, entry := range list.Tokens {
		if entry.Token != "" {
			t.Errorf("token secret leaked without full=true: %q", entry.Token)
		}
	}

	// full=true reveals secrets.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tokens?full=true", nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	secretSeen := false
	for _, entry := range list.Tokens {
		if entry.Token != "" {
			secretSeen = true
		}
	}
	if !secretSeen {
		t.Error("expected raw tokens with full=true")
	}

	// Rotate the created token: old revoked, new active.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tokens/"+created.Token+"/rotate", nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate token: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var rotated tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Token == "" || rotated.Token == created.Token {
		t.Errorf("expected fresh token, got %q", rotated.Token)
	}

	// The revoked original can no longer authenticate.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token should be rejected, got %d", rec.Code)
	}

	// Revoke the rotated token.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/"+rotated.Token, nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke token: expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	// Revoking again returns 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/"+rotated.Token, nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("second revoke: expected 404, got %d", rec.Code)
	}
}

func TestTokenCreateRequiresAuth(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerAdminMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"ttl_days":1}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestExpiredTokenRejectedByAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	// A token whose expires_at has already passed, in the current token format.
	now := time.Now().Unix()
	data := fmt.Sprintf(`{"tokens":{"expired-token":{"created_at":%d,"expires_at":%d}}}`, now-7200, now-3600)
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: &config.Config{TokensFile: path}}
	mux := registerAdminMux(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer expired-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d", rec.Code)
	}
}

func TestPruneEndpointForFutureUse(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerAdminMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/prune", nil)
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "for_future_use" {
		t.Errorf("expected status for_future_use, got %q", body["status"])
	}
}

func TestPruneEndpointRequiresAuth(t *testing.T) {
	s, _ := newTestServer(t)
	mux := registerAdminMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/prune", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestManifestsEndpointListsStoredRequirements(t *testing.T) {
	s, _ := newTestServer(t)

	rolesManifest := []byte("roles:\n  - name: geerlingguy.nginx\n    version: 1.2.3\n")
	if err := s.fetcher.SaveManifest("roles", rolesManifest); err != nil {
		t.Fatal(err)
	}
	if err := s.fetcher.SaveManifest("collections", []byte("collections:\n  - name: community.general\n    version: 8.4.0\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.fetcher.SaveManifest("collections", []byte("collections:\n  - name: community.docker\n    version: 3.6.0\n")); err != nil {
		t.Fatal(err)
	}

	mux := registerAdminMux(s)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/manifests", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET manifests: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Roles       []fetcher.ManifestMeta `json:"roles"`
		Collections []fetcher.ManifestMeta `json:"collections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(body.Roles) != 1 {
		t.Fatalf("expected 1 roles manifest, got %d", len(body.Roles))
	}
	if len(body.Collections) != 2 {
		t.Fatalf("expected 2 collections manifests, got %d", len(body.Collections))
	}

	got := body.Roles[0]
	if want := fmt.Sprintf("%x", sha256.Sum256(rolesManifest)); got.SHA256 != want {
		t.Errorf("sha256 mismatch: got %q want %q", got.SHA256, want)
	}
	if got.Type != "roles" {
		t.Errorf("expected type roles, got %q", got.Type)
	}
	if len(got.Roles) != 1 || got.Roles[0].Name != "geerlingguy.nginx" || got.Roles[0].Version != "1.2.3" {
		t.Errorf("roles manifest entries not parsed: %+v", got.Roles)
	}

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/manifests", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET manifests without auth: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
}

func TestStorageInventoryEndpoint(t *testing.T) {
	s, storage := newTestServer(t)

	// Roles: two cached versions. Only 1.0.0 is pinned by a manifest.
	roleDir := filepath.Join(storage, "roles", "acme.sample")
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if err := os.MkdirAll(filepath.Join(roleDir, v), 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(roleDir, "1.0.0", "main.yml"), []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "1.1.0", "main.yml"), []byte("y"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := s.fetcher.SaveManifest("roles", []byte("roles:\n  - name: acme.sample\n    version: 1.0.0\n")); err != nil {
		t.Fatal(err)
	}

	// Collections: a single artifact pinned by a manifest, plus a git clone dir.
	colDir := filepath.Join(storage, "collections", "community")
	if err := os.MkdirAll(filepath.Join(colDir, "git"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(colDir, "community-general-8.4.0.tar.gz"), []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := s.fetcher.SaveManifest("collections", []byte("collections:\n  - name: community.general\n    version: 8.4.0\n")); err != nil {
		t.Fatal(err)
	}

	mux := registerAdminMux(s)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/storage", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer valid-admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET storage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Roles       []storageItem `json:"roles"`
		Collections []storageItem `json:"collections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(body.Roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(body.Roles))
	}
	role := body.Roles[0]
	if role.Name != "acme.sample" {
		t.Errorf("expected role acme.sample, got %q", role.Name)
	}
	if len(role.Versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(role.Versions))
	}
	for _, v := range role.Versions {
		if v.Version == "1.0.0" && !v.Declared {
			t.Errorf("1.0.0 should be declared")
		}
		if v.Version == "1.1.0" && v.Declared {
			t.Errorf("1.1.0 should not be declared")
		}
		if v.SizeBytes == 0 {
			t.Errorf("expected non-zero size for %s", v.Version)
		}
	}

	if len(body.Collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(body.Collections))
	}
	col := body.Collections[0]
	if col.Name != "community.general" {
		t.Errorf("expected community.general, got %q", col.Name)
	}
	if len(col.Versions) != 1 || !col.Versions[0].Declared || col.Versions[0].Version != "8.4.0" {
		t.Errorf("collection versions not as expected: %+v", col.Versions)
	}

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/storage", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET storage without auth: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
}
