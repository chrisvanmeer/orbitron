package fetcher

import (
	"net/http"
	"testing"

	"orbitron/internal/config"
)

func TestParseNoProxyMatch(t *testing.T) {
	tests := []struct {
		name     string
		noProxy  string
		hostname string
		want     bool
	}{
		{name: "empty never matches", want: false},
		{name: "exact host", noProxy: "internal", hostname: "internal", want: true},
		{name: "different host", noProxy: "internal", hostname: "external", want: false},
		{name: "exact with port stripped", noProxy: "internal:3128", hostname: "internal", want: true},
		{name: "subdomain wildcard", noProxy: "*.example.com", hostname: "www.example.com", want: true},
		{name: "apex domain wildcard", noProxy: "*.example.com", hostname: "example.com", want: true},
		{name: "leading dot suffix", noProxy: ".example.com", hostname: "mirror.example.com", want: true},
		{name: "leading dot apex", noProxy: ".example.com", hostname: "example.com", want: true},
		{name: "unrelated suffix", noProxy: ".example.com", hostname: "notexample.com", want: false},
		{name: "case insensitive", noProxy: "INTERNAL", hostname: "Internal", want: true},
		{name: "catch all", noProxy: "*", hostname: "galaxy.ansible.com", want: true},
		{name: "cidr ipv4", noProxy: "10.0.0.0/8", hostname: "10.1.2.3", want: true},
		{name: "cidr ipv4 miss", noProxy: "10.0.0.0/8", hostname: "192.168.1.1", want: false},
		{name: "hostname not ip for cidr", noProxy: "10.0.0.0/8", hostname: "host.internal", want: false},
		{name: "list of entries", noProxy: "localhost,127.0.0.1,.internal", hostname: "squid.internal", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := parseNoProxy(tt.noProxy)
			if got := matchesNoProxy(entries, tt.hostname); got != tt.want {
				t.Errorf("matchesNoProxy(%q, %q) = %v, want %v", tt.noProxy, tt.hostname, got, tt.want)
			}
		})
	}
}

func TestProxyFuncSchemeSelection(t *testing.T) {
	p := ProxyConfig{
		HTTPProxy:  "http://squid.internal:3128",
		HTTPSProxy: "https://squid-ssl.internal:3129",
		NoProxy:    "internal.galaxy,127.0.0.1",
	}
	proxyFunc, err := p.proxyFunc()
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}

	tests := []struct {
		name string
		req  string
		want string
	}{
		{name: "https uses https proxy", req: "https://galaxy.ansible.com/api/", want: "https://squid-ssl.internal:3129"},
		{name: "http uses http proxy", req: "http://galaxy.ansible.com/api/", want: "http://squid.internal:3128"},
		{name: "no_proxy exact bypasses", req: "https://internal.galaxy/api/", want: ""},
		{name: "no_proxy ip bypasses", req: "http://127.0.0.1:8080/api/", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.req, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			gotURL, err := proxyFunc(req)
			if err != nil {
				t.Fatalf("proxyFunc() returned error: %v", err)
			}
			if tt.want == "" {
				if gotURL != nil {
					t.Errorf("expected direct (nil proxy), got %s", gotURL)
				}
				return
			}
			if gotURL == nil || gotURL.String() != tt.want {
				t.Errorf("expected proxy %q, got %v", tt.want, gotURL)
			}
		})
	}
}

func TestProxyFuncCrossSchemeFallback(t *testing.T) {
	p := ProxyConfig{HTTPProxy: "http://squid.internal:3128"}
	proxyFunc, err := p.proxyFunc()
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://galaxy.ansible.com/api/", nil)
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}
	if got == nil || got.String() != "http://squid.internal:3128" {
		t.Errorf("https should fall back to http proxy, got %v", got)
	}
}

func TestProxyFuncFallsBackToEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.internal:8080")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.internal:8080")
	t.Setenv("NO_PROXY", "127.0.0.1")

	// No explicit config: must honor the process environment.
	proxyFunc, err := ProxyConfig{}.proxyFunc()
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, "https://galaxy.ansible.com/api/", nil)
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}
	if got == nil || got.String() != "http://env-proxy.internal:8080" {
		t.Errorf("expected env proxy, got %v", got)
	}

	bypass, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/", nil)
	got, err = proxyFunc(bypass)
	if err != nil {
		t.Fatalf("proxyFunc() returned error: %v", err)
	}
	if got != nil {
		t.Errorf("expected direct for NO_PROXY host, got %v", got)
	}
}

func TestProxyConfigInvalidURL(t *testing.T) {
	p := ProxyConfig{HTTPProxy: "://missing-scheme"}
	if _, err := p.proxyFunc(); err == nil {
		t.Error("expected error for invalid proxy URL")
	}
}

func TestGitEnvExportsProxy(t *testing.T) {
	f := NewFetcher(t.TempDir(), 2, ProxyConfig{
		HTTPProxy:  "http://squid.internal:3128",
		HTTPSProxy: "http://squid.internal:3128",
		NoProxy:    "127.0.0.1,.internal",
	}, config.TLSConfig{})

	env := f.gitEnv()
	for _, want := range []string{
		"http_proxy=http://squid.internal:3128",
		"HTTP_PROXY=http://squid.internal:3128",
		"https_proxy=http://squid.internal:3128",
		"HTTPS_PROXY=http://squid.internal:3128",
		"no_proxy=127.0.0.1,.internal",
		"NO_PROXY=127.0.0.1,.internal",
	} {
		if !containsStr(env, want) {
			t.Errorf("git env missing %q (got %v)", want, env)
		}
	}
}

func containsStr(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
