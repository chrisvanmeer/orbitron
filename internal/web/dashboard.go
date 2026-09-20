package web

import (
	"bufio"
	"fmt"
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
	"orbitron/internal/logger"
)

type sizeEntry struct {
	size int64
	ts   time.Time
}

type Dashboard struct {
	cfg       *config.Config
	sizeMu    sync.Mutex
	sizeCache map[string]sizeEntry
}

func NewDashboard(cfg *config.Config) *Dashboard {
	return &Dashboard{cfg: cfg, sizeCache: make(map[string]sizeEntry)}
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

	// Protected UI routes
	mux.HandleFunc("/ui", d.requireAuth(d.handleIndex))
	mux.HandleFunc("/ui/storage", d.requireAuth(d.handleStorage))
	mux.HandleFunc("/ui/logs", d.requireAuth(d.handleLogs))
	mux.HandleFunc("/ui/sync", d.requireAuth(d.handleSyncTime))
}

// --- Middleware & Auth Logic ---

func (d *Dashboard) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if cookie, err := r.Cookie("orbitron_token"); err == nil {
			token = cookie.Value
		}

		valid := false
		if token != "" {
			if store, err := auth.LoadTokens(d.cfg.TokensFile); err == nil {
				if store.Valid(token) {
					valid = true
				}
			}
		}

		if !valid {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/ui")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			d.renderLogin(w, false)
			return
		}

		next(w, r)
	}
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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

	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}

	if valid {
		logger.Info("Web UI session established (from %s)", clientIP)
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

	logger.Warn("Web UI authentication failed (from %s)", clientIP)
	d.renderLogin(w, true)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	clientIP := r.Header.Get("X-Forwarded-For")
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}
	logger.Info("Web UI session terminated (from %s)", clientIP)

	http.SetCookie(w, &http.Cookie{
		Name:     "orbitron_token",
		Value:    "",
		Path:     "/ui",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui", http.StatusSeeOther)
}

func (d *Dashboard) renderLogin(w http.ResponseWriter, hasError bool) {
	display := "none"
	if hasError {
		display = "block"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.Replace(loginTemplate, "{{DISPLAY}}", display, 1)
	_, _ = w.Write([]byte(html))
}

// --- HTML Templates ---

const loginTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>ORBITRON // AUTHENTICATION</title>
    <link rel="icon" type="image/svg+xml" href="/favicon.svg">
    <style>
        :root {
            --bg-color: #050505;
            --panel-bg: #0a0a10;
            --neon-cyan: #00f0ff;
            --neon-pink: #ff003c;
            --neon-yellow: #fcee0a;
        }
        body {
            background-color: var(--bg-color); color: var(--neon-cyan);
            font-family: 'Courier New', Courier, monospace; margin: 0;
            display: flex; align-items: center; justify-content: center; height: 100vh;
            background-image: linear-gradient(rgba(0, 240, 255, 0.03) 1px, transparent 1px),
            linear-gradient(90deg, rgba(0, 240, 255, 0.03) 1px, transparent 1px);
            background-size: 20px 20px;
        }
        .login-box {
            background: var(--panel-bg); border: 1px solid var(--neon-cyan);
            box-shadow: inset 0 0 10px rgba(0, 240, 255, 0.1), 0 0 20px rgba(0, 240, 255, 0.1);
            padding: 40px; text-align: center; width: 350px; position: relative;
        }
        .login-box::before {
            content: ''; position: absolute; top: -2px; left: -2px;
            width: 15px; height: 15px; border-top: 2px solid var(--neon-yellow); border-left: 2px solid var(--neon-yellow);
        }
        .login-box::after {
            content: ''; position: absolute; bottom: -2px; right: -2px;
            width: 15px; height: 15px; border-bottom: 2px solid var(--neon-yellow); border-right: 2px solid var(--neon-yellow);
        }
        .login-logo { width: 92px; height: auto; margin: 0 auto 18px; display: block; filter: drop-shadow(0 0 6px rgba(0, 240, 255, 0.35)); }
        h1 { color: var(--neon-pink); margin-top: 0; letter-spacing: 2px; text-shadow: 0 0 5px var(--neon-pink); }
        input[type="password"] {
            width: 85%; padding: 12px; margin: 25px 0; background: #000;
            border: 1px solid #4a5c66; color: var(--neon-cyan); font-family: inherit;
            outline: none; text-align: center; font-size: 1.1em; letter-spacing: 2px;
        }
        input[type="password"]:focus { border-color: var(--neon-cyan); box-shadow: 0 0 8px rgba(0, 240, 255, 0.4); }
        button {
            background: transparent; color: var(--neon-yellow); border: 1px solid var(--neon-yellow);
            padding: 12px 25px; cursor: pointer; font-family: inherit; text-transform: uppercase;
            transition: 0.3s; font-weight: bold; letter-spacing: 1px;
        }
        button:hover { background: var(--neon-yellow); color: #000; box-shadow: 0 0 10px var(--neon-yellow); }
        .error-msg { color: var(--neon-pink); font-size: 0.9em; margin-bottom: 10px; display: {{DISPLAY}}; font-weight: bold; }
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
        <div class="error-msg">[ ACCESS DENIED: INVALID TOKEN ]</div>
        <form method="POST" action="/ui/login">
            <input type="password" name="token" placeholder="ENTER ACCESS TOKEN" required autofocus>
            <br>
            <button type="submit">Establish Link</button>
        </form>
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
        :root {
            --bg-color: #050505;
            --panel-bg: #0a0a10;
            --neon-cyan: #00f0ff;
            --neon-pink: #ff003c;
            --neon-yellow: #fcee0a;
            --text-dim: #4a5c66;
        }
        * { box-sizing: border-box; }
        body {
            background-color: var(--bg-color); color: var(--neon-cyan);
            font-family: 'Courier New', Courier, monospace; margin: 0; padding: 0;
            overflow: hidden; height: 100vh; width: 100vw;
            background-image: linear-gradient(rgba(0, 240, 255, 0.03) 1px, transparent 1px),
            linear-gradient(90deg, rgba(0, 240, 255, 0.03) 1px, transparent 1px);
            background-size: 20px 20px;
        }
        
        /* Fullscreen Layout */
        .app-layout {
            display: flex; flex-direction: column; height: 100vh; width: 100vw; position: relative;
        }
        .header-bar {
            display: flex; justify-content: space-between; align-items: center;
            border-bottom: 2px solid var(--neon-pink); padding: 12px 20px; background: rgba(10, 10, 16, 0.95);
            z-index: 10;
        }
        h1 {
            color: var(--neon-pink); text-shadow: 0 0 5px var(--neon-pink); margin: 0;
            font-size: 1.4em; text-transform: uppercase; letter-spacing: 2px;
        }
        .header-actions {
            display: flex; align-items: center; gap: 15px;
        }
        .btn-metrics {
            background: var(--panel-bg); color: var(--neon-yellow); border: 1px solid var(--neon-yellow);
            padding: 6px 12px; cursor: pointer; font-family: inherit; font-size: 0.85em; font-weight: bold;
            text-transform: uppercase; transition: 0.3s;
        }
        .btn-metrics:hover { background: var(--neon-yellow); color: #000; box-shadow: 0 0 10px var(--neon-yellow); }

        .btn-logout {
            color: var(--neon-pink); text-decoration: none; border: 1px solid var(--neon-pink);
            padding: 6px 12px; font-size: 0.85em; transition: 0.3s; font-weight: bold;
            background: transparent; cursor: pointer; font-family: inherit; text-transform: uppercase;
        }
        .btn-logout:hover { background: var(--neon-pink); color: #000; }
        .btn-logout.armed { background: var(--neon-pink); color: #000; box-shadow: 0 0 12px var(--neon-pink); }

        /* Main Workspace (Full Screen Matrix) */
        .main-workspace {
            flex: 1; padding: 20px; overflow-y: auto; position: relative; z-index: 1;
        }
        
        /* Tables */
        table { width: 100%; border-collapse: collapse; margin-top: 10px; }
        th, td { text-align: left; padding: 10px; border-bottom: 1px solid var(--text-dim); }
        th { color: var(--neon-pink); border-bottom: 2px solid var(--neon-pink); }
        tr:hover { background: rgba(0, 240, 255, 0.05); }

        /* Slide Drawers */
        .drawer-btn {
            background: var(--panel-bg); color: var(--neon-yellow); border: 1px solid var(--neon-yellow);
            padding: 6px 12px; cursor: pointer; font-family: inherit; font-size: 0.8em; font-weight: bold;
            text-transform: uppercase; z-index: 20; position: fixed; transition: 0.3s;
        }
        .drawer-btn:hover { background: var(--neon-yellow); color: #000; box-shadow: 0 0 10px var(--neon-yellow); }

        /* Right Telemetry Drawer */
        #right-drawer {
            position: fixed; top: 0; right: -340px; width: 340px; height: 100vh;
            background: rgba(10, 10, 16, 0.98); border-left: 1px solid var(--neon-cyan);
            box-shadow: -5px 0 20px rgba(0, 240, 255, 0.15); transition: right 0.3s ease;
            z-index: 25; padding: 20px; overflow-y: auto;
        }
        #right-drawer.open { right: 0; }

        /* Bottom Log Drawer */
        #bottom-drawer {
            position: fixed; bottom: -35vh; left: 0; width: 100vw; height: 35vh;
            background: rgba(0, 0, 0, 0.98); border-top: 2px solid var(--neon-pink);
            box-shadow: 0 -5px 20px rgba(255, 0, 60, 0.2); transition: bottom 0.3s ease;
            z-index: 15; padding: 15px 20px 20px 20px; display: flex; flex-direction: column;
        }
        #bottom-drawer.open { bottom: 0; }
        .btn-bottom-toggle { bottom: 10px; left: 20px; }

        .log-viewer {
            flex: 1; background: #000; color: #0f0; padding: 12px;
            overflow-y: auto; border: 1px solid var(--text-dim);
            font-size: 0.85em; line-height: 1.4; margin-top: 10px;
        }

        .stat-label { color: var(--text-dim); font-size: 0.8em; text-transform: uppercase; margin-top: 15px; }
        .stat-value { color: var(--neon-cyan); font-size: 1.0em; font-weight: bold; margin-top: 3px; word-break: break-all; }

        .blink { animation: blinker 1.5s linear infinite; }
        @keyframes blinker { 50% { opacity: 0; } }

        /* Secret mode (type "orbitron") */
        #orbitron-secret {
            display: none; position: fixed; inset: 0; z-index: 9998;
            pointer-events: none; color: var(--neon-pink);
            font-family: 'Courier New', Courier, monospace;
        }
        #orbitron-secret.show { display: block; animation: secret-fade 4s ease forwards; }
        #orbitron-secret pre {
            margin: 0; padding: 14px 18px; text-align: center;
            font-size: 2.2em; font-weight: bold; letter-spacing: 4px;
            color: var(--neon-yellow); text-shadow: 0 0 12px var(--neon-pink), 0 0 32px rgba(0,240,255,0.6);
        }
        #orbitron-secret .secret-sub {
            display: block; margin-top: 10px; font-size: 0.55em; letter-spacing: 6px;
            color: var(--neon-cyan); text-shadow: 0 0 10px rgba(0,240,255,0.8);
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
            <h1>Orbitron // Cache Matrix<span class="blink">_</span></h1>
            <div class="header-actions">
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
        <button class="drawer-btn btn-bottom-toggle" onclick="toggleBottomDrawer()">▲ LOG STREAM</button>
        <div id="bottom-drawer">
            <div style="display:flex; justify-content:space-between; align-items:center;">
                <h3 style="margin:0; color:var(--neon-pink);">SYSTEM LOGS // {{LOG_PATH}}</h3>
                <span style="color:var(--text-dim); font-size:0.8em; cursor:pointer; font-weight:bold;" onclick="toggleBottomDrawer()">[ CLOSE ]</span>
            </div>
            <div class="log-viewer" hx-get="/ui/logs" hx-trigger="load, every 5s" id="log-container">
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

        // Tail -f: keep log viewer anchored to the bottom on each refresh,
        // unless the user has scrolled up to read history.
        (function () {
            const container = document.getElementById('log-container');
            if (!container) return;
            let pinned = true;
            const bottomOf = () => container.scrollHeight - container.scrollTop - container.clientHeight < 40;
            container.addEventListener('scroll', function () {
                pinned = bottomOf();
            });
            var logContainer = document.getElementById('log-container');
            const rePin = function () {
                if (pinned || bottomOf()) {
                    container.scrollTop = container.scrollHeight;
                    pinned = true;
                }
            };
            if (window.htmx && window.htmx.on) {
                // htmx 1.x fires "htmx:after:swap"; htmx 2.x fires "htmx:afterSwap".
                window.htmx.on(container, 'htmx:after:swap', rePin);
                window.htmx.on(container, 'htmx:afterSwap', rePin);
            } else {
                document.addEventListener('htmx:after:swap', rePin);
                document.addEventListener('htmx:afterSwap', rePin);
            }
            if (typeof MutationObserver !== 'undefined') {
                new MutationObserver(function () {
                    if (!pinned) { pinned = bottomOf(); }
                    if (pinned || bottomOf()) { container.scrollTop = container.scrollHeight; }
                }).observe(container, { childList: true, characterData: true, subtree: true });
            }
        })();
    </script>
</body>
</html>
`

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	logPath := strings.Replace(htmlTemplate, "{{LOG_PATH}}", d.logSource(), 1)
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
// a cached version, showing "NEVER" when it has no recorded access.
func lastAccessCell(snapshot map[access.Key]time.Time, key access.Key) (string, int64) {
	ts := snapshot[key]
	if ts.IsZero() {
		return `<span class="last-access" data-iso="N/A" style="color:var(--text-dim);">NEVER</span>`, 0
	}
	return fmt.Sprintf(`<span class="last-access" data-iso="%s" style="color:var(--neon-yellow);">%s</span>`, ts.Format(time.RFC3339), humanizeLastAccess(ts)), ts.Unix()
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

	for _, r := range rows {
		fmt.Fprintf(w, `<tr class="version-row" data-group="%s" data-search="%s %s %s" style="display:none"><td></td><td><span style='color:%s;'>%s</span></td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			groupKey, strings.ToLower(typeLabel), strings.ToLower(name), strings.ToLower(r.version),
			color, typeLabel, name, r.version, r.accessStr, formatSize(r.size))
	}
}

func (d *Dashboard) handleStorage(w http.ResponseWriter, r *http.Request) {
	rec := access.New(d.cfg.StoragePath)
	snapshot := rec.Snapshot()

	var html strings.Builder
	html.WriteString("<h2 style='color:var(--neon-yellow); margin-top:0;'>LOCAL CACHE MATRIX</h2>")
	html.WriteString(`<div style="margin-bottom:12px; display:flex; align-items:center; gap:8px;">
		<input id="storage-search" type="text" placeholder="SEARCH TYPE / NAME / VERSION..." style="flex:1; background:rgba(0,0,0,0.55); border:1px solid var(--neon-cyan); border-radius:4px; color:var(--neon-yellow); padding:8px 12px; font-family:inherit; font-size:0.9em; outline:none;">
		<span id="storage-search-count" style="color:var(--text-dim); font-size:0.8em;">0 entries</span>
		<span style="color:var(--text-dim); font-size:0.75em; white-space:nowrap;">CLICK COLUMN HEADERS TO SORT (↕)</span>
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

	// 1. Process Roles
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

	// 2. Process Collections
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

		for fullName, versions := range colMap {
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

	if !hasEntries {
		html.WriteString("<tr><td colspan='6' style='text-align:center; color:var(--text-dim); padding: 30px;'>[ NO ROLES OR COLLECTIONS CACHED YET ]</td></tr>")
	}

	html.WriteString("</tbody></table>")
	html.WriteString(`<div id="iso-tooltip"></div>`)
	html.WriteString(`<style>
.storage-matrix-wrap { overflow-x: auto; }
table#storage-matrix { width: 100%; border-collapse: collapse; }
#storage-matrix th.matrix-head { cursor: pointer; user-select: none; text-align: left; color: var(--neon-cyan); }
#storage-matrix th.matrix-head:hover { color: var(--neon-yellow); }
#storage-matrix th.matrix-head .sort-caret { display: inline-block; width: 0.9em; color: var(--text-dim); transition: color 0.15s ease, text-shadow 0.15s ease; }
#storage-matrix th.matrix-head:hover .sort-caret { color: var(--neon-yellow); }
#storage-matrix th.matrix-head.sorted .sort-caret { color: var(--neon-pink); text-shadow: 0 0 6px rgba(255, 0, 60, 0.6); }
#storage-matrix td, #storage-matrix th { padding: 7px 10px; border-bottom: 1px solid rgba(128,128,128,0.25); }
#storage-matrix tr.group-row { cursor: pointer; }
#storage-matrix tr.group-row:hover { background: rgba(0,240,255,0.09); }
#storage-matrix .expand-caret {
    background: transparent; border: 1px solid var(--neon-cyan); color: var(--neon-yellow);
    width: 22px; height: 20px; font-size: 0.7em; line-height: 1; padding: 0; cursor: pointer;
    font-family: inherit; border-radius: 3px;
}
#storage-matrix .expand-caret:hover { background: var(--neon-yellow); color: #000; }
#storage-matrix tr.version-row td:nth-child(3) { padding-left: 26px; }
#iso-tooltip {
    position: fixed; z-index: 9999; pointer-events: none; opacity: 0;
    transform: translateY(4px); transition: opacity 0.12s ease, transform 0.12s ease;
    background: rgba(2, 6, 10, 0.96); border: 1px solid var(--neon-yellow);
    box-shadow: 0 0 12px rgba(255, 220, 0, 0.25), 0 4px 16px rgba(0,0,0,0.6);
    padding: 7px 11px; font-size: 0.95em; color: var(--neon-yellow);
    font-family: 'Courier New', Courier, monospace; letter-spacing: 0.5px;
    border-radius: 3px; white-space: nowrap;
}
#iso-tooltip .tt-meta { display: block; color: var(--neon-pink); font-size: 0.78em; letter-spacing: 1px; }
#iso-tooltip.show { opacity: 1; transform: translateY(0); }
.last-access { border-bottom: 1px dashed rgba(255, 220, 0, 0.5); cursor: help; }
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

	// Expand state survives the 10s htmx re-renders via window.
	var expanded = window.storageExpandState = window.storageExpandState || {};

	var state = window.storageSortState = window.storageSortState || { col: null, dir: 1 };
	var lastCol = state.col;
	var lastDir = state.dir;
	var inputValue = '';

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
		});
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
			<h3 style="color:var(--neon-yellow); margin:0;">SYSTEM METRICS</h3>
			<span style="color:var(--text-dim); font-size:0.8em; cursor:pointer; font-weight:bold;" onclick="toggleRightDrawer()">[ CLOSE ]</span>
		</div>
		
		<div class="stat-label">Orbitron Version</div>
		<div class="stat-value" style="color:var(--neon-cyan);">%s</div>

		<div class="stat-label">Uplink Status</div>
		<div class="stat-value" style="color:var(--neon-cyan);">%s</div>

		<div class="stat-label">Last Cache Activity</div>
		<div class="stat-value">%s</div>

		<div class="stat-label">System Time</div>
		<div class="stat-value">%s</div>

		<hr style="border-color: var(--text-dim); margin-top:20px;">

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

func (d *Dashboard) handleLogs(w http.ResponseWriter, r *http.Request) {
	if d.cfg.LogPath == "" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w,
			"> File logging is disabled (log_path: \"\"): logs go to %s via the container runtime.<br>"+
				">&nbsp; Live view: <code>nomad alloc logs &lt;alloc-id&gt;</code> or <code>docker compose logs -f</code>.",
			stdoutLogLabel)
		return
	}

	content, err := tailFile(d.cfg.LogPath, 20)
	if err != nil {
		content = []string{fmt.Sprintf("> ERROR READING LOGS: %v", err)}
	}

	w.Header().Set("Content-Type", "text/html")
	for _, line := range content {
		_, _ = w.Write([]byte(html.EscapeString(line) + "<br>"))
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

func tailFile(fileName string, lines int) ([]string, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	stat, _ := file.Stat()
	var size = stat.Size()
	var chunk int64 = 4096

	if chunk > size {
		chunk = size
	}
	buf := make([]byte, chunk)

	if _, err := file.Seek(-chunk, 2); err != nil {
		return nil, err
	}
	if _, err := file.Read(buf); err != nil {
		return nil, err
	}

	linesArr := strings.Split(string(buf), "\n")

	if len(linesArr) > 0 && linesArr[len(linesArr)-1] == "" {
		linesArr = linesArr[:len(linesArr)-1]
	}

	var output []string
	if len(linesArr) > lines {
		output = linesArr[len(linesArr)-lines:]
	} else {
		output = linesArr
	}

	return output, nil
}
