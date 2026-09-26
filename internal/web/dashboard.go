package web

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
        .log-line .log-ts { color: var(--text-faint); }
        .log-line .log-level { font-weight: 700; margin-right: 2px; }
        .log-line .log-level-info { color: var(--cyan); }
        .log-line .log-level-debug { color: var(--text-dim); }
        .log-line .log-level-warn { color: var(--yellow); }
        .log-line .log-level-error { color: var(--red); }
        .log-line .log-level-trace { color: var(--indigo); }

        .sync-indicator {
            font-family: var(--font-mono); font-size: 0.72rem; font-weight: 600;
            letter-spacing: 0.08em; color: var(--text-dim); white-space: nowrap;
            border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
            padding: 8px 12px; background: rgba(13, 14, 22, 0.6);
            display: inline-flex; align-items: center; gap: 8px;
            transition: color 0.2s ease, border-color 0.2s ease, box-shadow 0.2s ease;
        }
        .sync-indicator:not(.busy):not(.failed) { color: var(--text-dim); }
        .sync-indicator.ok { color: var(--cyan); }
        .sync-indicator.busy { color: var(--cyan); border-color: var(--cyan); box-shadow: 0 0 14px rgba(0, 240, 255, 0.18); }
        .sync-indicator.failed { color: var(--red); border-color: var(--red); box-shadow: 0 0 14px rgba(255, 45, 85, 0.18); }
        .sync-indicator::before {
            content: ''; width: 8px; height: 8px; border-radius: 50%;
            border: 2px solid var(--border-strong);
        }
        .sync-indicator.ok::before { background: var(--cyan); border-color: var(--cyan); box-shadow: 0 -1px 6px var(--cyan); }
        .sync-indicator.failed::before { background: var(--red); border-color: var(--red); }
        .sync-indicator.busy::before {
            border-color: rgba(0, 240, 255, 0.25); border-top-color: var(--cyan);
            animation: sync-spin 0.9s linear infinite;
        }
        @keyframes sync-spin { to { transform: rotate(360deg); } }
        .sync-trigger {
            background: rgba(13, 14, 22, 0.6); color: var(--text-dim); border: 1px solid var(--border-strong);
            border-radius: var(--radius-sm); padding: 8px 12px; cursor: pointer; font-family: var(--font-mono);
            font-size: 0.72rem; font-weight: 600; letter-spacing: 0.08em;
            transition: color 0.15s ease, border-color 0.15s ease;
        }
        .sync-trigger:hover { color: var(--cyan); border-color: var(--cyan); }
        .log-download {
            display: inline-flex; align-items: center; gap: 6px;
            background: rgba(13, 14, 22, 0.6); color: var(--cyan); border: 1px solid var(--border-strong);
            border-radius: var(--radius-sm); padding: 6px 10px; cursor: pointer; font-family: var(--font-mono);
            font-size: 0.72rem; font-weight: 600; letter-spacing: 0.08em;
            transition: color 0.15s ease, border-color 0.15s ease, box-shadow 0.15s ease;
        }
        .log-download:hover { color: var(--yellow); border-color: var(--yellow); box-shadow: 0 0 12px -4px rgba(252, 238, 10, 0.6); }
        .log-download svg { flex: none; }
        #log-filter:focus { border-color: var(--cyan); box-shadow: 0 0 0 3px rgba(0, 240, 255, 0.15); }

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
        /* Rare whole-viewport shooting star (interval driven from JS). */
        #star-shower {
            position: fixed; inset: 0; overflow: hidden; pointer-events: none;
            z-index: 9997;
        }
        .falling-star {
            position: absolute; top: -16px; width: 8px; height: 8px; border-radius: 50%;
            background: radial-gradient(circle, #fff 0%, rgba(190, 252, 255, 0.98) 42%, rgba(0, 240, 255, 0) 74%);
            box-shadow: 0 0 12px 3px rgba(0, 240, 255, 0.85), 0 0 26px 6px rgba(120, 220, 255, 0.35);
            animation: falling 1.9s cubic-bezier(0.25, 0.05, 0.55, 1) forwards;
            will-change: transform, opacity;
        }
        .falling-star::before {
            content: ''; position: absolute; left: 50%; transform: translateX(-50%);
            bottom: 100%; width: 3px; height: 190px;
            background: linear-gradient(180deg, rgba(120, 235, 255, 0) 0%, rgba(120, 235, 255, 0.55) 55%, rgba(230, 255, 255, 1) 100%);
            border-radius: 3px;
            clip-path: polygon(38% 0, 62% 0, 100% 100%, 0 100%);
        }
        .falling-star::after {
            content: ''; position: absolute; left: 50%; top: 50%; width: 300px; height: 300px; margin: -150px;
            border-radius: 50%; pointer-events: none;
            background: radial-gradient(circle, rgba(255, 255, 255, 0.95) 0%, rgba(160, 240, 255, 0.4) 28%, rgba(0, 240, 255, 0) 62%);
            opacity: 0; transform: scale(0.1);
            animation: atmos-flash 0.55s ease-out var(--flash-delay, 0s) forwards;
        }
        @keyframes atmos-flash {
            0%   { opacity: 0; transform: scale(0.1); }
            15%  { opacity: 1; }
            100% { opacity: 0; transform: scale(1.7); }
        }
        @keyframes falling {
            0%   { transform: translate3d(0, 0, 0) rotate(var(--angle)) scale(0.3); opacity: 0; }
            6%   { opacity: 1; }
            45%  { transform: translate3d(calc(var(--drift) * 0.45), 42vh, 0) rotate(var(--angle)) scale(1); opacity: 1; }
            100% { transform: translate3d(var(--drift), 106vh, 0) rotate(var(--angle)) scale(1); opacity: 0; }
        }
    </style>
</head>
<body>
    <div class="app-layout">
        <div id="star-shower" aria-hidden="true"></div>
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
                <span id="sync-indicator" class="sync-indicator" title="Background sync state">◍ MIRROR IDLE</span>
                <button id="sync-trigger" class="sync-trigger" type="button" title="Trigger a full cache sync now">⟳ SYNC</button>
                <button class="btn-log-toggle btn-bottom-toggle" onclick="toggleBottomDrawer()">▲ LOG STREAM</button>
                <button class="btn-metrics" onclick="toggleRightDrawer()">◄ SYS METRICS</button>
                <button id="btn-disconnect" class="btn-logout" onclick="armDisconnect()">[ DISCONNECT ]</button>
            </div>
        </div>

        <!-- Fullscreen Local Cache Matrix (auto-refresh via its own checkbox) -->
        <div id="main-workspace" class="main-workspace" hx-get="/ui/storage" hx-trigger="load">
            <h2>[ Scanning Cache Matrix... ]</h2>
        </div>

        <!-- Right System Metrics Drawer -->
        <div id="right-drawer" hx-get="/ui/sync" hx-trigger="load, every 10s">
            <h3>[ Telemetry Stream... ]</h3>
        </div>

        <!-- Bottom Log Stream Drawer -->
        <div id="bottom-drawer">
            <div style="display:flex; justify-content:space-between; align-items:center; gap:12px;">
                <h3 style="margin:0;">SYSTEM LOGS // {{LOG_PATH}}</h3>
                <div style="display:flex; align-items:center; gap:10px;">
                    <input id="log-filter" type="text" placeholder="FILTER LOGS... (LEVEL / TEXT)" autocomplete="off" style="background:var(--bg-deep); border:1px solid var(--border-strong); border-radius:var(--radius-sm); color:var(--text); padding:5px 10px; font-family:var(--font-mono); font-size:0.75em; outline:none; width:240px;">
                    <button id="log-download" class="log-download" onclick="downloadLogs()" title="Download log buffer as .txt" aria-label="Download log buffer as .txt"><svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M4 20h16"/></svg> TXT</button>
                    <span style="color:var(--text-dim); font-size:0.78em; font-family:var(--font-mono); letter-spacing:0.08em; cursor:pointer; font-weight:600;" onclick="toggleBottomDrawer()">[ CLOSE ]</span>
                </div>
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

        // Secret mode: typing "orbitron" (in quick succession, outside form
        // fields) fires the easter egg. Inputs never trigger it, so typing in
        // the log filter stays a normal filter.
        (function () {
            var seq = 'orbitron';
            var pos = 0;
            var lastTs = 0;
            document.addEventListener('keydown', function (e) {
                if (e.target && (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA')) return;
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

        // Typing "starz" fires a shooting star on demand (same rule as the
        // orbitron egg: never while typing inside an INPUT or TEXTAREA).
        (function () {
            var seq = 'starz';
            var pos = 0;
            var lastTs = 0;
            document.addEventListener('keydown', function (e) {
                if (e.target && (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA')) return;
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
                    if (window.__orbitronShootStar) window.__orbitronShootStar();
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

            const renderLines = function (nodes) {
                for (let i = 0; i < nodes.length; i++) {
                    container.appendChild(nodes[i]);
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
                const nodeList = Array.prototype.slice.call(nodes);
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
                    renderLines(nodeList);
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
                        renderLines(nodeList);
                    } else {
                        const fresh = texts.slice(idx + 1);
                        if (fresh.length) {
                            buf = buf.concat(fresh);
                            renderLines(nodeList.slice(idx + 1));
                        }
                    }
                }
                if (buf.length > MAX_BUF) buf = buf.slice(buf.length - MAX_BUF);
                window.__logLines = buf;
                rePin();
                // Freshly appended lines must honour the active filter too,
                // otherwise polling would leak unhidden lines back in.
                if (window.__applyLogFilter) window.__applyLogFilter();
            };

            if (typeof MutationObserver !== 'undefined') {
                new MutationObserver(rePin).observe(container, { childList: true, characterData: true, subtree: true });
            }
        })();

        // Live background-sync indicator in the header, polled from the API.
        // The busy look is held briefly after a job so even a fast background
        // sync stays perceivable in the header. The "SYNC" button next to it
        // kicks off a full resync on demand.
        (function () {
            const el = document.getElementById('sync-indicator');
            const btn = document.getElementById('sync-trigger');
            if (!el) return;
            const HOLD_MS = 9000;
            let busyUntil = 0;
            function render(cur, last) {
                if (cur) {
                    busyUntil = Date.now() + HOLD_MS;
                }
                if (cur || Date.now() < busyUntil) {
                    el.classList.add('busy');
                    el.classList.remove('ok', 'failed');
                    const kind = cur.kind !== 'full' ? cur.kind.toUpperCase() : 'SYNCING';
                    el.textContent = '⟳ ' + kind + ' ' + (cur ? cur.done + '/' + cur.total : '…');
                } else {
                    el.classList.remove('busy');
                    const failed = last && last.status === 'failed';
                    el.classList.toggle('failed', !!failed);
                    el.classList.toggle('ok', !failed);
                    el.textContent = failed ? '◉ LAST SYNC FAILED' : '◍ MIRROR IDLE';
                }
            }
            function tick() {
                fetch('/ui/sync-status', { headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
                    .then(function (res) { return res.ok ? res.json() : null; })
                    .then(function (data) {
                        if (!data) return;
                        const hist = data.history || [];
                        render(data.current, hist.length ? hist[hist.length - 1] : null);
                    })
                    .catch(function () {});
            }
            if (btn) {
                btn.addEventListener('click', function () {
                    busyUntil = Date.now() + HOLD_MS;
                    el.classList.add('busy');
                    el.classList.remove('ok', 'failed');
                    el.textContent = '⟳ SYNCING …';
                    fetch('/ui/sync', { method: 'POST', credentials: 'same-origin' }).catch(function () {});
                });
            }
            tick();
            setInterval(tick, 10000);
        })();

        // Log filter + download utilities for the SYSTEM LOGS drawer. The filter is
        // exposed on window so freshly appended log batches re-apply it.
        (function () {
            const filter = document.getElementById('log-filter');
            const container = document.getElementById('log-container');
            if (!filter || !container) return;
            const applyLogFilter = function () {
                const q = filter.value.trim().toLowerCase();
                const divs = container.querySelectorAll('.log-line');
                for (let i = 0; i < divs.length; i++) {
                    const d = divs[i];
                    d.style.display = (!q || (d.textContent || '').toLowerCase().indexOf(q) !== -1) ? '' : 'none';
                }
            };
            filter.addEventListener('input', applyLogFilter);
            window.__applyLogFilter = applyLogFilter;
            window.downloadLogs = function () {
                const lines = window.__logLines || [];
                const blob = new Blob([lines.join('\n') + '\n'], { type: 'text/plain' });
                const a = document.createElement('a');
                a.href = URL.createObjectURL(blob);
                a.download = 'orbitron-logs.txt';
                document.body.appendChild(a);
                a.click();
                document.body.removeChild(a);
                URL.revokeObjectURL(a.href);
            };
        })();

        // Rare whole-viewport shooting star. It spawns high above the page and
        // falls diagonally across the entire window (over the table too) once
        // every 10 minutes by default. window.__orbitronShootStar() fires one
        // on demand while testing.
        (function () {
            const shower = document.getElementById('star-shower');
            if (!shower) return;
            const STAR_INTERVAL_MS = 10 * 60 * 1000;
            function shoot() {
                const s = document.createElement('div');
                s.className = 'falling-star';
                const dir = (Math.random() < 0.5) ? -1 : 1;
                const driftVw = dir * (16 + Math.random() * 26);
                const lean = (-dir * 22) + (Math.random() * 10 - 5);
                s.style.left = (20 + Math.random() * 56) + 'vw';
                s.style.setProperty('--drift', driftVw + 'vw');
                s.style.setProperty('--angle', lean + 'deg');
                const dur = 1.5 + Math.random() * 0.8;
                s.style.animationDuration = dur + 's';
                s.style.animationDelay = (Math.random() * 0.25) + 's';
                s.style.setProperty('--flash-delay', (dur * 0.45).toFixed(2) + 's');
                shower.appendChild(s);
                setTimeout(function () {
                    if (s.parentNode) s.parentNode.removeChild(s);
                }, (dur + 0.6) * 1000);
            }
            window.__orbitronShootStar = shoot;
            setInterval(shoot, STAR_INTERVAL_MS);
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
// a cached version. Versions with no recorded access (freshly cached but never
// served to a client yet) show an em-dash instead of a made-up timestamp: the
// mirror only knows when content was actually pulled, so it never invents one.
func lastAccessCell(snapshot map[access.Key]time.Time, key access.Key) (string, int64) {
	ts, ok := snapshot[key]
	if !ok || ts.IsZero() {
		return `<span class="last-access" data-iso="N/A" style="color:var(--text-faint);">—</span>`, 0
	}
	cls := "last-access"
	if time.Since(ts) < time.Hour {
		cls += " fresh"
	}
	return fmt.Sprintf(`<span class="%s" data-iso="%s" style="color:var(--neon-yellow);">%s</span>`, cls, ts.Format(time.RFC3339), humanizeLastAccess(ts)), ts.Unix()
}

// renderMatrixRow writes a plain, non-collapsible row for a role/collection
// that has exactly one cached version.
func renderMatrixRow(w *strings.Builder, typeLabel, color, name string, r cachedRow) {
	fmt.Fprintf(w, `<tr data-search="%s %s %s" data-type="%s" data-name="%s" data-version="%s" data-lastaccess="%d" data-disk="%d"><td></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
		strings.ToLower(typeLabel), strings.ToLower(name), strings.ToLower(r.version),
		strings.ToLower(typeLabel), name, r.version, r.epoch, r.size,
		color, typeLabel, name, r.version, r.accessStr, diskMarkup(r.size))
}

// diskMarkup renders the DISK USAGE cell: a mini throughput bar that is scaled
// client-side to the largest item currently visible, plus the human-readable
// size. The raw byte count rides along as a tooltip.
func diskMarkup(size int64) string {
	return `<span class="disk-cell"><span class="disk-bar"><span class="disk-fill"></span></span><span class="disk-size" title="` + strconv.FormatInt(size, 10) + ` bytes">` + formatSize(size) + `</span></span>`
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
		color, typeLabel, name, len(rows), best.accessStr, diskMarkup(totalSize))

	for i, r := range rows {
		rowClass := ""
		if i == len(rows)-1 {
			rowClass = " last-version"
		}
		versionCell := r.version
		if i == 0 {
			versionCell += `<span class="latest-pill">LATEST</span>`
		}
		fmt.Fprintf(w, `<tr class="version-row%s" data-group="%s" data-search="%s %s %s" data-disk="%d" style="display:none"><td></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			rowClass, groupKey, strings.ToLower(typeLabel), strings.ToLower(name), strings.ToLower(r.version), r.size,
			color, typeLabel, name, versionCell, r.accessStr, diskMarkup(r.size))
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
			</div>
			<div class="storage-actions">
				<label class="auto-refresh-toggle" title="Auto-reload the table data every 10s"><input type="checkbox" id="auto-refresh"> AUTO REFRESH</label>
				<button id="density-toggle" type="button" class="density-toggle" title="Toggle row density">DENSITY: COMFORTABLE</button>
				<span class="storage-hint">SORT BY CLICKING HEADERS ↕</span>
			</div>
		</div>
		<div class="storage-search-row">
			<div class="type-chips" role="group" aria-label="Filter by type">
				<button type="button" class="type-chip" data-type="all">ALL</button>
				<button type="button" class="type-chip" data-type="role">ROLES</button>
				<button type="button" class="type-chip" data-type="collection">COLLECTIONS</button>
			</div>
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
#storage-matrix tr.group-row { cursor: default; }
#storage-matrix tr.group-row:hover { background: rgba(0,240,255,0.06); }
#storage-matrix .expand-caret {
    background: rgba(13,14,22,0.6); border: 1px solid var(--border-strong); color: var(--cyan);
    width: 24px; height: 22px; font-size: 0.7em; line-height: 1; padding: 3px 0 5px 0; cursor: pointer;
    font-family: var(--font-mono); border-radius: 6px;
    display: flex; align-items: center; justify-content: center;
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
.storage-actions { display: flex; align-items: center; gap: 12px; }
.density-toggle {
    background: rgba(13,14,22,0.6); border: 1px solid var(--border-strong); color: var(--text-dim);
    border-radius: var(--radius-sm); padding: 4px 10px; font-family: var(--font-mono);
    font-size: 0.7em; letter-spacing: 0.08em; cursor: pointer; white-space: nowrap;
    transition: color 0.15s ease, border-color 0.15s ease;
}
.density-toggle:hover { color: var(--yellow); border-color: var(--yellow); }
.density-toggle.compact { color: var(--cyan); border-color: var(--cyan); }
.auto-refresh-toggle {
    display: inline-flex; align-items: center; gap: 7px; color: var(--text-dim);
    font-family: var(--font-mono); font-size: 0.68em; letter-spacing: 0.1em; cursor: pointer;
    white-space: nowrap; user-select: none;
}
.auto-refresh-toggle input {
    appearance: none; width: 13px; height: 13px; border: 1px solid var(--border-strong);
    border-radius: 4px; background: rgba(13,14,22,0.6); margin: 0; cursor: pointer;
    transition: border-color 0.15s ease, box-shadow 0.15s ease, background 0.15s ease;
    position: relative;
}
.auto-refresh-toggle input:checked {
    border-color: var(--cyan); box-shadow: 0 0 0 3px rgba(0, 240, 255, 0.14); background: rgba(0,240,255,0.15);
}
.auto-refresh-toggle input:checked::after {
    content: ''; position: absolute; left: 3px; top: 1px; width: 4px; height: 7px;
    border: solid var(--cyan); border-width: 0 2px 2px 0; transform: rotate(45deg);
}
.auto-refresh-toggle:hover { color: var(--cyan); }
.type-chips { display: flex; gap: 6px; flex-shrink: 0; }
.type-chip {
    background: rgba(13,14,22,0.6); border: 1px solid var(--border-strong); color: var(--text-dim);
    border-radius: var(--radius-sm); padding: 5px 12px; font-family: var(--font-mono);
    font-size: 0.68em; letter-spacing: 0.1em; cursor: pointer; white-space: nowrap;
    transition: color 0.15s ease, border-color 0.15s ease, box-shadow 0.15s ease;
}
.type-chip:hover { color: var(--yellow); border-color: var(--yellow); }
.type-chip.active { color: var(--cyan); border-color: var(--cyan); box-shadow: 0 0 0 3px rgba(0, 240, 255, 0.12); }
#storage-matrix.compact td, #storage-matrix.compact th { padding: 3px 10px; font-size: 0.86em; }
.latest-pill {
    display: inline-block; margin-left: 8px; padding: 1px 6px; border-radius: 999px;
    border: 1px solid rgba(0, 240, 255, 0.45); color: var(--cyan);
    font-size: 0.62em; letter-spacing: 0.14em; vertical-align: middle; white-space: nowrap;
    box-shadow: 0 0 8px rgba(0, 240, 255, 0.25);
}
/* Mini disk-usage bar, scaled client-side to the largest visible item. */
#storage-matrix .disk-cell { display: inline-flex; align-items: center; gap: 8px; }
#storage-matrix .disk-bar {
    width: 64px; height: 6px; background: rgba(13, 14, 22, 0.6);
    border: 1px solid var(--border-strong); border-radius: 3px; overflow: hidden; flex-shrink: 0;
}
#storage-matrix .disk-fill {
    display: block; height: 100%; width: 0%;
    background: linear-gradient(90deg, rgba(0, 240, 255, 0.45), rgba(0, 240, 255, 0.95));
    transition: width 0.25s ease;
}
#storage-matrix .disk-size { color: var(--text-dim); font-family: var(--font-mono); font-size: 0.78em; white-space: nowrap; }
#storage-matrix tr.group-row .disk-fill { background: linear-gradient(90deg, rgba(255, 0, 122, 0.45), rgba(255, 0, 122, 0.95)); }
/* Pulsing dot on very recently accessed items. */
.last-access.fresh::before {
    content: ''; display: inline-block; width: 7px; height: 7px; border-radius: 50%;
    background: var(--cyan); margin-right: 6px; vertical-align: middle;
    box-shadow: 0 0 8px rgba(0, 240, 255, 0.9);
    animation: pulse-dot 1.6s ease-in-out infinite;
}
@keyframes pulse-dot {
    0%, 100% { opacity: 1; transform: scale(1); }
    50% { opacity: 0.4; transform: scale(0.7); }
}
#storage-matrix tr.sel { background: rgba(0, 240, 255, 0.08); outline: 1px dashed rgba(0, 240, 255, 0.35); outline-offset: -1px; }
tr.storage-empty td {
    text-align: center; color: var(--text-dim); padding: 34px 20px;
    font-family: var(--font-mono); letter-spacing: 0.12em; font-size: 0.85em;
}
tr.storage-empty td .empty-stars { color: var(--yellow); letter-spacing: 0.4em; display: block; margin-bottom: 10px; font-size: 1.1em; }
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

	var typeFilter = 'all';
	try {
		typeFilter = sessionStorage.getItem('orbitronStorageType') || 'all';
	} catch (e) {}
	var compact = false;
	try {
		compact = sessionStorage.getItem('orbitronStorageDensity') === 'compact';
	} catch (e) {}

	function saveState() {
		try {
			sessionStorage.setItem('orbitronStorageSearch', inputValue);
			sessionStorage.setItem('orbitronStorageType', typeFilter);
			sessionStorage.setItem('orbitronStorageDensity', compact ? 'compact' : '');
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
			var match = (!q || search.toLowerCase().indexOf(q) !== -1) &&
				(typeFilter === 'all' || tr.getAttribute('data-type') === typeFilter);
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
		sizeBars();
		if (emptyRow) emptyRow.style.display = shown === 0 ? '' : 'none';
	}

	// Scale every visible DISK USAGE bar to the largest item currently shown,
	// so bars stay meaningful as search/type filters shrink the view.
	function sizeBars() {
		var max = 0;
		var i, tr;
		for (i = 0; i < allRows.length; i++) {
			tr = allRows[i];
			if (tr.style.display === 'none') continue;
			var d = parseInt(tr.getAttribute('data-disk') || '0', 10);
			if (d > max) max = d;
		}
		for (i = 0; i < allRows.length; i++) {
			tr = allRows[i];
			if (tr.style.display === 'none') continue;
			var fill = tr.querySelector('.disk-fill');
			if (!fill) continue;
			var sz = parseInt(tr.getAttribute('data-disk') || '0', 10);
			var pct = max > 0 ? (sz * 100 / max) : 0;
			if (pct > 100) pct = 100;
			fill.style.width = pct + '%';
		}
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

	// Clicking the expand caret folds a group row in or out. Clicking anywhere
	// else on the row does nothing, so dense rows stay predictable.
	tbody.addEventListener('click', function (evt) {
		var caret = evt.target.closest ? evt.target.closest('.expand-caret') : null;
		if (!caret) return;
		var el = caret.closest ? caret.closest('tr[data-group]') : null;
		if (!el || el.classList.contains('version-row')) return;
		var g = el.getAttribute('data-group');
		expanded[g] = !expanded[g];
		applyFilter();
		saveState();
	});

	// Type filter chips (ALL / ROLES / COLLECTIONS). The active chip is rebuilt
	// from session state on every re-render so auto-refresh never falls back to
	// the server-rendered ALL chip.
	var chipEls = document.querySelectorAll('.type-chip');
	for (var c = 0; c < chipEls.length; c++) chipEls[c].classList.remove('active');
	for (var c = 0; c < chipEls.length; c++) {
		chipEls[c].addEventListener('click', function () {
			typeFilter = this.getAttribute('data-type');
			for (var k = 0; k < chipEls.length; k++) chipEls[k].classList.toggle('active', chipEls[k] === this);
			applyFilter();
			saveState();
		});
		if (chipEls[c].getAttribute('data-type') === typeFilter) chipEls[c].classList.add('active');
	}

	// Density toggle (comfortable / compact).
	var densityBtn = document.getElementById('density-toggle');
	if (densityBtn) {
		function applyDensity() {
			table.classList.toggle('compact', compact);
			densityBtn.classList.toggle('compact', compact);
			densityBtn.textContent = 'DENSITY: ' + (compact ? 'COMPACT' : 'COMFORTABLE');
		}
		applyDensity();
		densityBtn.addEventListener('click', function () {
			compact = !compact;
			applyDensity();
			saveState();
		});
	}

	// Auto-refresh: a checkbox (default off, persisted in localStorage) that
	// polls only the table data. The interval is re-armed on every htmx
	// re-render; density, chips, search and folding survive intact, and the
	// metrics drawer + log stream keep their own independent cadence.
	var autoRefreshEl = document.getElementById('auto-refresh');
	var autoOn = false;
	try {
		autoOn = localStorage.getItem('orbitronAutoRefresh') === '1';
	} catch (e) {}
	function scheduleAutoRefresh() {
		if (window.__autoRefreshTimer) {
			clearInterval(window.__autoRefreshTimer);
			window.__autoRefreshTimer = null;
		}
		if (!autoOn) {
			try { localStorage.setItem('orbitronAutoRefresh', '0'); } catch (e) {}
			return;
		}
		window.__autoRefreshTimer = setInterval(function () {
			if (window.htmx && window.htmx.ajax) {
				window.htmx.ajax('GET', '/ui/storage', { target: '#main-workspace', swap: 'innerHTML' });
			}
		}, 10000);
	}
	if (autoRefreshEl) {
		autoRefreshEl.checked = autoOn;
		autoRefreshEl.addEventListener('change', function () {
			autoOn = autoRefreshEl.checked;
			try { localStorage.setItem('orbitronAutoRefresh', autoOn ? '1' : '0'); } catch (e) {}
			scheduleAutoRefresh();
		});
	}
	scheduleAutoRefresh();

	// Empty-state row shown when search or type filters match nothing.
	var emptyRow = document.createElement('tr');
	emptyRow.className = 'storage-empty';
	emptyRow.style.display = 'none';
	emptyRow.innerHTML = '<td colspan="6"><span class="empty-stars">✦ ✧ ✦</span>NO STARS IN THIS QUADRANT — BROADEN THE SEARCH</td>';
	tbody.appendChild(emptyRow);

	// Selection + keyboard shortcuts: "/" focuses search, arrows move a
	// highlighted row, Enter/→/← expand or collapse the selected group.
	var selIndex = -1;
	function selRows() {
		var out = [];
		for (var i = 0; i < rows.length; i++) {
			if (rows[i].style.display !== 'none') out.push(rows[i]);
		}
		return out;
	}
	function setSel(delta) {
		var vis = selRows();
		if (!vis.length) { selIndex = -1; return; }
		if (selIndex < 0) {
			selIndex = delta > 0 ? 0 : vis.length - 1;
		} else {
			selIndex = (selIndex + delta + vis.length) % vis.length;
		}
		for (var i = 0; i < vis.length; i++) vis[i].classList.toggle('sel', i === selIndex);
		if (vis[selIndex] && vis[selIndex].scrollIntoView) vis[selIndex].scrollIntoView({ block: 'nearest' });
	}
	document.addEventListener('keydown', function (e) {
		if (e.ctrlKey || e.metaKey || e.altKey) return;
		var tag = (e.target && e.target.tagName) || '';
		if (e.key === '/' && tag !== 'INPUT' && tag !== 'TEXTAREA') {
			e.preventDefault();
			if (input) input.focus();
			return;
		}
		if (tag === 'INPUT' || tag === 'TEXTAREA') return;
		if (e.key === 'ArrowDown') { e.preventDefault(); setSel(1); }
		else if (e.key === 'ArrowUp') { e.preventDefault(); setSel(-1); }
		else if (e.key === 'Enter' || e.key === 'ArrowRight' || e.key === 'ArrowLeft' || e.key === ' ') {
			var vis = selRows();
			if (!vis.length) return;
			if (selIndex < 0) { setSel(1); return; }
			var el = vis[selIndex];
			if (el && el.getAttribute('data-group')) {
				e.preventDefault();
				if (e.key === 'ArrowRight') expanded[el.getAttribute('data-group')] = true;
				else if (e.key === 'ArrowLeft') expanded[el.getAttribute('data-group')] = false;
				else expanded[el.getAttribute('data-group')] = !expanded[el.getAttribute('data-group')];
				applyFilter();
				saveState();
			}
		}
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
	snapshot := access.New(d.cfg.StoragePath).Snapshot()
	lastSync := time.Time{}
	for _, ts := range snapshot {
		if ts.After(lastSync) {
			lastSync = ts
		}
	}

	// Access history = recorded touches only. Versions that were cached but
	// never served are not activity, so the sparkline and the LAST ACCESS
	// column report real pulls and nothing else.
	var accessTs []time.Time
	for _, ts := range snapshot {
		accessTs = append(accessTs, ts)
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
	activity := accessActivitySparkline(accessTs, 14)

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

		<hr style="border-color: var(--border); margin-top:20px;">

		<div class="stat-label">Cache Access Activity (14 days)</div>
		<div class="stat-value">%s</div>
	`, build.Version, status, timeStr, time.Now().Format("15:04:05"), formatSize(cacheUsedSpace), formatSize(freeDisk), osName, osVer, osArch, bootTime, activity)

	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(html))
}

// accessActivitySparkline renders a small inline SVG histogram of cache
// accesses bucketed per day over the last `days` days (oldest on the left):
// a gradient-filled area under a polyline, a "now" marker on the newest day
// and a baseline so sparse mirrors stay legible. Days with zero pulls stay at
// the baseline; a mirror that never served anything renders a NO ACTIVITY plate
// instead of pretending there was traffic.
func accessActivitySparkline(timestamps []time.Time, days int) string {
	if days < 2 {
		days = 14
	}
	buckets := make([]int, days)
	now := time.Now().Truncate(24 * time.Hour)
	for _, ts := range timestamps {
		d := int(now.Sub(ts) / (24 * time.Hour))
		if d < 0 {
			continue
		}
		if d >= days {
			continue
		}
		buckets[days-1-d]++
	}

	const w, h = 280, 44
	pad := 4
	max := 0
	for _, n := range buckets {
		if n > max {
			max = n
		}
	}

	var b strings.Builder
	if max == 0 {
		b.WriteString(`<svg width="280" height="44" viewBox="0 0 280 44" aria-label="No cache activity"><line x1="4" y1="40" x2="276" y2="40" stroke="rgba(0,240,255,0.35)" stroke-width="1"/><text x="140" y="24" text-anchor="middle" fill="var(--text-faint)" font-family="var(--font-mono)" font-size="10">NO ACTIVITY YET</text></svg>`)
		return b.String()
	}

	step := (w - 2*pad) / maxInt(days-1, 1)
	var xs, ys []int
	for i := 0; i < days; i++ {
		d := days - 1 - i
		xs = append(xs, pad+i*step)
		ys = append(ys, h-pad-(buckets[d]*(h-2*pad))/max)
	}

	fmt.Fprintf(&b, `<svg width="280" height="44" viewBox="0 0 280 44" role="img" aria-label="Cache access activity over the last %d days" style="display:block; max-width:100%%;">`, days)
	// Gradient fill under the line (solid cyan fading to transparent).
	b.WriteString(`<defs><linearGradient id="spark-fill" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="rgba(0,240,255,0.4)"/><stop offset="100%" stop-color="rgba(0,240,255,0)"/></linearGradient></defs>`)
	b.WriteString(`<path d="M`)
	fmt.Fprintf(&b, "%d %d", xs[0], ys[0])
	for i := 1; i < days; i++ {
		fmt.Fprintf(&b, " L%d %d", xs[i], ys[i])
	}
	fmt.Fprintf(&b, " L%d %d L%d %d Z", xs[len(xs)-1], h-pad, xs[0], h-pad)
	b.WriteString(`" fill="url(#spark-fill)"/>`)
	// The activity line itself.
	b.WriteString(`<path d="`)
	fmt.Fprintf(&b, "M%d %d", xs[0], ys[0])
	for i := 1; i < days; i++ {
		fmt.Fprintf(&b, " L%d %d", xs[i], ys[i])
	}
	b.WriteString(`" fill="none" stroke="var(--cyan)" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>`)
	// Per-day dots on days that actually saw a pull.
	for i := 0; i < days; i++ {
		if buckets[days-1-i] > 0 {
			fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="2" fill="var(--cyan)"/>`, xs[i], ys[i])
		}
	}
	// Baseline + axis caption.
	b.WriteString(`<line x1="4" y1="40" x2="276" y2="40" stroke="rgba(0,240,255,0.22)" stroke-width="1"/>`)
	b.WriteString(`<text x="276" y="34" text-anchor="end" fill="var(--text-faint)" font-family="var(--font-mono)" font-size="9">max `)
	fmt.Fprintf(&b, "%d", max)
	b.WriteString(`/day</text>`)
	b.WriteString(`</svg>`)
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
		_, _ = w.Write([]byte(`<div class="log-line" data-logline="` + html.EscapeString(strings.ToLower(line)) + `">` + renderLogLine(line) + "</div>"))
	}
}

// logLineRe matches the daemon logger prefix "[LEVEL] <ISO short ts> "
// followed by the message, e.g. "[INFO] 2026-09-25T05:09:41 message".
var logLineRe = regexp.MustCompile(`^\[(INFO|WARN|ERROR|DEBUG|TRACE)\] (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}) (.*)$`)

// renderLogLine splits a raw daemon log line into level, timestamp and message
// spans so the viewer can colour levels and highlight the timestamp column.
func renderLogLine(line string) string {
	if m := logLineRe.FindStringSubmatch(line); m != nil {
		level, ts, msg := m[1], m[2], m[3]
		return `<span class="log-level log-level-` + strings.ToLower(level) + `">[` + html.EscapeString(level) + `]</span>` +
			` <span class="log-ts">` + html.EscapeString(ts) + `</span> ` +
			`<span class="log-msg">` + html.EscapeString(msg) + `</span>`
	}
	return `<span class="log-msg">` + html.EscapeString(line) + `</span>`
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
