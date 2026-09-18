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

	"orbitron/internal/auth"
	"orbitron/internal/build"
	"orbitron/internal/config"
	"orbitron/internal/fetcher"
	"orbitron/internal/logger"
	ver "orbitron/internal/version"
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
    </style>
</head>
<body>
    <div class="app-layout">
        <div class="header-bar">
            <h1>Orbitron // Cache Matrix<span class="blink">_</span></h1>
            <div class="header-actions">
                <button class="btn-metrics" onclick="toggleRightDrawer()">◄ SYS METRICS</button>
                <button id="btn-disconnect" class="btn-logout" onclick="armDisconnect()">[ DISCONNECT ]</button>
            </div>
        </div>

        <!-- Fullscreen Local Cache Matrix -->
        <div class="main-workspace" hx-get="/ui/storage" hx-trigger="load, every 30s">
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
    </script>
</body>
</html>
`

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	logPath := strings.Replace(htmlTemplate, "{{LOG_PATH}}", d.logLabel(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(logPath))
}

func (d *Dashboard) logLabel() string {
	if d.cfg.LogPath != "" {
		return d.cfg.LogPath
	}
	return "/var/log/orbitron/orbitron.log"
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

func (d *Dashboard) handleStorage(w http.ResponseWriter, r *http.Request) {
	activeVersions, _ := getManifestStats(d.cfg.StoragePath)

	var html strings.Builder
	html.WriteString("<h2 style='color:var(--neon-yellow); margin-top:0;'>LOCAL CACHE MATRIX</h2>")
	html.WriteString("<table><tr><th>Type</th><th>Name</th><th>Version</th><th>Disk Usage</th></tr>")

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

		var versions []string
		for _, vEntry := range verEntries {
			if vEntry.IsDir() {
				versions = append(versions, vEntry.Name())
			}
		}

		sort.Sort(sort.Reverse(sort.StringSlice(versions)))

		for _, version := range versions {
			hasEntries = true
			verPath := filepath.Join(versionsDir, version)
			size := d.dirSize(verPath)

			verInManifest, declared := activeVersions[roleName]
			normVerInManifest := strings.TrimPrefix(verInManifest, "v")
			normVerOnDisk := strings.TrimPrefix(version, "v")

			legacyActive := verInManifest == "main" ||
				verInManifest == "master" ||
				verInManifest == "HEAD"
			isActive := declared && (ver.ShouldKeep(verInManifest, versions, version) ||
				legacyActive ||
				(normVerInManifest != "" && normVerInManifest == normVerOnDisk))

			var verHTML string
			if isActive {
				verHTML = fmt.Sprintf(`<span style="color:var(--neon-yellow); font-weight:bold;">%s &nbsp;<span style="font-size:0.8em; color:var(--neon-pink);">[ACTIVE]</span></span>`, version)
			} else {
				verHTML = fmt.Sprintf(`<span style="color:var(--text-dim);">%s</span>`, version)
			}

			fmt.Fprintf(&html, "<tr><td><span style='color:var(--neon-pink);'>ROLE</span></td><td>%s</td><td>%s</td><td>%s</td></tr>", roleName, verHTML, formatSize(size))
		}
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

			for _, version := range versions {
				hasEntries = true
				size := sizeMap[fullName+"@"+version]

				verInManifest, declared := activeVersions[fullName]
				normVerInManifest := strings.TrimPrefix(verInManifest, "v")
				normVerOnDisk := strings.TrimPrefix(version, "v")

				legacyActive := verInManifest == "main" ||
					verInManifest == "master" ||
					verInManifest == "HEAD"
				isActive := declared && (ver.ShouldKeep(verInManifest, versions, version) ||
					legacyActive ||
					(normVerInManifest != "" && normVerInManifest == normVerOnDisk))

				var verHTML string
				if isActive {
					verHTML = fmt.Sprintf(`<span style="color:var(--neon-yellow); font-weight:bold;">%s &nbsp;<span style="font-size:0.8em; color:var(--neon-pink);">[ACTIVE]</span></span>`, version)
				} else {
					verHTML = fmt.Sprintf(`<span style="color:var(--text-dim);">%s</span>`, version)
				}

				fmt.Fprintf(&html, "<tr><td><span style='color:var(--neon-cyan);'>COLLECTION</span></td><td>%s</td><td>%s</td><td>%s</td></tr>", fullName, verHTML, formatSize(size))
			}
		}
	}

	if !hasEntries {
		html.WriteString("<tr><td colspan='4' style='text-align:center; color:var(--text-dim); padding: 30px;'>[ NO ROLES OR COLLECTIONS CACHED YET ]</td></tr>")
	}

	html.WriteString("</table>")
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(html.String()))
}

func (d *Dashboard) handleSyncTime(w http.ResponseWriter, r *http.Request) {
	_, lastSync := getManifestStats(d.cfg.StoragePath)

	status := "ONLINE"
	if lastSync.IsZero() {
		status = "NO MANIFEST INGESTED"
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

		<div class="stat-label">Last Manifest Ingest</div>
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
	content, err := tailFile(d.logLabel(), 20)
	if err != nil {
		content = []string{fmt.Sprintf("> ERROR READING LOGS: %v", err)}
	}

	w.Header().Set("Content-Type", "text/html")
	for _, line := range content {
		_, _ = w.Write([]byte(html.EscapeString(line) + "<br>"))
	}
}

// --- Metrics & System Helpers ---

func extractRoleName(name, src string) string {
	target := strings.TrimSpace(name)
	if target == "" {
		target = strings.TrimSpace(src)
	}
	if target == "" {
		return ""
	}
	target = strings.TrimSuffix(target, ".git")
	if idx := strings.LastIndexAny(target, "/:"); idx != -1 {
		target = target[idx+1:]
	}
	return strings.Trim(target, "\"'")
}

func getManifestStats(storagePath string) (map[string]string, time.Time) {
	active := make(map[string]string)
	var lastSync time.Time

	_ = filepath.Walk(storagePath, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), "_requirements.yml") {
			if info.ModTime().After(lastSync) {
				lastSync = info.ModTime()
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			reqs, err := fetcher.ParseRequirements(data)
			if err == nil {
				for _, r := range reqs.Roles {
					roleName := extractRoleName(r.Name, r.Src)
					if roleName != "" {
						ver := strings.Trim(strings.TrimSpace(r.Version), "\"'")
						active[roleName] = ver
					}
				}
				for _, c := range reqs.Collections {
					name := strings.Trim(strings.TrimSpace(c.Name), "\"'")
					if name != "" {
						ver := strings.Trim(strings.TrimSpace(c.Version), "\"'")
						active[name] = ver
					}
				}
			}
		}
		return nil
	})

	return active, lastSync
}

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
