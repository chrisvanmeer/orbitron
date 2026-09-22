package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"orbitron/internal/auth"
	"orbitron/internal/config"
	"orbitron/internal/oidc"
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

	d := NewDashboard(&config.Config{StoragePath: storage}, nil, nil)
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

// TestLogSourceReflectsConfig verifies that the SYSTEM LOGS panel shows the
// configured log path when set, and an explicit stdout label (never a
// hardcoded file path) when log_path is empty such as under Docker/Nomad.
func TestLogSourceReflectsConfig(t *testing.T) {
	d := NewDashboard(&config.Config{}, nil, nil)

	if got, want := d.logSource(), stdoutLogLabel; got != want {
		t.Fatalf("empty log_path: logSource()=%q, want %q", got, want)
	}

	const custom = "/var/log/orbitron/orbitron.log"
	d.cfg.LogPath = custom
	if got := d.logSource(); got != custom {
		t.Fatalf("set log_path: logSource()=%q, want %q", got, custom)
	}
}

// renderLoginBody renders the login page into a string using the provided
// configuration.
func renderLoginBody(cfg *config.Config) string {
	d := NewDashboard(cfg, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/login?error=oidc", nil)
	d.handleLogin(rec, req)
	return rec.Body.String()
}

// TestLoginPageWithoutOIDC verifies the login page keeps the classic single
// "Establish Link" token button and no SSO affordance when OIDC is disabled.
func TestLoginPageWithoutOIDC(t *testing.T) {
	body := renderLoginBody(&config.Config{})
	if !strings.Contains(body, ">Establish Link<") {
		t.Fatalf("expected 'Establish Link' button when OIDC disabled\n%s", body)
	}
	if strings.Contains(body, "Authenticate with Token") {
		t.Fatalf("token button must not be renamed when OIDC disabled\n%s", body)
	}
	if strings.Contains(body, `class="oidc-btn"`) {
		t.Fatalf("SSO button must not render when OIDC disabled\n%s", body)
	}
}

// TestLoginPageWithOIDC verifies that enabling OIDC renames the token button
// to "Authenticate with Token" and adds a distinct neon SSO button next to it,
// and that an SSO failure surfaces the dedicated access-denied message.
func TestLoginPageWithOIDC(t *testing.T) {
	cfg := &config.Config{OIDC: config.OIDCConfig{
		Enabled:         true,
		Issuer:          "https://keycloak.example.org/realms/orbitron",
		ClientID:        "orbitron",
		SessionTTLHours: 8,
	}}
	body := renderLoginBody(cfg)

	if !strings.Contains(body, ">Authenticate with Token<") {
		t.Fatalf("expected renamed token button when OIDC enabled\n%s", body)
	}
	if !strings.Contains(body, `class="oidc-btn" href="/ui/oidc/start"`) {
		t.Fatalf("expected SSO button linking to /ui/oidc/start\n%s", body)
	}
	if !strings.Contains(body, ">SSO Login<") {
		t.Fatalf("expected 'SSO Login' button label\n%s", body)
	}
	if !strings.Contains(body, "[ SSO ACCESS DENIED ]") {
		t.Fatalf("expected SSO access-denied message (via ?error=oidc)\n%s", body)
	}
	if strings.Contains(body, "Establish Link") {
		t.Fatalf("'Establish Link' must be replaced when OIDC enabled\n%s", body)
	}
}

// TestRequireAuthAcceptsOIDCSession verifies the dashboard grants access to
// requests carrying a valid SSO session cookie (no admin token present).
func TestRequireAuthAcceptsOIDCSession(t *testing.T) {
	sessions := auth.NewSessionManager(0)
	session, err := sessions.Create()
	if err != nil {
		t.Fatal(err)
	}

	d := NewDashboard(&config.Config{TokensFile: filepath.Join(t.TempDir(), "tokens.json")}, nil, sessions)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/storage", nil)
	req.AddCookie(&http.Cookie{Name: OIDCSessionCookie, Value: session})

	d.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("requireAuth rejected valid OIDC session (code=%d)", rec.Code)
	}
}

func TestOIDCAuthorizedAllowsGroupMember(t *testing.T) {
	d := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"orbitron-admins", "ops"}}}, nil, nil)
	claims := &oidc.Claims{Groups: []string{"ops", "other"}}
	if !d.oidcAuthorized(claims) {
		t.Fatal("member of an allowed group should be admitted")
	}
}

func TestOIDCAuthorizedDeniesNonMember(t *testing.T) {
	d := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"orbitron-admins"}}}, nil, nil)
	claims := &oidc.Claims{Groups: []string{"other-group"}}
	if d.oidcAuthorized(claims) {
		t.Fatal("non-member must be denied")
	}
}

func TestOIDCAuthorizedDeniesMissingGroupsClaim(t *testing.T) {
	d := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"orbitron-admins"}}}, nil, nil)
	if d.oidcAuthorized(&oidc.Claims{}) {
		t.Fatal("filter must fail closed when the ID token has no groups claim")
	}
}

func TestOIDCAuthorizedMatchesLeadingSlashPath(t *testing.T) {
	d := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"orbitron-admins"}}}, nil, nil)
	// Keycloak "Group Membership" mappers emit full paths (leading slash)
	// when "Full group path" is on.
	claims := &oidc.Claims{Groups: []string{"/orbitron-admins", "/other"}}
	if !d.oidcAuthorized(claims) {
		t.Fatal("claim with leading slash must match the plain allowed name")
	}
	// And a plain claim must match an allowed name written with a leading slash.
	claims = &oidc.Claims{Groups: []string{"orbitron-admins"}}
	d2 := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"/orbitron-admins"}}}, nil, nil)
	if !d2.oidcAuthorized(claims) {
		t.Fatal("plain claim must match an allowed name with leading slash")
	}
}

func TestOIDCAuthorizedMatchesNestedGroupLeaf(t *testing.T) {
	d := NewDashboard(&config.Config{OIDC: config.OIDCConfig{AllowedGroups: []string{"admins"}}}, nil, nil)
	claims := &oidc.Claims{Groups: []string{"/orbitron/devs/admins"}}
	if !d.oidcAuthorized(claims) {
		t.Fatal("last path segment of a nested group must match the allowed name")
	}
	// A nested claim must not grant access to a different leaf.
	if d.oidcAuthorized(&oidc.Claims{Groups: []string{"/orbitron/devs/users"}}) {
		t.Fatal("non-matching nested group leaf must be denied")
	}
}

func TestOIDCAuthorizedBypassWhenUnconfigured(t *testing.T) {
	d := NewDashboard(&config.Config{}, nil, nil)
	claims := &oidc.Claims{Groups: nil}
	if !d.oidcAuthorized(claims) {
		t.Fatal("no allowed_groups configured: everyone admitted")
	}
}

func TestIdentityForPicksMostReadablePrincipal(t *testing.T) {
	cases := []struct {
		want   string
		claims *oidc.Claims
	}{
		{"ops@example.org", &oidc.Claims{Email: "ops@example.org", PreferredUsername: "ops", Subject: "abc"}},
		{"ops", &oidc.Claims{PreferredUsername: "ops", Subject: "abc"}},
		{"abc", &oidc.Claims{Subject: "abc"}},
	}
	for _, tc := range cases {
		if got := identityFor(tc.claims); got != tc.want {
			t.Errorf("identityFor(%+v) = %q, want %q", tc.claims, got, tc.want)
		}
	}
}

// collectionSearchOrder extracts the data-search attributes of collection rows
// in document order, which is what the default (unsorted) table shows.
func collectionSearchOrder(body string) []string {
	re := regexp.MustCompile(`data-search="collection [^"]*"`)
	var out []string
	for _, m := range re.FindAllString(body, -1) {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(m, `data-search="`), `"`))
	}
	return out
}

// TestStorageCollectionOrderStableAcrossRefreshes guards against the previous
// random Go map iteration: the collections table is re-rendered on every 10s
// poll and, with the default (no-sort-column) view, must keep an identical
// alphabetical order in every request.
func TestStorageCollectionOrderStableAcrossRefreshes(t *testing.T) {
	storage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(storage, "collections/community"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storage, "collections/ansible"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"community-general-8.5.0.tar.gz",
		"community-aws-5.0.0.tar.gz",
		"ansible-posix-1.5.4.tar.gz",
		"ansible-utils-3.0.0.tar.gz",
	} {
		if err := os.WriteFile(filepath.Join(storage, "collections/"+strings.Split(f, "-")[0], f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d := NewDashboard(&config.Config{StoragePath: storage}, nil, nil)

	var want []string
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		d.handleStorage(rec, httptest.NewRequest("GET", "/ui/storage", nil))
		order := collectionSearchOrder(rec.Body.String())
		if len(order) != 4 {
			t.Fatalf("iteration %d: expected 4 collection rows, got %d\n%s", i, len(order), order)
		}
		if want == nil {
			want = order
		} else {
			for j := range want {
				if order[j] != want[j] {
					t.Fatalf("iteration %d: collection order jumped: got %v, want %v", i, order, want)
				}
			}
		}
	}

	for i := 1; i < len(want); i++ {
		if want[i-1] > want[i] {
			t.Fatalf("default collection order is not alphabetical: %v", want)
		}
	}
}

// TestHandleLogsTailsConfiguredLimit verifies the log viewer serves up to
// logTailLimit trailing lines as .log-line divs, newest-last, from the
// configured log file.
func TestHandleLogsTailsConfiguredLimit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "orbitron.log")
	var buf strings.Builder
	for i := 1; i <= logTailLimit+100; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	d := NewDashboard(&config.Config{LogPath: logPath}, nil, nil)
	rec := httptest.NewRecorder()
	d.handleLogs(rec, httptest.NewRequest("GET", "/ui/logs", nil))
	body := rec.Body.String()

	if got := strings.Count(body, `class="log-line"`); got != logTailLimit {
		t.Fatalf("handleLogs returned %d lines, want %d", got, logTailLimit)
	}
	first := logTailLimit + 1
	if !strings.Contains(body, fmt.Sprintf(">line %d<", first)) {
		t.Fatalf("expected tail to start at line %d (the oldest of the last %d)", first, logTailLimit)
	}
	if !strings.Contains(body, fmt.Sprintf(">line %d<", logTailLimit+100)) {
		t.Fatalf("expected the newest line %d in the tail", logTailLimit+100)
	}
	if strings.Contains(body, ">line 1<") {
		t.Fatalf("tail must not include lines older than the limit")
	}
}

// TestHandleLogsWithoutFile shows the container-logs hint (not the login page
// or an error) when log_path is empty.
func TestHandleLogsWithoutFile(t *testing.T) {
	d := NewDashboard(&config.Config{}, nil, nil)
	rec := httptest.NewRecorder()
	d.handleLogs(rec, httptest.NewRequest("GET", "/ui/logs", nil))
	if !strings.Contains(rec.Body.String(), "File logging is disabled") {
		t.Fatalf("empty log_path should explain logs go to the container runtime, got: %s", rec.Body.String())
	}
}

// TestTailFileHandlesLargeLineCounts verifies tailFile keeps working when the
// requested line count spans many times the initial 64KiB read window.
func TestTailFileHandlesLargeLineCounts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "orbitron.log")
	var buf strings.Builder
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := tailFile(logPath, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5000 {
		t.Fatalf("tailFile returned %d lines, want 5000", len(got))
	}
	if got[0] != "line 15001" || got[len(got)-1] != "line 20000" {
		t.Fatalf("unexpected tail content: first=%q last=%q", got[0], got[len(got)-1])
	}
}
