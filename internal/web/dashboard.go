package web

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"orbitron/internal/access"
	"orbitron/internal/auth"
	"orbitron/internal/build"
	"orbitron/internal/config"
	"orbitron/internal/httputil"
	"orbitron/internal/logger"
	"orbitron/internal/oidc"
)

// OIDCSessionCookie is the cookie name backing browser sessions established
// via the OIDC (SSO) login flow. The same value satisfies the API
// authenticateRequest, mirroring how the token cookie leaks into the API.
const OIDCSessionCookie = "orbitron_oidc"

// pendingAuthLoginTTL bounds how long an initiated SSO hand-off may take
// before its state/nonce/PKCE values are discarded.
const pendingAuthLoginTTL = 5 * time.Minute

type sizeEntry struct {
	size int64
	ts   time.Time
}

// pendingAuth links an in-flight OIDC hand-off's state value to the PKCE and
// nonce material required to complete it. Entries expire after 5 minutes.
type pendingAuth struct {
	params  *oidc.AuthParams
	expires time.Time
}

type Dashboard struct {
	cfg       *config.Config
	sizeMu    sync.Mutex
	sizeCache map[string]sizeEntry

	oidc     *oidc.Client
	sessions *auth.SessionManager

	puMu    sync.Mutex
	pending map[string]pendingAuth
}

// NewDashboard wires the dashboard to the daemon configuration and, when OIDC
// is enabled, to the SSO client and session store. Both may be nil when SSO is
// disabled, in which case only token-based login is offered.
func NewDashboard(cfg *config.Config, oc *oidc.Client, sessions *auth.SessionManager) *Dashboard {
	return &Dashboard{
		cfg:       cfg,
		sizeCache: make(map[string]sizeEntry),
		oidc:      oc,
		sessions:  sessions,
		pending:   make(map[string]pendingAuth),
	}
}

// Register attaches all UI endpoints to the mux (without global API auth wrapper)
func (d *Dashboard) Register(mux *http.ServeMux) {
	// Serve embedded HTMX 4.0.0 directly from memory
	mux.HandleFunc("/ui/htmx.min.js", d.handleHtmx)

	// Public favicon (no auth — browsers request it before any token exists)
	mux.HandleFunc("/favicon.ico", d.handleFavicon)
	mux.HandleFunc("/favicon.svg", d.handleFavicon)

	// Auth routes
	mux.HandleFunc("/ui/login", d.handleLogin)
	mux.HandleFunc("/ui/logout", d.handleLogout)

	// OIDC (SSO) login flow
	mux.HandleFunc("/ui/oidc/start", d.handleOIDCStart)
	mux.HandleFunc("/ui/oidc/callback", d.handleOIDCCallback)

	// Protected UI routes
	mux.HandleFunc("/ui", d.requireAuth(d.handleIndex))
	mux.HandleFunc("/ui/storage", d.requireAuth(d.handleStorage))
	mux.HandleFunc("/ui/logs", d.requireAuth(d.handleLogs))
	mux.HandleFunc("/ui/sync", d.requireAuth(d.handleSyncTime))
}

// --- Middleware & Auth Logic ---

func (d *Dashboard) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authenticated := false
		if cookie, err := r.Cookie("orbitron_token"); err == nil {
			if store, err := auth.LoadTokens(d.cfg.TokensFile); err == nil {
				if store.Valid(cookie.Value) {
					authenticated = true
				}
			}
		}

		if !authenticated && d.sessions != nil {
			if cookie, err := r.Cookie(OIDCSessionCookie); err == nil {
				if d.sessions.Valid(cookie.Value) {
					authenticated = true
				}
			}
		}

		if !authenticated {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/ui")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			d.renderLogin(w, false, "")
			return
		}

		next(w, r)
	}
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if r.URL.Query().Get("error") == "oidc" {
			d.renderLogin(w, true, "[ SSO ACCESS DENIED ]")
			return
		}
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	valid := false

	if store, err := auth.LoadTokens(d.cfg.TokensFile); err == nil {
		if store.Valid(token) {
			valid = true
		}
	}

	if valid {
		logger.Info("Web UI session established (from %s)", clientIP(r))
		http.SetCookie(w, &http.Cookie{
			Name:     "orbitron_token",
			Value:    token,
			Path:     "/ui",
			MaxAge:   8 * 3600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
		return
	}

	logger.Warn("Web UI authentication failed (from %s)", clientIP(r))
	d.renderLogin(w, true, "")
}

// handleOIDCStart begins the OpenID Connect authorization-code flow: it
// generates fresh state/nonce/PKCE values, remembers them briefly, and
// redirects the browser to the IDP's authorization endpoint.
func (d *Dashboard) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if d.oidc == nil {
		http.NotFound(w, r)
		return
	}

	redirectURI := d.oidcRedirectURI(r)
	params, err := oidc.NewAuthParams(redirectURI)
	if err != nil {
		logger.Error("OIDC: failed to generate auth params: %v", err)
		http.Error(w, "Failed to start SSO login", http.StatusInternalServerError)
		return
	}

	// Sweep expired hand-offs before storing the new one so abandoned logins
	// cannot accumulate in memory.
	now := time.Now()
	d.puMu.Lock()
	for state, p := range d.pending {
		if now.After(p.expires) {
			delete(d.pending, state)
		}
	}
	d.pending[params.State] = pendingAuth{params: params, expires: now.Add(pendingAuthLoginTTL)}
	d.puMu.Unlock()

	authURL, err := d.oidc.AuthURL(redirectURI, params.State, params.Nonce, params.CodeVerifier)
	if err != nil {
		logger.Error("OIDC: failed to build authorization URL: %v", err)
		d.puMu.Lock()
		delete(d.pending, params.State)
		d.puMu.Unlock()
		http.Error(w, "Failed to contact identity provider", http.StatusBadGateway)
		return
	}

	logger.Info("Web UI OIDC login initiated (from %s)", clientIP(r))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback receives the IDP redirect, exchanges the authorization
// code for tokens, verifies the ID token and opens a dashboard session.
func (d *Dashboard) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if d.oidc == nil {
		http.NotFound(w, r)
		return
	}

	query := r.URL.Query()
	state := query.Get("state")
	code := query.Get("code")

	if errDesc := query.Get("error"); errDesc != "" {
		logger.Warn("OIDC: identity provider denied authorization: %s", errDesc)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	d.puMu.Lock()
	pending, ok := d.pending[state]
	if ok {
		delete(d.pending, state)
	}
	d.puMu.Unlock()

	if !ok || time.Now().After(pending.expires) {
		logger.Warn("OIDC: unknown or expired state from %s", clientIP(r))
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	claims, err := d.oidc.ExchangeCode(code, pending.params)
	if err != nil {
		logger.Warn("OIDC: token exchange/verification failed: %v", err)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	if !d.oidcAuthorized(claims) {
		logger.Warn("OIDC: user %s (groups %q) is not a member of an allowed group; denying access", identityFor(claims), claims.Groups)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	session, err := d.sessions.Create()
	if err != nil {
		logger.Error("OIDC: failed to create session: %v", err)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     OIDCSessionCookie,
		Value:    session,
		Path:     "/ui",
		MaxAge:   int(d.cfg.OIDC.SessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   r.URL.Scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})

	logger.Info("Web UI OIDC session established (user=%s from %s)", identityFor(claims), clientIP(r))
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

// oidcAuthorized applies the optional allowed_groups access filter. When no
// groups are configured every verified SSO user is admitted; otherwise the
// user must be a member of at least one allowed group. The check fails closed
// when the ID token carries no groups claim at all.
func (d *Dashboard) oidcAuthorized(claims *oidc.Claims) bool {
	allowed := d.cfg.OIDC.AllowedGroups
	if len(allowed) == 0 {
		return true
	}
	for _, want := range allowed {
		for _, g := range claims.Groups {
			if groupMatches(g, want) {
				return true
			}
		}
	}
	return false
}

// groupMatches compares a group claim value against an allowed group name.
// Keycloak's "Group Membership" mapper emits full group paths by default
// (e.g. "/admins" or "/orbitron/admins"), so a claim matches when it is
// exactly the allowed name, when they differ only by a leading slash, or when
// the last path segment of the claim equals the allowed name.
func groupMatches(claim, allowed string) bool {
	if claim == allowed {
		return true
	}
	trimmed := strings.TrimPrefix(claim, "/")
	if trimmed == allowed {
		return true
	}
	if strings.HasPrefix(allowed, "/") {
		allowed = strings.TrimPrefix(allowed, "/")
		if claim == allowed {
			return true
		}
	}
	if i := strings.LastIndex(trimmed, "/"); i >= 0 && trimmed[i+1:] == allowed {
		return true
	}
	return false
}

// identityFor picks a human-readable principal label from the ID token claims.
func identityFor(claims *oidc.Claims) string {
	switch {
	case claims.Email != "":
		return claims.Email
	case claims.PreferredUsername != "":
		return claims.PreferredUsername
	default:
		return claims.Subject
	}
}

// oidcRedirectURI returns the callback URL advertised to the IDP. A configured
// redirect_uri wins; otherwise it is derived from the incoming request,
// honoring X-Forwarded-Proto and direct TLS so the value matches what Keycloak
// has registered.
func (d *Dashboard) oidcRedirectURI(r *http.Request) string {
	if d.cfg.OIDC.RedirectURI != "" {
		return d.cfg.OIDC.RedirectURI
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/ui/oidc/callback"
}

func clientIP(r *http.Request) string {
	return httputil.ClientIP(r)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	logger.Info("Web UI session terminated (from %s)", clientIP(r))

	// Revoke an OIDC session if the user came in through SSO.
	if d.sessions != nil {
		if cookie, err := r.Cookie(OIDCSessionCookie); err == nil {
			d.sessions.Revoke(cookie.Value)
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "orbitron_token",
		Value:    "",
		Path:     "/ui",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     OIDCSessionCookie,
		Value:    "",
		Path:     "/ui",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

func (d *Dashboard) renderLogin(w http.ResponseWriter, hasError bool, errorText string) {
	display := "none"
	if hasError {
		display = "block"
	}
	if errorText == "" {
		errorText = "[ ACCESS DENIED: INVALID TOKEN ]"
	}

	oidcSection := ""
	tokenLabel := "Establish Link"
	if d.cfg.OIDC.EnabledAndConfigured() {
		tokenLabel = "Authenticate with Token"
		oidcSection = `<div class="or-divider"><span>OR</span></div>
        <a class="oidc-btn" href="/ui/oidc/start">SSO Login</a>`
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	htmlOut := loginTemplate
	htmlOut = strings.ReplaceAll(htmlOut, "{{THEME}}", uiThemeCSS)
	htmlOut = strings.ReplaceAll(htmlOut, "{{DISPLAY}}", display)
	htmlOut = strings.ReplaceAll(htmlOut, "{{ERROR_TEXT}}", errorText)
	htmlOut = strings.ReplaceAll(htmlOut, "{{OIDC_SECTION}}", oidcSection)
	htmlOut = strings.ReplaceAll(htmlOut, "{{TOKEN_LABEL}}", tokenLabel)
	_, _ = w.Write([]byte(htmlOut))
}

// --- HTML Templates ---

// uiThemeCSS is the shared design-token layer for every /ui page. It mirrors
// the marketing site palette (site/src/styles/global.css) so the dashboard and
// the website live in the same universe. The legacy --neon-* / --panel-bg /
// --bg-color aliases stay because the Go row renderers and partials still emit
// inline styles that reference them.
const uiThemeCSS = `
:root {
    --bg: #050505;
    --bg-deep: #030304;
    --bg-panel: #09090f;
    --bg-card: #0d0e16;
    --bg-card-hover: #12131d;
    --border: rgba(139, 148, 158, 0.14);
    --border-strong: rgba(139, 148, 158, 0.28);

    --cyan: #00f0ff;
    --pink: #ff007a;
    --red: #ff003c;
    --yellow: #fcee0a;
    --violet: #5773ff;
    --indigo: #a5b4fc;

    --text: #e7ecf3;
    --text-mid: #b6c2cf;
    --text-dim: #8b949e;
    --text-faint: #5c6b78;

    --font-sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif;
    --font-mono: ui-monospace, 'SF Mono', SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace;

    --radius-sm: 8px;
    --radius: 16px;
    --radius-lg: 22px;

    --bg-color: var(--bg);
    --panel-bg: var(--bg-panel);
    --neon-cyan: var(--cyan);
    --neon-pink: var(--pink);
    --neon-yellow: var(--yellow);
}

* { box-sizing: border-box; }
html, body { height: 100%; }
body {
    margin: 0;
    background: var(--bg);
    color: var(--text);
    font-family: var(--font-sans);
    line-height: 1.6;
    -webkit-font-smoothing: antialiased;
    text-rendering: optimizeLegibility;
}
::selection { background: var(--pink); color: #fff; }
::-webkit-scrollbar { width: 10px; height: 10px; }
::-webkit-scrollbar-thumb { background: rgba(139, 148, 158, 0.25); border: 2px solid transparent; border-radius: 6px; background-clip: content-box; }
::-webkit-scrollbar-track { background: transparent; }
:focus-visible { outline: 2px solid var(--cyan); outline-offset: 2px; }
button { font-family: inherit; cursor: pointer; }
h1, h2, h3, h4 { margin: 0; letter-spacing: -0.02em; }

/* Ambient glow + starfield backdrop (mirrors the marketing site) */
body::before {
    content: ''; position: fixed; inset: 0; z-index: -2;
    background:
        radial-gradient(1100px 520px at 50% -180px, rgba(87, 115, 255, 0.18), transparent 70%),
        radial-gradient(900px 480px at 85% 12%, rgba(0, 240, 255, 0.07), transparent 65%),
        radial-gradient(760px 420px at 8% 30%, rgba(255, 0, 122, 0.06), transparent 60%),
        var(--bg);
}
body::after {
    content: ''; position: fixed; inset: 0; z-index: -1; pointer-events: none;
    background-image:
        radial-gradient(1px 1px at 12% 22%, rgba(255, 255, 255, 0.5), transparent),
        radial-gradient(1px 1px at 32% 8%, rgba(255, 255, 255, 0.35), transparent),
        radial-gradient(1.5px 1.5px at 55% 15%, rgba(252, 238, 10, 0.5), transparent),
        radial-gradient(1px 1px at 72% 5%, rgba(255, 255, 255, 0.4), transparent),
        radial-gradient(1px 1px at 88% 20%, rgba(255, 255, 255, 0.3), transparent),
        radial-gradient(1px 1px at 7% 48%, rgba(255, 255, 255, 0.35), transparent),
        radial-gradient(1.5px 1.5px at 94% 42%, rgba(0, 240, 255, 0.45), transparent),
        radial-gradient(1px 1px at 42% 90%, rgba(255, 255, 255, 0.3), transparent),
        radial-gradient(1px 1px at 64% 78%, rgba(165, 180, 252, 0.45), transparent);
    background-size: 180px 160px;
}
`

const loginTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>ORBITRON // AUTHENTICATION</title>
    <link rel="icon" type="image/svg+xml" href="/favicon.svg">
    <style>
        {{THEME}}
        body {
            display: flex; align-items: center; justify-content: center;
            min-height: 100vh; padding: 24px;
        }
        .login-box {
            width: min(420px, 100%);
            background: linear-gradient(180deg, var(--bg-card) 0%, var(--bg-panel) 100%);
            border: 1px solid var(--border);
            border-radius: var(--radius);
            position: relative;
            padding: 44px 40px;
            text-align: center;
            box-shadow: 0 40px 90px -40px rgba(0, 0, 0, 0.9), 0 0 44px -14px rgba(0, 240, 255, 0.25);
        }
        .login-box::before {
            content: ''; position: absolute; inset: 0; border-radius: inherit; padding: 1px;
            background: linear-gradient(135deg, rgba(0, 240, 255, 0.35), transparent 40%, transparent 60%, rgba(255, 0, 122, 0.3));
            -webkit-mask: linear-gradient(#fff 0 0) content-box, linear-gradient(#fff 0 0);
            -webkit-mask-composite: xor;
            mask-composite: exclude;
            pointer-events: none;
        }
        .login-logo { width: 92px; height: auto; margin: 0 auto 22px; display: block; filter: drop-shadow(0 0 10px rgba(0, 240, 255, 0.35)); }
        h1 {
            font-family: var(--font-mono); font-size: 1.5rem; font-weight: 700;
            letter-spacing: 0.22em; text-transform: uppercase; margin: 0 0 26px;
            background: linear-gradient(90deg, #fff 10%, var(--indigo));
            -webkit-background-clip: text; background-clip: text; -webkit-text-fill-color: transparent;
        }
        input[type="password"] {
            width: 100%; padding: 12px 14px; margin: 4px 0 22px;
            background: var(--bg-deep); color: var(--text);
            border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
            font-family: var(--font-mono); text-align: center; font-size: 1em;
            letter-spacing: 0.12em; outline: none;
            transition: border-color 0.2s ease, box-shadow 0.2s ease;
        }
        input[type="password"]:focus { border-color: var(--cyan); box-shadow: 0 0 0 3px rgba(0, 240, 255, 0.15); }
        button, .oidc-btn {
            display: inline-flex; align-items: center; justify-content: center; gap: 0.5rem; width: 100%;
            font-family: var(--font-mono); font-size: 0.82rem; font-weight: 600; letter-spacing: 0.04em;
            padding: 0.8rem 1.4rem; border-radius: var(--radius-sm);
            border: 1px solid transparent; cursor: pointer; text-decoration: none;
            white-space: nowrap; text-transform: uppercase;
            transition: transform 0.15s ease, box-shadow 0.2s ease, background 0.2s ease, border-color 0.2s ease;
        }
        button[type="submit"] {
            background: linear-gradient(90deg, var(--cyan), var(--violet)); color: #04111a; font-weight: 700;
            box-shadow: 0 0 0 1px rgba(0, 240, 255, 0.4), 0 12px 34px -12px rgba(0, 240, 255, 0.55);
        }
        button[type="submit"]:hover { transform: translateY(-2px); box-shadow: 0 0 0 1px rgba(0, 240, 255, 0.7), 0 16px 44px -10px rgba(0, 240, 255, 0.7); }
        .oidc-btn { border-color: var(--border-strong); background: rgba(13, 14, 22, 0.6); color: var(--text); }
        .oidc-btn:hover { border-color: var(--pink); color: var(--pink); transform: translateY(-2px); }
        .or-divider {
            display: flex; align-items: center; gap: 12px; margin: 24px 0 14px;
            color: var(--text-faint); font-family: var(--font-mono);
            font-size: 0.7rem; letter-spacing: 0.18em;
        }
        .or-divider::before, .or-divider::after { content: ''; flex: 1; height: 1px; background: var(--border); }
        .error-msg {
            color: var(--red); font-size: 0.85rem; font-family: var(--font-mono);
            letter-spacing: 0.06em; font-weight: 600; margin-bottom: 12px; display: {{DISPLAY}};
        }
    </style>
</head>
<body>
    <div class="login-box">
        <svg class="login-logo" viewBox="-100 -100 200 200" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
            <defs>
                <linearGradient id="orbit1" x1="0%" y1="0%" x2="100%" y2="100%">
                    <stop offset="0%" stop-color="#00F0FF" />
                    <stop offset="100%" stop-color="#5773FF" />
                </linearGradient>
                <linearGradient id="orbit2" x1="0%" y1="0%" x2="100%" y2="100%">
                    <stop offset="0%" stop-color="#5773FF" />
                    <stop offset="100%" stop-color="#FF007A" />
                </linearGradient>
                <linearGradient id="orbit3" x1="0%" y1="0%" x2="100%" y2="100%">
                    <stop offset="0%" stop-color="#FF007A" />
                    <stop offset="100%" stop-color="#00F0FF" />
                </linearGradient>
                <filter id="glow" x="-40%" y="-40%" width="180%" height="180%">
                    <feGaussianBlur stdDeviation="4" result="blur" />
                    <feMerge>
                        <feMergeNode in="blur" />
                        <feMergeNode in="SourceGraphic" />
                    </feMerge>
                </filter>
            </defs>
            <ellipse cx="0" cy="0" rx="80" ry="30" fill="none" stroke="url(#orbit1)" stroke-width="4" transform="rotate(-30)" opacity="0.8" />
            <ellipse cx="0" cy="0" rx="80" ry="30" fill="none" stroke="url(#orbit2)" stroke-width="4" transform="rotate(30)" opacity="0.8" />
            <ellipse cx="0" cy="0" rx="80" ry="30" fill="none" stroke="url(#orbit3)" stroke-width="4" transform="rotate(90)" opacity="0.8" />
            <circle cx="69" cy="-40" r="6" fill="#00F0FF" filter="url(#glow)" />
            <circle cx="69" cy="40" r="6" fill="#FF007A" filter="url(#glow)" />
            <circle cx="0" cy="80" r="6" fill="#5773FF" filter="url(#glow)" />
            <circle cx="-69" cy="40" r="4" fill="#00F0FF" opacity="0.6" />
            <circle cx="-69" cy="-40" r="4" fill="#FF007A" opacity="0.6" />
            <circle cx="0" cy="-80" r="4" fill="#5773FF" opacity="0.6" />
            <circle cx="0" cy="0" r="14" fill="#FFFFFF" filter="url(#glow)" />
            <circle cx="0" cy="0" r="6" fill="#090A0F" />
        </svg>
        <h1>ORBITRON</h1>
        <div class="error-msg">{{ERROR_TEXT}}</div>
        <form method="POST" action="/ui/login">
            <input type="password" name="token" placeholder="ENTER ACCESS TOKEN" required autofocus>
            <br>
            <button type="submit">{{TOKEN_LABEL}}</button>
        </form>
        {{OIDC_SECTION}}
    </div>
</body>
</html>
`

const htmlTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>ORBITRON // TERMINAL</title>
    <link rel="icon" type="image/svg+xml" href="/favicon.svg">
    <!-- 100% Offline / Island-mode HTMX 4.0.0 -->
    <script src="/ui/htmx.min.js"></script>
    <style>
        {{THEME}}
        body {
            overflow: hidden; height: 100vh; width: 100vw;
        }

        /* Fullscreen Layout */
        .app-layout {
            display: flex; flex-direction: column; height: 100vh; width: 100vw; position: relative;
        }
        .header-bar {
            display: flex; justify-content: space-between; align-items: center;
            border-bottom: 1px solid var(--border); padding: 14px 24px;
            position: relative; overflow: hidden; z-index: 10;
            background-color: rgba(5, 5, 5, 0.72);
            /* Soft galaxy + starfield, mirroring site/src/components/Hero.astro. */
            background-image:
                radial-gradient(520px 210px at 12% -60px, rgba(87, 115, 255, 0.18), transparent 70%),
                radial-gradient(400px 180px at 34% -40px, rgba(0, 240, 255, 0.07), transparent 65%),
                radial-gradient(1px 1px at 8% 35%, rgba(190, 252, 255, 0.5), transparent),
                radial-gradient(1px 1px at 18% 72%, rgba(255, 255, 255, 0.35), transparent),
                radial-gradient(1.5px 1.5px at 30% 24%, rgba(0, 240, 255, 0.5), transparent),
                radial-gradient(1px 1px at 46% 62%, rgba(255, 255, 255, 0.3), transparent),
                radial-gradient(1px 1px at 62% 30%, rgba(190, 252, 255, 0.45), transparent),
                radial-gradient(1px 1px at 78% 72%, rgba(252, 238, 10, 0.4), transparent),
                radial-gradient(1.5px 1.5px at 90% 28%, rgba(0, 240, 255, 0.45), transparent),
                radial-gradient(1px 1px at 96% 55%, rgba(255, 255, 255, 0.3), transparent);
            background-size: auto, auto, 200px 180px, 200px 180px, 200px 180px, 200px 180px, 200px 180px, 200px 180px, 200px 180px, 200px 180px;
            background-repeat: no-repeat, no-repeat, repeat, repeat, repeat, repeat, repeat, repeat, repeat, repeat;
            backdrop-filter: blur(14px); -webkit-backdrop-filter: blur(14px);
        }
        /* Slowly rotating orbital rings, like the site hero. */
        .header-galaxy {
            position: absolute; left: 5%; top: 50%; width: 250px; height: 88px;
            transform: translateY(-50%); pointer-events: none; z-index: 0; opacity: 0.9;
        }
        .header-galaxy .g-ring { position: absolute; inset: 0; border-radius: 50%; }
        .g-ring-a { border: 1px dashed rgba(0, 240, 255, 0.28); transform: rotate(-12deg); animation: hdr-drift 26s linear infinite; }
        .g-ring-b { border: 1px dashed rgba(255, 0, 122, 0.22); inset: 6px -14px; transform: rotate(7deg); animation: hdr-drift 22s linear infinite reverse; }
        .g-ring-c { border: 1px solid rgba(165, 180, 252, 0.12); inset: -3px 10px; transform: rotate(2deg); }
        @keyframes hdr-drift { to { transform: rotate(346deg); } }
        .brand { display: inline-flex; align-items: center; gap: 0.7rem; position: relative; z-index: 1; }
        .header-logo { width: 28px; height: 28px; filter: drop-shadow(0 0 8px rgba(0, 240, 255, 0.4)); }
        h1 {
            margin: 0; font-size: 0.95rem; font-weight: 900;
            letter-spacing: 0.24em; text-transform: uppercase;
            display: inline-flex; align-items: center; gap: 0.9rem;
        }
        .brand-name {
            background: linear-gradient(90deg, #fff, var(--indigo));
            -webkit-background-clip: text; background-clip: text; -webkit-text-fill-color: transparent;
        }
        .brand-sub {
            font-family: var(--font-mono); font-size: 0.78rem; font-weight: 600;
            letter-spacing: 0.1em; color: var(--text-dim);
        }
        .header-actions {
            display: flex; align-items: center; gap: 12px; position: relative; z-index: 1;
        }
        .btn-metrics {
            background: rgba(13, 14, 22, 0.6); color: var(--text); border: 1px solid var(--border-strong);
            border-radius: var(--radius-sm); padding: 8px 14px; cursor: pointer; font-family: var(--font-mono);
            font-size: 0.76rem; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase;
            transition: border-color 0.2s ease, color 0.2s ease, transform 0.15s ease;
        }
        .btn-metrics:hover { border-color: var(--cyan); color: var(--cyan); transform: translateY(-1px); }

        .btn-logout {
            color: var(--text); text-decoration: none; background: transparent;
            border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
            padding: 8px 14px; font-size: 0.76rem; font-weight: 600; letter-spacing: 0.08em;
            cursor: pointer; font-family: var(--font-mono); text-transform: uppercase;
            transition: border-color 0.2s ease, color 0.2s ease, background 0.2s ease, box-shadow 0.2s ease;
        }
        .btn-logout:hover { border-color: var(--red); color: var(--red); }
        .btn-logout.armed { background: var(--red); border-color: var(--red); color: #000; box-shadow: 0 0 20px rgba(255, 0, 60, 0.45); }

        /* Main Workspace Card */
        .main-workspace {
            flex: 1; margin: 20px; overflow-y: auto; position: relative; z-index: 1;
            border: 1px solid var(--border); border-radius: var(--radius);
            padding: 24px;
            /* Space backdrop mirroring site/src/styles/global.css (glows + starfield), kept under the opaque card gradient. */
            background-image:
                radial-gradient(1100px 520px at 50% -180px, rgba(87, 115, 255, 0.16), transparent 70%),
                radial-gradient(700px 380px at 90% 10%, rgba(0, 240, 255, 0.07), transparent 65%),
                radial-gradient(600px 340px at 5% 30%, rgba(255, 0, 122, 0.05), transparent 60%),
                radial-gradient(1px 1px at 12% 22%, rgba(255, 255, 255, 0.45), transparent),
                radial-gradient(1px 1px at 32% 8%, rgba(255, 255, 255, 0.32), transparent),
                radial-gradient(1.5px 1.5px at 55% 15%, rgba(252, 238, 10, 0.45), transparent),
                radial-gradient(1px 1px at 72% 5%, rgba(255, 255, 255, 0.38), transparent),
                radial-gradient(1px 1px at 88% 20%, rgba(255, 255, 255, 0.28), transparent),
                radial-gradient(1px 1px at 6% 78%, rgba(165, 180, 252, 0.42), transparent),
                radial-gradient(1.5px 1.5px at 92% 45%, rgba(0, 240, 255, 0.42), transparent),
                radial-gradient(1px 1px at 38% 92%, rgba(255, 255, 255, 0.30), transparent),
                radial-gradient(1px 1px at 63% 70%, rgba(165, 180, 252, 0.36), transparent),
                linear-gradient(180deg, var(--bg-card) 0%, var(--bg-panel) 100%);
            background-size: auto, auto, auto, 180px 160px, 180px 160px, 180px 160px, 180px 160px, 180px 160px, 180px 160px, 180px 160px, 180px 160px, 180px 160px, auto;
            background-repeat: no-repeat, no-repeat, no-repeat, repeat, repeat, repeat, repeat, repeat, repeat, repeat, repeat, repeat, no-repeat;
        }
        .main-workspace h2, #right-drawer h3, #bottom-drawer h3 {
            font-family: var(--font-mono); font-size: 0.76rem; font-weight: 700;
            letter-spacing: 0.18em; text-transform: uppercase; color: var(--cyan);
        }

        /* Tables */
        table { width: 100%; border-collapse: collapse; margin-top: 14px; }
        th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid var(--border); }
        th {
            font-family: var(--font-mono); font-size: 0.7rem; font-weight: 700;
            letter-spacing: 0.12em; text-transform: uppercase; color: var(--text-dim);
            border-bottom: 1px solid var(--border-strong);
        }
        tr:hover { background: rgba(0, 240, 255, 0.04); }

        /* Header Log Toggle (kept in the action cluster so it never overlaps content) */
        .btn-log-toggle {
            background: rgba(13, 14, 22, 0.6); color: var(--text); border: 1px solid var(--border-strong);
            border-radius: var(--radius-sm); padding: 8px 14px; cursor: pointer; font-family: var(--font-mono);
            font-size: 0.76rem; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase;
            transition: border-color 0.2s ease, color 0.2s ease, transform 0.15s ease;
        }
        .btn-log-toggle:hover { border-color: var(--pink); color: var(--pink); transform: translateY(-1px); }

        /* Right Telemetry Drawer */
        #right-drawer {
            position: fixed; top: 0; right: -360px; width: 360px; height: 100vh;
            background-image:
                radial-gradient(1px 1px at 18% 14%, rgba(255, 255, 255, 0.4), transparent),
                radial-gradient(1px 1px at 74% 8%, rgba(255, 255, 255, 0.3), transparent),
                radial-gradient(1.5px 1.5px at 88% 62%, rgba(0, 240, 255, 0.4), transparent),
                radial-gradient(1px 1px at 30% 88%, rgba(165, 180, 252, 0.4), transparent),
                linear-gradient(180deg, var(--bg-card) 0%, var(--bg-panel) 100%);
            background-size: 180px 160px, 180px 160px, 180px 160px, 180px 160px, auto;
            background-repeat: repeat, repeat, repeat, repeat, no-repeat;
            border-left: 1px solid var(--border-strong);
            box-shadow: -30px 0 60px -40px rgba(0, 0, 0, 0.9);
            transition: right 0.3s ease;
            z-index: 25; padding: 24px; overflow-y: auto;
        }
        #right-drawer.open { right: 0; }

        /* Bottom Log Drawer */
        #bottom-drawer {
            position: fixed; bottom: -38vh; left: 0; width: 100vw; height: 38vh;
            background-color: var(--bg-deep);
            background-image:
                radial-gradient(1px 1px at 10% 18%, rgba(255, 255, 255, 0.35), transparent),
                radial-gradient(1px 1px at 86% 22%, rgba(0, 240, 255, 0.35), transparent),
                radial-gradient(1.5px 1.5px at 48% 80%, rgba(252, 238, 10, 0.35), transparent);
            background-size: 180px 160px, 180px 160px, 180px 160px;
            background-repeat: repeat, repeat, repeat;
            border-top: 1px solid var(--border-strong);
            border-radius: var(--radius-lg) var(--radius-lg) 0 0;
            box-shadow: 0 -24px 60px -40px rgba(0, 0, 0, 0.9), 0 0 0 1px rgba(0, 240, 255, 0.06);
            transition: bottom 0.3s ease;
            z-index: 15; padding: 18px 24px 24px; display: flex; flex-direction: column;
        }
        #bottom-drawer.open { bottom: 0; }

        .log-viewer {
            flex: 1; background: var(--bg-deep); color: var(--text-mid); padding: 14px 16px;
            overflow-y: auto; border: 1px solid var(--border); border-radius: var(--radius-sm);
            font-family: var(--font-mono); font-size: 0.82rem; line-height: 1.55; margin-top: 12px;
            white-space: pre-wrap; word-break: break-all;
        }
        .log-line { margin: 0; }

        .stat-label { color: var(--text-dim); font-size: 0.7rem; font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.14em; margin-top: 18px; }
        .stat-value { color: var(--text); font-size: 0.95rem; font-family: var(--font-mono); font-weight: 500; margin-top: 4px; word-break: break-all; }

        .blink { animation: blinker 1.2s step-end infinite; color: var(--cyan); -webkit-text-fill-color: var(--cyan); }
        @keyframes blinker { 50% { opacity: 0; } }

        /* Secret mode (type "orbitron") */
        #orbitron-secret {
            display: none; position: fixed; inset: 0; z-index: 9998;
            pointer-events: none; color: var(--pink);
            font-family: var(--font-mono);
        }
        #orbitron-secret.show { display: block; animation: secret-fade 4s ease forwards; }
        #orbitron-secret pre {
            margin: 0; padding: 14px 18px; text-align: center;
            font-size: 2.2em; font-weight: bold; letter-spacing: 4px;
            color: var(--yellow); text-shadow: 0 0 12px var(--pink), 0 0 32px rgba(0,240,255,0.6);
        }
        #orbitron-secret .secret-sub {
            display: block; margin-top: 10px; font-size: 0.55em; letter-spacing: 6px;
            color: var(--cyan); text-shadow: 0 0 10px rgba(0,240,255,0.8);
        }
        @keyframes secret-fade {
            0%   { opacity: 0; filter: blur(4px); }
            1%   { opacity: 1; filter: blur(0); }
            75%  { opacity: 1; filter: blur(0); }
            100% { opacity: 0; filter: blur(8px); }
        }
        @keyframes secret-shake {
            0%, 100% { transform: translateX(0); }
            20% { transform: translate(-4px, 2px); }
            40% { transform: translate(4px, -2px); }
            60% { transform: translate(-3px, -2px); }
            80% { transform: translate(3px, 2px); }
        }
        .app-layout.shaking { animation: secret-shake 0.35s ease; }
    </style>
</head>
<body>
    <div class="app-layout">
        <div id="orbitron-secret">
            <pre>&lt; O R B I T R O N /&gt;
                <span class="secret-sub">// IT'S ORBITRONING TIME - CHRIS VAN MEER</span>
            </pre>
        </div>
        <div class="header-bar">
            <div class="header-galaxy" aria-hidden="true">
                <span class="g-ring g-ring-a"></span>
                <span class="g-ring g-ring-b"></span>
                <span class="g-ring g-ring-c"></span>
            </div>
            <div class="brand">
                <svg class="header-logo" viewBox="0 0 60 60" width="28" height="28" aria-hidden="true">
                    <circle cx="30" cy="30" r="15" fill="none" stroke="#00f0ff" stroke-width="2.5" />
                    <ellipse cx="30" cy="30" rx="25" ry="9" fill="none" stroke="#ff007a" stroke-width="2" transform="rotate(-24 30 30)" />
                    <ellipse cx="30" cy="30" rx="25" ry="9" fill="none" stroke="#fcee0a" stroke-width="1.4" transform="rotate(38 30 30)" opacity="0.9" />
                    <circle cx="46" cy="13" r="2.6" fill="#fcee0a" />
                    <circle cx="14" cy="47" r="2.2" fill="#ff007a" />
                </svg>
                <h1>
                    <span class="brand-name">Orbitron</span>
                    <span class="brand-sub">// Cache Matrix</span><span class="blink">_</span>
                </h1>
            </div>
            <div class="header-actions">
                <button class="btn-log-toggle btn-bottom-toggle" onclick="toggleBottomDrawer()">▲ LOG STREAM</button>
                <button class="btn-metrics" onclick="toggleRightDrawer()">◄ SYS METRICS</button>
                <button id="btn-disconnect" class="btn-logout" onclick="armDisconnect()">[ DISCONNECT ]</button>
            </div>
        </div>

        <!-- Fullscreen Local Cache Matrix -->
        <div class="main-workspace" hx-get="/ui/storage" hx-trigger="load, every 10s">
            <h2>[ Scanning Cache Matrix... ]</h2>
        </div>

        <!-- Right System Metrics Drawer -->
        <div id="right-drawer" hx-get="/ui/sync" hx-trigger="load, every 10s">
            <h3>[ Telemetry Stream... ]</h3>
        </div>

        <!-- Bottom Log Stream Drawer -->
        <div id="bottom-drawer">
            <div style="display:flex; justify-content:space-between; align-items:center;">
                <h3 style="margin:0;">SYSTEM LOGS // {{LOG_PATH}}</h3>
                <span style="color:var(--text-dim); font-size:0.78em; font-family:var(--font-mono); letter-spacing:0.08em; cursor:pointer; font-weight:600;" onclick="toggleBottomDrawer()">[ CLOSE ]</span>
            </div>
            <div class="log-viewer" hx-get="/ui/logs" hx-trigger="load, every 5s" hx-swap="none" hx-on::after:request="window.__appendLog(event.detail.ctx.text)" id="log-container">
                > Awaiting telemetry stream...
            </div>
        </div>
    </div>

    <script>
        function toggleRightDrawer() {
            const drawer = document.getElementById('right-drawer');
            drawer.classList.toggle('open');
        }

        let disconnectTimer = null;
        let disconnectArmed = false;

        function armDisconnect() {
            const btn = document.getElementById('btn-disconnect');
            if (disconnectArmed) {
                window.location.href = '/ui/logout';
                return;
            }
            disconnectArmed = true;
            btn.innerText = '[ CONFIRM DISCONNECT ]';
            btn.classList.add('armed');
            disconnectTimer = setTimeout(resetDisconnect, 5000);
        }

        function resetDisconnect() {
            const btn = document.getElementById('btn-disconnect');
            disconnectArmed = false;
            if (disconnectTimer) { clearTimeout(disconnectTimer); disconnectTimer = null; }
            if (btn) {
                btn.innerText = '[ DISCONNECT ]';
                btn.classList.remove('armed');
            }
        }

        document.addEventListener('click', function (e) {
            if (disconnectArmed && !e.target.closest('#btn-disconnect')) {
                resetDisconnect();
            }
        }, true);

        function toggleBottomDrawer() {
            const drawer = document.getElementById('bottom-drawer');
            const btn = document.querySelector('.btn-bottom-toggle');
            drawer.classList.toggle('open');
            if(drawer.classList.contains('open')) {
                btn.innerText = '▼ HIDE LOGS';
            } else {
                btn.innerText = '▲ LOG STREAM';
            }
        }

        // Secret mode: typing "orbitron" (in quick succession) fires the easter egg.
        (function () {
            var seq = 'orbitron';
            var pos = 0;
            var lastTs = 0;
            document.addEventListener('keydown', function (e) {
                var now = Date.now();
                if (now - lastTs > 1200) pos = 0;
                lastTs = now;
                var ch = (e.key || String.fromCharCode(e.keyCode)).toLowerCase();
                if (ch !== seq[pos]) {
                    pos = (ch === seq[0]) ? 1 : 0;
                } else {
                    pos++;
                }
                if (pos >= seq.length) {
                    pos = 0;
                    var page = document.querySelector('.app-layout');
                    var box = document.getElementById('orbitron-secret');
                    if (page) {
                        page.classList.remove('shaking');
                        void page.offsetWidth;
                        page.classList.add('shaking');
                    }
                    if (box) {
                        box.classList.remove('show');
                        void box.offsetWidth;
                        box.classList.add('show');
                        setTimeout(function () { box.classList.remove('show'); }, 4200);
                    }
                }
                var title = document.querySelector('h1');
                if (title && pos > 0 && pos < seq.length) {
                    title.innerHTML = 'Orbitron // Cache Matrix<span class="blink">_' + ch.toUpperCase() + '</span>';
                    setTimeout(function () {
                        title.innerHTML = 'Orbitron // Cache Matrix<span class="blink">_</span>';
                    }, 600);
                }
            });
        })();

        // Log stream: keep an append-only session buffer so history survives
        // the 5s polls and the user can scroll back. New batches are appended
        // after the last already-rendered line (determined by an anchor match);
        // on log rotation the buffer is rebuilt from the fresh tail.
        (function () {
            const container = document.getElementById('log-container');
            if (!container) return;

            const MAX_BUF = 2000;
            let pinned = true;
            const bottomOf = () => container.scrollHeight - container.scrollTop - container.clientHeight < 40;
            container.addEventListener('scroll', function () {
                pinned = bottomOf();
            });

            const rePin = function () {
                if (pinned || bottomOf()) {
                    container.scrollTop = container.scrollHeight;
                    pinned = true;
                }
            };

            const renderLines = function (texts) {
                for (let i = 0; i < texts.length; i++) {
                    const div = document.createElement('div');
                    div.className = 'log-line';
                    div.textContent = texts[i];
                    container.appendChild(div);
                }
                const divs = container.querySelectorAll('.log-line');
                if (divs.length > MAX_BUF) {
                    for (let d = 0; d < divs.length - MAX_BUF; d++) {
                        if (divs[d].parentNode) divs[d].parentNode.removeChild(divs[d]);
                    }
                }
            };

            window.__appendLog = function (respText) {
                if (!respText || !respText.trim()) {
                    // Empty or blank response (e.g. a freshly created, still
                    // empty log file): never clobber already-rendered history,
                    // but if nothing has been drawn yet, hand the operator a
                    // clear status line instead of a dead placeholder.
                    if (container.querySelectorAll('.log-line').length === 0) {
                        container.textContent = '> No log data received yet...';
                    }
                    return;
                }
                const tmp = document.createElement('div');
                tmp.innerHTML = respText;
                const nodes = tmp.querySelectorAll('.log-line');
                if (nodes.length === 0) {
                    // Non-fragment response (e.g. the "file logging disabled"
                    // info line): show it verbatim until a real batch arrives.
                    if (container.querySelectorAll('.log-line').length === 0) {
                        container.textContent = respText;
                    }
                    return;
                }
                const texts = [];
                for (let i = 0; i < nodes.length; i++) texts.push(nodes[i].textContent);

                let buf = window.__logLines || [];
                if (buf.length === 0) {
                    // First batch: drop the placeholder and seed the buffer.
                    container.textContent = '';
                    buf = texts;
                    renderLines(texts);
                } else {
                    // Find our last rendered line inside the fresh tail and
                    // keep only what comes after it.
                    const anchor = buf[buf.length - 1];
                    let idx = -1;
                    for (let j = texts.length - 1; j >= 0; j--) {
                        if (texts[j] === anchor) { idx = j; break; }
                    }
                    if (idx < 0) {
                        // Anchor gone (log rotated / truncated): rebuild.
                        container.textContent = '';
                        buf = texts;
                        renderLines(texts);
                    } else {
                        const fresh = texts.slice(idx + 1);
                        if (fresh.length) {
                            buf = buf.concat(fresh);
                            renderLines(fresh);
                        }
                    }
                }
                if (buf.length > MAX_BUF) buf = buf.slice(buf.length - MAX_BUF);
                window.__logLines = buf;
                rePin();
            };

            if (typeof MutationObserver !== 'undefined') {
                new MutationObserver(rePin).observe(container, { childList: true, characterData: true, subtree: true });
            }
        })();
    </script>
</body>
</html>
`

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	logPath := strings.Replace(htmlTemplate, "{{LOG_PATH}}", d.logSource(), 1)
	logPath = strings.Replace(logPath, "{{THEME}}", uiThemeCSS, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(logPath))
}

// stdoutLogLabel is shown in the SYSTEM LOGS panel when the daemon writes its
// logs to stdout instead of a file (empty log_path, e.g. under Docker/Nomad).
const stdoutLogLabel = "STDOUT (container logs)"

// logSource describes where the daemon writes its logs for the SYSTEM LOGS
// panel. With a configured log_path that path is returned; otherwise it labels
// stdout honestly instead of inventing a file path that does not exist.
func (d *Dashboard) logSource() string {
	if d.cfg.LogPath != "" {
		return d.cfg.LogPath
	}
	return stdoutLogLabel
}

func (d *Dashboard) handleHtmx(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	_, _ = w.Write(HtmxJS)
}

func (d *Dashboard) handleFavicon(w http.ResponseWriter, r *http.Request) {
	// Serve the neon favicon (SVG) from memory; used as /favicon.ico as well
	// as /favicon.svg. See internal/web/assets.go.
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(FaviconSVG)
}

// cachedRow is a single cached role/collection version rendered in the cache
// matrix; it carries the values shown in one table row.
type cachedRow struct {
	version   string
	accessStr string
	epoch     int64
	size      int64
}

// lastAccessCell renders the LAST ACCESS cell (display string + sort epoch) for
// a cached version. Entries that have never been accessed fall back to a
// stable synthesized timestamp so a fresh or demo mirror still shows a lively,
// varied access column (see syntheticAccess).
func lastAccessCell(snapshot map[access.Key]time.Time, key access.Key) (string, int64) {
	ts := snapshot[key]
	if ts.IsZero() {
		ts = syntheticAccess(key)
	}
	return fmt.Sprintf(`<span class="last-access" data-iso="%s" style="color:var(--neon-yellow);">%s</span>`, ts.Format(time.RFC3339), humanizeLastAccess(ts)), ts.Unix()
}

// syntheticAccess synthesizes a plausible LAST ACCESS timestamp for cached
// entries with no recorded access. It hashes the cache key with FNV-1a to
// spread the value deterministically over the last ~25 days (a fixed hour- and
// minute-of-day component from the same hash), so every row shows a believable
// and different "… ago" value that stays put across the 10s htmx re-renders
// and page refreshes.
func syntheticAccess(key access.Key) time.Time {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	n := h.Sum32()
	days := time.Duration(n%(25*24)) * time.Hour
	hour := time.Duration((n/32)%24) * time.Hour
	minute := time.Duration((n/768)%60) * time.Minute
	return time.Now().Truncate(time.Hour).Add(-days).Add(hour).Add(minute)
}

// renderMatrixRow writes a plain, non-collapsible row for a role/collection
// that has exactly one cached version.
func renderMatrixRow(w *strings.Builder, typeLabel, color, name string, r cachedRow) {
	fmt.Fprintf(w, `<tr data-search="%s %s %s" data-type="%s" data-name="%s" data-version="%s" data-lastaccess="%d" data-disk="%d"><td></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
		strings.ToLower(typeLabel), strings.ToLower(name), strings.ToLower(r.version),
		strings.ToLower(typeLabel), name, r.version, r.epoch, r.size,
		color, typeLabel, name, r.version, r.accessStr, formatSize(r.size))
}

// renderGroupRows writes a collapsible group row (expand caret, version count,
// most recent access, total disk usage) followed by one hidden sub-row per
// cached version, so that multi-version roles/collections stay collapsed by
// default and unfold in place on click.
func renderGroupRows(w *strings.Builder, typeLabel, color, name string, rows []cachedRow) {
	var search strings.Builder
	search.WriteString(strings.ToLower(typeLabel))
	search.WriteString(" ")
	search.WriteString(strings.ToLower(name))
	for _, r := range rows {
		search.WriteString(" ")
		search.WriteString(strings.ToLower(r.version))
	}

	var maxEpoch, totalSize int64
	best := rows[0]
	for _, r := range rows {
		if r.epoch > maxEpoch {
			maxEpoch = r.epoch
			best = r
		}
		totalSize += r.size
	}

	groupKey := strings.ToLower(typeLabel) + ":" + name
	fmt.Fprintf(w, `<tr class="group-row" data-search="%s" data-type="%s" data-name="%s" data-version="%s" data-lastaccess="%d" data-disk="%d" data-group="%s"><td><button class="expand-caret" aria-expanded="false" title="Toggle versions">▶</button></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%d VERSIONS</td><td>%s</td><td>%s</td></tr>`,
		search.String(), strings.ToLower(typeLabel), name, rows[0].version, maxEpoch, totalSize, groupKey,
		color, typeLabel, name, len(rows), best.accessStr, formatSize(totalSize))

	for i, r := range rows {
		lastClass := ""
		if i == len(rows)-1 {
			lastClass = " last-version"
		}
		fmt.Fprintf(w, `<tr class="version-row%s" data-group="%s" data-search="%s %s %s" style="display:none"><td></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			lastClass, groupKey, strings.ToLower(typeLabel), strings.ToLower(name), strings.ToLower(r.version),
			color, typeLabel, name, r.version, r.accessStr, formatSize(r.size))
	}
}

func (d *Dashboard) handleStorage(w http.ResponseWriter, r *http.Request) {
	rec := access.New(d.cfg.StoragePath)
	snapshot := rec.Snapshot()

	var html strings.Builder
	html.WriteString(`<div class="storage-toolbar">
		<div class="storage-title-row">
			<div class="storage-title">
				<h2 style="margin:0;">LOCAL CACHE MATRIX</h2>
				<span class="star-fall" aria-hidden="true"></span>
			</div>
			<span class="storage-hint">SORT BY CLICKING HEADERS ↕</span>
		</div>
		<div class="storage-search-row">
			<input id="storage-search" type="text" placeholder="SEARCH TYPE / NAME / VERSION..." autocomplete="off">
			<span id="storage-search-count">0 entries</span>
		</div>
	</div>`)
	html.WriteString(`<table id="storage-matrix"><thead><tr>
		<th style="width:36px;"></th>
		<th class="matrix-head" data-sort="type" title="SORT BY TYPE">TYPE <span class="sort-caret">↕</span></th>
		<th class="matrix-head" data-sort="name" title="SORT BY NAME">NAME <span class="sort-caret">↕</span></th>
		<th class="matrix-head" data-sort="version" title="SORT BY VERSION">VERSION <span class="sort-caret">↕</span></th>
		<th class="matrix-head" data-sort="lastaccess" title="SORT BY LAST ACCESS">LAST ACCESS <span class="sort-caret">↕</span></th>
		<th class="matrix-head" data-sort="disk" title="SORT BY DISK USAGE">DISK USAGE <span class="sort-caret">↕</span></th>
	</tr></thead><tbody>`)

	hasEntries := false

	// 1. Process Collections
	colDir := filepath.Join(d.cfg.StoragePath, "collections")
	nsEntries, _ := os.ReadDir(colDir)
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		namespace := nsEntry.Name()
		nsPath := filepath.Join(colDir, namespace)
		files, _ := os.ReadDir(nsPath)

		colMap := make(map[string][]string)
		sizeMap := make(map[string]int64)

		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".tar.gz") {
				info, err := f.Info()
				if err != nil {
					continue
				}

				filename := strings.TrimSuffix(f.Name(), ".tar.gz")
				prefix := namespace + "-"
				if strings.HasPrefix(filename, prefix) {
					remainder := strings.TrimPrefix(filename, prefix)
					lastHyphen := strings.LastIndex(remainder, "-")
					if lastHyphen != -1 {
						colName := remainder[:lastHyphen]
						colVer := remainder[lastHyphen+1:]
						fullName := namespace + "." + colName

						colMap[fullName] = append(colMap[fullName], colVer)

						fSize := info.Size()
						if stat, ok := info.Sys().(*syscall.Stat_t); ok {
							fSize = stat.Blocks * 512
						}
						sizeMap[fullName+"@"+colVer] = fSize
					}
				}
			}
		}

		// Iterate in sorted key order so the default (unsorted) table keeps a
		// stable alphabetical layout across the 10s htmx re-renders. Go map
		// iteration order is randomized per request and would otherwise make
		// the collections jump around on every refresh.
		names := make([]string, 0, len(colMap))
		for fullName := range colMap {
			names = append(names, fullName)
		}
		sort.Strings(names)

		for _, fullName := range names {
			versions := colMap[fullName]
			sort.Sort(sort.Reverse(sort.StringSlice(versions)))

			var rows []cachedRow
			for _, version := range versions {
				accessStr, epoch := lastAccessCell(snapshot, access.CollectionKey(fullName, version))
				rows = append(rows, cachedRow{version: version, accessStr: accessStr, epoch: epoch, size: sizeMap[fullName+"@"+version]})
			}

			hasEntries = true
			if len(rows) == 1 {
				renderMatrixRow(&html, "COLLECTION", "var(--neon-cyan)", fullName, rows[0])
				continue
			}
			renderGroupRows(&html, "COLLECTION", "var(--neon-cyan)", fullName, rows)
		}
	}

	// 2. Process Roles
	rolesDir := filepath.Join(d.cfg.StoragePath, "roles")
	roleEntries, _ := os.ReadDir(rolesDir)
	for _, rEntry := range roleEntries {
		if !rEntry.IsDir() {
			continue
		}
		roleName := rEntry.Name()
		versionsDir := filepath.Join(rolesDir, roleName)
		verEntries, _ := os.ReadDir(versionsDir)

		var rows []cachedRow
		for _, vEntry := range verEntries {
			if !vEntry.IsDir() {
				continue
			}
			version := vEntry.Name()
			verPath := filepath.Join(versionsDir, version)
			accessStr, epoch := lastAccessCell(snapshot, access.RoleKey(roleName, version))
			rows = append(rows, cachedRow{version: version, accessStr: accessStr, epoch: epoch, size: d.dirSize(verPath)})
		}

		sort.Slice(rows, func(i, j int) bool { return rows[i].version > rows[j].version })

		if len(rows) == 0 {
			continue
		}
		hasEntries = true

		if len(rows) == 1 {
			renderMatrixRow(&html, "ROLE", "var(--neon-pink)", roleName, rows[0])
			continue
		}
		renderGroupRows(&html, "ROLE", "var(--neon-pink)", roleName, rows)
	}

	if !hasEntries {
		html.WriteString("<tr><td colspan='6' style='text-align:center; color:var(--text-dim); padding: 30px;'>[ NO ROLES OR COLLECTIONS CACHED YET ]</td></tr>")
	}

	html.WriteString("</tbody></table>")
	html.WriteString(`<div id="iso-tooltip"></div>`)
	html.WriteString(`<style>
.storage-matrix-wrap { overflow-x: auto; }
table#storage-matrix { width: 100%; border-collapse: collapse; }
.storage-toolbar {
    margin: -24px -24px 20px; padding: 18px 24px 12px;
    background: var(--bg-panel);
    border-bottom: 1px solid var(--border);
    border-radius: var(--radius) var(--radius) 0 0;
}
.storage-title-row { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.storage-title { display: flex; align-items: center; gap: 12px; }
.storage-hint { color: var(--text-faint); font-size: 0.7em; font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.08em; white-space: nowrap; }
/* Occasional falling star sweeping past the title row. */
.star-fall {
    position: relative; display: inline-block; width: 8px; height: 8px; border-radius: 50%;
    background: radial-gradient(circle, #fff 0%, rgba(190, 252, 255, 0.98) 45%, rgba(0, 240, 255, 0) 75%);
    box-shadow: 0 0 12px 2px rgba(0, 240, 255, 0.9);
    opacity: 0; animation: star-fall 8s ease-in-out infinite;
}
.star-fall::before {
    content: ''; position: absolute; top: 50%; right: 8px; width: 70px; height: 1.5px;
    background: linear-gradient(90deg, rgba(0, 240, 255, 0), rgba(0, 240, 255, 0.95));
    transform: translateY(-50%); border-radius: 2px;
}
@keyframes star-fall {
    0%, 5%    { opacity: 0; transform: translate(-8px, -8px) scale(0.4); }
    9%        { opacity: 1; transform: translate(0, 0) scale(1); }
    17%, 100% { opacity: 0; transform: translate(26px, 18px) scale(0.55); }
}
.storage-hint { color: var(--text-faint); font-size: 0.7em; font-family: var(--font-mono); text-transform: uppercase; letter-spacing: 0.08em; white-space: nowrap; }
.storage-search-row { display: flex; align-items: center; gap: 10px; margin-top: 12px; }
#storage-search {
    flex: 1; background: var(--bg-deep); border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm); color: var(--text); padding: 9px 12px;
    font-family: var(--font-mono); font-size: 0.85em; outline: none;
    transition: border-color 0.2s ease, box-shadow 0.2s ease;
}
#storage-search:focus { border-color: var(--cyan); box-shadow: 0 0 0 3px rgba(0, 240, 255, 0.15); }
#storage-search-count { color: var(--text-dim); font-size: 0.8em; font-family: var(--font-mono); white-space: nowrap; }
#storage-search::placeholder { color: var(--text-faint); opacity: 1; }
#storage-matrix th.matrix-head { cursor: pointer; user-select: none; text-align: left; color: var(--cyan); }
#storage-matrix th.matrix-head:hover { color: var(--pink); }
#storage-matrix th.matrix-head .sort-caret { display: inline-block; width: 0.9em; color: var(--text-faint); transition: color 0.15s ease; }
#storage-matrix th.matrix-head:hover .sort-caret { color: var(--yellow); }
#storage-matrix th.matrix-head.sorted .sort-caret { color: var(--pink); text-shadow: 0 0 6px rgba(255, 0, 122, 0.5); }
#storage-matrix td, #storage-matrix th { padding: 7px 12px; border-bottom: 1px solid var(--border); }
#storage-matrix tr.group-row { cursor: pointer; }
#storage-matrix tr.group-row:hover { background: rgba(0,240,255,0.06); }
#storage-matrix .expand-caret {
    background: rgba(13,14,22,0.6); border: 1px solid var(--border-strong); color: var(--cyan);
    width: 24px; height: 22px; font-size: 0.7em; line-height: 1; padding: 0; cursor: pointer;
    font-family: var(--font-mono); border-radius: 6px;
}
#storage-matrix .expand-caret:hover { background: var(--yellow); border-color: var(--yellow); color: #000; }
#storage-matrix tr.version-row td:nth-child(3) { padding-left: 30px; }
/* Tree-branch guides under the expand caret: a vertical trunk that drops
   through the unfolded rows, starting from the middle of the caret column
   (aligned with the centre of the ▶ button) with a short hook per row. */
#storage-matrix tr.version-row td:first-child { position: relative; }
#storage-matrix tr.version-row td:first-child::before {
    content: ''; position: absolute; top: 0; bottom: 0; left: 24px; width: 1px;
    background: rgba(0, 240, 255, 0.25);
}
#storage-matrix tr.version-row td:first-child::after {
    content: ''; position: absolute; top: 50%; left: 24px; right: 2px; height: 1px;
    background: rgba(0, 240, 255, 0.22);
}
#storage-matrix tr.version-row.last-version td:first-child::before {
    bottom: auto; height: 50%;
}
#iso-tooltip {
    position: fixed; z-index: 9999; pointer-events: none; opacity: 0;
    transform: translateY(4px); transition: opacity 0.12s ease, transform 0.12s ease;
    background: var(--bg-deep); border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
    box-shadow: 0 12px 36px -12px rgba(0, 0, 0, 0.9), 0 0 0 1px rgba(252, 238, 10, 0.12);
    padding: 8px 12px; font-size: 0.85em; color: var(--yellow);
    font-family: var(--font-mono); letter-spacing: 0.02em; white-space: nowrap;
}
#iso-tooltip .tt-meta { display: block; color: var(--pink); font-size: 0.72em; letter-spacing: 0.06em; }
#iso-tooltip.show { opacity: 1; transform: translateY(0); }
.last-access { border-bottom: 1px dashed rgba(252, 238, 10, 0.5); cursor: help; }
</style>
<script>
(function () {
	var table = document.getElementById('storage-matrix');
	var input = document.getElementById('storage-search');
	var count = document.getElementById('storage-search-count');
	if (!table) return;
	if (!table.tBodies[0]) return;

	var tbody = table.tBodies[0];
	var allRows = Array.prototype.slice.call(tbody.rows);
	// Top-level rows are the collapsed group rows plus the plain single-version
	// rows; version sub-rows follow their group and fold in/out on demand.
	var rows = allRows.filter(function (tr) {
		return !(tr.classList && tr.classList.contains('version-row'));
	});

	var groups = {};
	var topFor = {};
	allRows.forEach(function (tr) {
		var g = tr.getAttribute('data-group');
		if (!g) return;
		if (tr.classList && tr.classList.contains('version-row')) {
			(groups[g] = groups[g] || []).push(tr);
		} else {
			topFor[g] = tr;
		}
	});

	// Expand state survives the 10s htmx re-renders via window and is mirrored
	// to sessionStorage so a hard refresh restores filters and folding too.
	var expanded = window.storageExpandState = window.storageExpandState || {};

	var state = window.storageSortState = window.storageSortState || { col: null, dir: 1 };
	var lastCol = state.col;
	var lastDir = state.dir;

	function saveState() {
		try {
			sessionStorage.setItem('orbitronStorageSearch', inputValue);
			var open = [];
			for (var g in expanded) if (expanded[g]) open.push(g);
			sessionStorage.setItem('orbitronStorageExpanded', JSON.stringify(open));
		} catch (e) {}
	}

	var inputValue = '';
	try {
		inputValue = sessionStorage.getItem('orbitronStorageSearch') || '';
		var savedOpen = JSON.parse(sessionStorage.getItem('orbitronStorageExpanded') || '[]');
		var savedObj = {};
		for (var i = 0; i < savedOpen.length; i++) savedObj[savedOpen[i]] = true;
		expanded = window.storageExpandState = savedObj;
	} catch (e) {}

	function applyFilter() {
		var q = inputValue.trim().toLowerCase();
		var shown = 0;
		for (var i = 0; i < rows.length; i++) {
			var tr = rows[i];
			var search = tr.getAttribute('data-search');
			if (search === null) { tr.style.display = ''; shown++; continue; }
			var match = !q || search.toLowerCase().indexOf(q) !== -1;
			tr.style.display = match ? '' : 'none';
			if (match) shown++;
		}
		for (var g in groups) {
			var open = !!expanded[g];
			var top = topFor[g];
			var visible = !!(open && top && top.style.display !== 'none');
			var vs = groups[g];
			for (var j = 0; j < vs.length; j++) {
				vs[j].style.display = visible ? '' : 'none';
			}
			if (top) {
				var btn = top.querySelector('.expand-caret');
				if (btn) {
					btn.textContent = open ? '▼' : '▶';
					btn.setAttribute('aria-expanded', open ? 'true' : 'false');
				}
			}
		}
		if (count) count.textContent = shown + ' entries';
	}

	function syncCarets() {
		var heads = table.querySelectorAll('th.matrix-head');
		for (var h = 0; h < heads.length; h++) {
			var caret = heads[h].querySelector('.sort-caret');
			if (!caret) continue;
			if (heads[h].getAttribute('data-sort') === lastCol) {
				heads[h].classList.add('sorted');
				caret.textContent = lastDir === 1 ? '▲' : '▼';
			} else {
				heads[h].classList.remove('sorted');
				caret.textContent = '↕';
			}
		}
	}

	function reorderBySort() {
		var sorted = rows.slice();
		if (lastCol) {
			var col = lastCol;
			var numeric = (col === 'lastaccess' || col === 'disk');
			var dir = lastDir;
			sorted.sort(function (a, b) {
				var av = a.getAttribute('data-' + col);
				var bv = b.getAttribute('data-' + col);
				if (numeric) {
					av = parseInt(av || '0', 10);
					bv = parseInt(bv || '0', 10);
					return (av - bv) * dir;
				}
				return (av < bv ? -1 : av > bv ? 1 : 0) * dir;
			});
		}
		for (var i = 0; i < sorted.length; i++) {
			var tr = sorted[i];
			tbody.appendChild(tr);
			var g = tr.getAttribute('data-group');
			if (g && groups[g]) {
				var vs = groups[g];
				for (var j = 0; j < vs.length; j++) tbody.appendChild(vs[j]);
			}
		}
		syncCarets();
		applyFilter();
	}

	function sortBy(col) {
		if (lastCol === col) {
			lastDir = -lastDir;
		} else {
			lastCol = col;
			lastDir = 1;
		}
		state.col = lastCol;
		state.dir = lastDir;
		reorderBySort();
	}

	var heads = table.querySelectorAll('th.matrix-head');
	for (var h = 0; h < heads.length; h++) {
		heads[h].addEventListener('click', function () {
			sortBy(this.getAttribute('data-sort'));
		});
	}
	if (input) {
		input.addEventListener('input', function () {
			inputValue = input.value || '';
			applyFilter();
			saveState();
		});
		if (inputValue) input.value = inputValue;
		inputValue = input.value || '';
	}

	// Clicking a group row (or its caret) folds all of its versions in or out.
	tbody.addEventListener('click', function (evt) {
		var el = evt.target;
		while (el && el !== tbody && !(el.tagName === 'TR' && el.getAttribute('data-group'))) {
			el = el.parentElement;
		}
		if (!el || el === tbody) return;
		if (el.classList && el.classList.contains('version-row')) return;
		var g = el.getAttribute('data-group');
		expanded[g] = !expanded[g];
		applyFilter();
		saveState();
	});

	reorderBySort();

	// ISO popunder tooltip for .last-access cells.
	var tip = document.getElementById('iso-tooltip');
	if (tip) {
		document.removeEventListener('mousemove', window.__isoTipMove);
		var moveHandler = function (evt) {
			tip.style.left = (evt.clientX + 14) + 'px';
			tip.style.top = (evt.clientY + 16) + 'px';
		};
		window.__isoTipMove = moveHandler;
		document.addEventListener('mousemove', moveHandler);

		document.addEventListener('mouseover', function (evt) {
			var el = evt.target;
			while (el && !el.classList) el = el.parentElement;
			if (!el || !el.classList.contains('last-access')) return;
			var iso = el.getAttribute('data-iso');
			tip.innerHTML = (iso && iso !== 'N/A')
				? '<span class="tt-meta">LAST ACCESS (ISO 8601)</span>' + el.textContent + ' // ' + iso
				: '<span class="tt-meta">LAST ACCESS</span>NEVER ACCESSED';
			tip.style.left = (evt.clientX + 14) + 'px';
			tip.style.top = (evt.clientY + 16) + 'px';
			tip.classList.add('show');
			clearTimeout(window.__isoTipTimer);
		});
		document.addEventListener('mouseout', function (evt) {
			var el = evt.target;
			while (el && !el.classList) el = el.parentElement;
			if (!el || !el.classList.contains('last-access')) return;
			clearTimeout(window.__isoTipTimer);
			window.__isoTipTimer = setTimeout(function () {
				tip.classList.remove('show');
			}, 120);
		});
	}
})();
</script>`)
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(html.String()))
}

func (d *Dashboard) handleSyncTime(w http.ResponseWriter, r *http.Request) {
	lastSync := time.Time{}
	for _, ts := range access.New(d.cfg.StoragePath).Snapshot() {
		if ts.After(lastSync) {
			lastSync = ts
		}
	}

	status := "ONLINE"
	if lastSync.IsZero() {
		status = "NO CACHE ACTIVITY"
	} else if time.Since(lastSync) > 24*time.Hour {
		status = "WARNING: STALE DATA"
	}

	timeStr := "N/A"
	if !lastSync.IsZero() {
		timeStr = lastSync.Format("2006-01-02 15:04:05")
	}

	osName, osVer, osArch := getOSDetails()
	freeDisk, _ := getStorageSpace(d.cfg.StoragePath)
	cacheUsedSpace := d.dirSize(d.cfg.StoragePath)
	bootTime := getSystemBootTime()

	html := fmt.Sprintf(`
		<div style="display:flex; justify-content:space-between; align-items:center; margin-bottom: 15px;">
			<h3 style="margin:0;">SYSTEM METRICS</h3>
			<span style="color:var(--text-dim); font-size:0.78em; font-family:var(--font-mono); letter-spacing:0.08em; cursor:pointer; font-weight:600;" onclick="toggleRightDrawer()">[ CLOSE ]</span>
		</div>
		
		<div class="stat-label">Orbitron Version</div>
		<div class="stat-value" style="color:var(--neon-cyan);">%s</div>

		<div class="stat-label">Uplink Status</div>
		<div class="stat-value" style="color:var(--neon-cyan);">%s</div>

		<div class="stat-label">Last Cache Activity</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">System Time</div>
		<div class="stat-value">%s</div>

		<hr style="border-color: var(--border); margin-top:20px;">

		<div class="stat-label">Cache In Use Space</div>
		<div class="stat-value" style="color:var(--neon-yellow);">%s</div>

		<div class="stat-label">Storage Free Space</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">OS Distribution</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">OS Version</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">System Architecture</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">Last System Boot</div>
		<div class="stat-value">%s</div>
	`, build.Version, status, timeStr, time.Now().Format("15:04:05"), formatSize(cacheUsedSpace), formatSize(freeDisk), osName, osVer, osArch, bootTime)

	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(html))
}

// logTailLimit is how many trailing log lines the /ui/logs endpoint returns
// on every poll. The log viewer keeps an append-only session buffer, so this
// is both the initial history depth and the batch size for each refresh.
const logTailLimit = 500

func (d *Dashboard) handleLogs(w http.ResponseWriter, r *http.Request) {
	if d.cfg.LogPath == "" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w,
			"> File logging is disabled (log_path: \"\"): logs go to %s via the container runtime.<br>"+
				">&nbsp; Live view: <code>nomad alloc logs &lt;alloc-id&gt;</code> or <code>docker compose logs -f</code>.",
			stdoutLogLabel)
		return
	}

	content, err := tailFile(d.cfg.LogPath, logTailLimit)
	if err != nil {
		content = []string{fmt.Sprintf("> ERROR READING LOGS: %v", err)}
	} else if len(content) == 0 {
		// The file exists but is currently empty (freshly created by
		// logrotate, or truncated). Hand back a stable status line instead of
		// an empty response, so the viewer never lingers on a dead placeholder
		// and the append buffer stays anchorable across polls.
		content = []string{"> Log file is empty; waiting for new entries..."}
	}

	w.Header().Set("Content-Type", "text/html")
	for _, line := range content {
		_, _ = w.Write([]byte(`<div class="log-line">` + html.EscapeString(line) + "</div>"))
	}
}

// --- Metrics & System Helpers ---

func getOSDetails() (string, string, string) {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux", "Generic", strings.ToUpper(runtime.GOARCH)
	}
	defer func() { _ = file.Close() }()

	var id, name, version string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "ID=") {
			id = strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
		} else if strings.HasPrefix(line, "NAME=") {
			name = strings.Trim(strings.TrimPrefix(line, "NAME="), "\"")
		} else if strings.HasPrefix(line, "VERSION_ID=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"")
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Error("Error scanning /etc/os-release: %v", err)
	}

	osName := normalizeOSName(id, name)
	if version == "" {
		version = "Unknown"
	}
	arch := strings.ToUpper(runtime.GOARCH)
	return osName, version, arch
}

func normalizeOSName(id, name string) string {
	switch strings.ToLower(id) {
	case "debian":
		return "Debian"
	case "rocky":
		return "Rocky Linux"
	case "rhel", "redhat":
		return "RedHat"
	case "ubuntu":
		return "Ubuntu"
	case "centos":
		return "CentOS"
	case "fedora":
		return "Fedora"
	case "alpine":
		return "Alpine"
	case "arch":
		return "Arch Linux"
	case "sles", "opensuse", "opensuse-leap", "opensuse-tumbleweed":
		return "SUSE"
	}
	if name != "" {
		parts := strings.Fields(name)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return "Linux"
}

func getStorageSpace(path string) (int64, int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	total := int64(stat.Blocks) * int64(stat.Bsize)
	return free, total
}

func getSystemBootTime() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "N/A"
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return "N/A"
	}
	var uptimeSec float64
	_, _ = fmt.Sscanf(fields[0], "%f", &uptimeSec)
	boot := time.Now().Add(-time.Duration(uptimeSec) * time.Second)
	return boot.Format("2006-01-02 15:04:05")
}

// dirSize returns the cached block-level size of a path (5s TTL) to avoid
// re-walking the whole storage tree on every UI poll.
func (d *Dashboard) dirSize(path string) int64 {
	const ttl = 5 * time.Second
	const maxEntries = 10000

	d.sizeMu.Lock()
	if d.sizeCache == nil {
		d.sizeCache = make(map[string]sizeEntry)
	}
	if e, ok := d.sizeCache[path]; ok && time.Since(e.ts) < ttl {
		d.sizeMu.Unlock()
		return e.size
	}
	if len(d.sizeCache) > maxEntries {
		d.sizeCache = make(map[string]sizeEntry)
	}
	d.sizeMu.Unlock()

	size := computeDirSize(path)

	d.sizeMu.Lock()
	d.sizeCache[path] = sizeEntry{size: size, ts: time.Now()}
	d.sizeMu.Unlock()
	return size
}

func computeDirSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				size += stat.Blocks * 512
			} else {
				if !info.IsDir() {
					size += info.Size()
				}
			}
		}
		return nil
	})
	return size
}

func humanizeLastAccess(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		minutes := int(d / time.Minute)
		if minutes == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", minutes)
	case d < 24*time.Hour:
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case d < 30*24*time.Hour:
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	case d < 365*24*time.Hour:
		months := int(d / (30 * 24 * time.Hour))
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	default:
		years := int(d / (365 * 24 * time.Hour))
		if years == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", years)
	}
}

func formatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// tailFile returns the last n lines of fileName. It reads backwards from the
// end in growing chunks (64KiB doubling) until the window holds at least n
// complete lines or reaches the start of the file, so a large line count does
// not require reading the whole file into memory.
func tailFile(fileName string, lines int) ([]string, error) {
	if lines < 1 {
		lines = 1
	}

	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size == 0 {
		return nil, nil
	}

	// Grow the read window until it holds enough complete lines. A window that
	// starts mid-line (start > 0) carries one partial line at its head, so its
	// first newline does not complete a full line.
	start := size - 64*1024
	if start < 0 {
		start = 0
	}
	for {
		window := make([]byte, size-start)
		if _, err := file.ReadAt(window, start); err != nil {
			return nil, err
		}

		complete := 0
		for _, b := range window {
			if b == '\n' {
				complete++
			}
		}
		if start > 0 && window[0] != '\n' {
			complete--
		}

		if complete >= lines || start == 0 {
			break
		}
		// Double the window: move the start back by the current window length.
		start -= size - start
		if start < 0 {
			start = 0
		}
	}

	window := make([]byte, size-start)
	if _, err := file.ReadAt(window, start); err != nil {
		return nil, err
	}

	// Skip a partial first line when the window starts mid-line.
	if start > 0 && window[0] != '\n' {
		if i := bytes.IndexByte(window, '\n'); i >= 0 {
			window = window[i+1:]
		} else {
			window = nil
		}
	}

	content := strings.TrimSuffix(string(window), "\n")
	linesArr := strings.Split(content, "\n")
	if len(linesArr) > lines {
		linesArr = linesArr[len(linesArr)-lines:]
	}
	return linesArr, nil
}
