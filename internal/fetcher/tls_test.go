package fetcher

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbitron/internal/config"
)

func writeServerCertToCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	der := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, der, 0600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return path
}

func TestFetcherRejectsUntrustedTLSServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{})
	resp, err := f.httpClient.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected TLS error for untrusted self-signed server")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected certificate error, got: %v", err)
	}
}

func TestFetcherTrustsConfiguredCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caFile := writeServerCertToCAFile(t, srv)
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{CAFile: caFile})
	resp, err := f.httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("request with pinned CA failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}

func TestFetcherSkipsTLSVerificationWhenConfigured(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{InsecureSkipVerify: true})
	resp, err := f.httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("request with skip-verify failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}

func TestGitEnvExportsTLSPolicy(t *testing.T) {
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{}, config.TLSConfig{
		CAFile:             "/etc/orbitron/ca.crt",
		InsecureSkipVerify: true,
	})
	env := f.gitEnv()
	for _, want := range []string{
		"GIT_SSL_CAINFO=/etc/orbitron/ca.crt",
		"GIT_SSL_NO_VERIFY=true",
	} {
		if !containsStr(env, want) {
			t.Fatalf("git env missing %q; got %v", want, env)
		}
	}
}
