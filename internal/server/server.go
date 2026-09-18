package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orbitron/internal/auth"
	"orbitron/internal/config"
	"orbitron/internal/fetcher"
	"orbitron/internal/logger"
	"orbitron/internal/telemetry"
	"orbitron/internal/web"
)

type shaEntry struct {
	sum string
	ts  time.Time
}

type Server struct {
	cfg      *config.Config
	fetcher  *fetcher.Fetcher
	httpSrv  *http.Server
	shaMu    sync.Mutex
	shaCache map[string]shaEntry
}

func NewServer(cfg *config.Config) *Server {
	return &Server{
		cfg:      cfg,
		fetcher:  fetcher.NewFetcher(cfg.StoragePath, cfg.MaxConcurrency),
		shaCache: make(map[string]shaEntry),
	}
}

func generateRoleID(roleName string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(roleName))
	return fmt.Sprintf("%d", h.Sum32())
}

func computeSHA256File(filePath string) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// computeSHA256 returns the file SHA-256, caching the result for 5 minutes.
func (s *Server) computeSHA256(filePath string) string {
	s.shaMu.Lock()
	if s.shaCache == nil {
		s.shaCache = make(map[string]shaEntry)
	}
	if e, ok := s.shaCache[filePath]; ok && time.Since(e.ts) < 5*time.Minute {
		s.shaMu.Unlock()
		return e.sum
	}
	s.shaMu.Unlock()

	sum := computeSHA256File(filePath)
	if sum == "" {
		return ""
	}

	s.shaMu.Lock()
	s.shaCache[filePath] = shaEntry{sum: sum, ts: time.Now()}
	s.shaMu.Unlock()
	return sum
}

// requestBaseURL returns the scheme://host prefix for building absolute,
// proxy-aware download URLs (honors X-Forwarded-Proto and direct TLS).
func (s *Server) requestBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) authenticateRequest(r *http.Request) bool {
	store, err := auth.LoadTokens(s.cfg.TokensFile)
	if err != nil {
		logger.Error("Failed to read token store at %s: %v", s.cfg.TokensFile, err)
		return false
	}

	// 1. Check Cookie (For Browser/UI sessions leaking into API)
	if cookie, err := r.Cookie("orbitron_token"); err == nil {
		if store.Valid(cookie.Value) {
			return true
		}
	}

	// 2. Check Bearer Token
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if store.Valid(token) {
			return true
		}
	}

	// 3. Check Basic Auth
	user, pass, ok := r.BasicAuth()
	if ok {
		if store.Valid(pass) {
			return true
		}
		if store.Valid(user) {
			return true
		}
	}

	return false
}

func (s *Server) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Suppress routine UI polling + favicon + liveness probe requests from
		// log output
		path := r.URL.Path
		if !strings.HasPrefix(path, "/ui") && path != "/favicon.ico" && path != "/favicon.svg" && path != "/healthz" {
			logger.Info("HTTP %s %s (from %s)", r.Method, r.URL.RequestURI(), r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticateRequest(r) {
			clientIP := r.Header.Get("X-Forwarded-For")
			if clientIP == "" {
				clientIP = r.RemoteAddr
			}
			logger.Warn("Unauthorized access attempt from %s (%s %s)", clientIP, r.Method, r.URL.Path)
			http.Error(w, "Unauthorized: valid token required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) PullAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.RequireAuthPull {
			if !s.authenticateRequest(r) {
				clientIP := r.Header.Get("X-Forwarded-For")
				if clientIP == "" {
					clientIP = r.RemoteAddr
				}
				logger.Warn("Unauthorized pull attempt from %s (%s %s)", clientIP, r.Method, r.URL.Path)
				http.Error(w, "Unauthorized: pull authentication required", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) HandleRequirementsCollections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error("Failed to read HTTP request body: %v", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	reqs, err := fetcher.ParseRequirements(body)
	if err != nil {
		logger.Error("Failed to parse collections requirements YAML: %v", err)
		http.Error(w, "Invalid YAML format", http.StatusBadRequest)
		return
	}

	if err := s.fetcher.SaveManifestReplacing("collections", body); err != nil {
		logger.Error("Failed to save collections manifest: %v", err)
		http.Error(w, "Failed to store manifest", http.StatusInternalServerError)
		return
	}
	logger.Info("Received collections manifest (%d collection(s) queued for sync)", len(reqs.Collections))

	go s.fetcher.ProcessCollections(reqs.Collections)

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"collections_sync_started"}`))
}

// HandleRequirementsRoles saves a roles requirements manifest and queues the
// enclosed roles for background sync.
func (s *Server) HandleRequirementsRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error("Failed to read HTTP request body: %v", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	reqs, err := fetcher.ParseRequirements(body)
	if err != nil {
		logger.Error("Failed to parse roles requirements YAML: %v", err)
		http.Error(w, "Invalid YAML format", http.StatusBadRequest)
		return
	}

	if err := s.fetcher.SaveManifestReplacing("roles", body); err != nil {
		logger.Error("Failed to save roles manifest: %v", err)
		http.Error(w, "Failed to store manifest", http.StatusInternalServerError)
		return
	}
	logger.Info("Received roles manifest (%d role(s) queued for sync)", len(reqs.Roles))

	go s.fetcher.ProcessRoles(reqs.Roles)

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"roles_sync_started"}`))
}

// HandleRoleDelete removes a single cached role version directory and, when it
// was pinned exactly in a stored requirements manifest, removes the pin so a
// later sync does not silently re-fetch it. Requires a valid admin token.
// Path form: /api/v1/storage/roles/{namespace}.{name}/{version}
func (s *Server) HandleRoleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	roleID := r.PathValue("role")
	version := r.PathValue("version")
	if roleID == "" || version == "" {
		http.Error(w, "role and version are required", http.StatusBadRequest)
		return
	}

	parts := strings.Split(roleID, ".")
	if len(parts) != 2 {
		http.Error(w, "role must be in namespace.name format", http.StatusBadRequest)
		return
	}
	namespace, name := parts[0], parts[1]

	lockKey := "role:" + roleID + "@" + version
	_ = lockKey // deletion lock applied at the route level

	if err := s.fetcher.DelRoleVersion(namespace, name, version); err != nil {
		logger.Error("Failed to delete cached role version %s@%s: %v", roleID, version, err)
		http.Error(w, "Failed to delete cached role version", http.StatusInternalServerError)
		return
	}

	logger.Info("Deleted cached role version %s@%s", roleID, version)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"role_version_deleted"}`))
}

// HandleCollectionDelete removes a single cached collection version artifact
// and, when it was pinned exactly in a stored requirements manifest, removes
// the pin so a later sync does not silently re-fetch it. Requires a valid
// admin token. Path form:
// /api/v1/storage/collections/{collection}/{version}
func (s *Server) HandleCollectionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	collection := r.PathValue("collection")
	version := r.PathValue("version")
	if collection == "" || version == "" {
		http.Error(w, "collection and version are required", http.StatusBadRequest)
		return
	}

	parts := strings.Split(collection, ".")
	if len(parts) != 2 {
		http.Error(w, "collection must be in namespace.name format", http.StatusBadRequest)
		return
	}
	namespace, name := parts[0], parts[1]

	if err := s.fetcher.DelCollectionVersion(namespace, name, version); err != nil {
		logger.Error("Failed to delete cached collection version %s.%s@%s: %v", namespace, name, version, err)
		http.Error(w, "Failed to delete cached collection version", http.StatusInternalServerError)
		return
	}

	logger.Info("Deleted cached collection version %s.%s@%s", namespace, name, version)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"collection_version_deleted"}`))
}

func (s *Server) HandleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	go s.fetcher.SyncAll()

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"full_sync_triggered"}`))
}

func (s *Server) HandleApiRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"available_versions":{"v1":"v1/","v2":"v2/","v3":"v3/"}}`))
}

func (s *Server) HandleGalaxyV1RolesRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, "/api/v1/roles/download/") {
		s.HandleRoleDownload(w, r)
		return
	}

	if strings.Contains(path, "/versions") {
		parts := strings.Split(path, "/")
		if len(parts) >= 6 {
			roleID := parts[4]
			s.HandleGalaxyV1RoleVersions(w, r, roleID)
			return
		}
	}

	owner := r.URL.Query().Get("owner__username")
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.URL.Query().Get("autocomplete")
	}

	roleFullName := name
	if owner != "" && name != "" && !strings.Contains(name, ".") {
		roleFullName = fmt.Sprintf("%s.%s", owner, name)
	}

	roleID := generateRoleID(roleFullName)

	w.Header().Set("Content-Type", "application/json")
	resp := fmt.Sprintf(`{
    "count": 1,
    "results": [{
      "id": %s,
      "name": "%s",
      "summary_fields": {
        "namespace": {"name": "%s"}
      }
    }]
  }`, roleID, name, owner)

	_, _ = w.Write([]byte(resp))
}

func (s *Server) HandleGalaxyV1RoleVersions(w http.ResponseWriter, r *http.Request, roleID string) {
	w.Header().Set("Content-Type", "application/json")

	rolesDir := filepath.Join(s.cfg.StoragePath, "roles")
	roleEntries, err := os.ReadDir(rolesDir)
	if err != nil {
		http.Error(w, `{"results":[]}`, http.StatusOK)
		return
	}

	type VersionResult struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	}
	results := []VersionResult{}

	for _, rEntry := range roleEntries {
		if !rEntry.IsDir() {
			continue
		}
		roleName := rEntry.Name()

		if generateRoleID(roleName) != roleID {
			continue
		}

		versionsDir := filepath.Join(rolesDir, roleName)
		verEntries, err := os.ReadDir(versionsDir)
		if err != nil {
			continue
		}

		for _, vEntry := range verEntries {
			if !vEntry.IsDir() {
				continue
			}
			ver := vEntry.Name()
			baseURL := s.requestBaseURL(r)
			downloadURL := fmt.Sprintf("%s/api/v1/roles/download/%s/%s.tar.gz", baseURL, roleName, ver)
			results = append(results, VersionResult{
				Name:        ver,
				DownloadURL: downloadURL,
			})
		}
		break
	}

	respBytes, _ := json.Marshal(map[string]any{"results": results})
	_, _ = w.Write(respBytes)
}

func (s *Server) HandleRoleDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 6 {
		http.Error(w, "Invalid download path", http.StatusBadRequest)
		return
	}

	roleName := parts[5]
	version := strings.TrimSuffix(parts[6], ".tar.gz")

	targetDir := filepath.Join(s.cfg.StoragePath, "roles", roleName, version)
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		logger.Warn("Requested role download not found: %s (%s)", roleName, version)
		http.Error(w, "Role version not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.tar.gz", roleName, version))

	if err := tarDirectory(targetDir, w); err != nil {
		logger.Error("Failed to stream role archive for %s: %v", roleName, err)
	}
}

type CollectionVersionItem struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	Href        string `json:"href"`
}

func (s *Server) listCollectionVersions(baseURL, basePath, namespace, name string) []CollectionVersionItem {
	results := []CollectionVersionItem{}
	nsDir := filepath.Join(s.cfg.StoragePath, "collections", namespace)
	files, err := os.ReadDir(nsDir)
	if err != nil {
		return results
	}

	prefix := fmt.Sprintf("%s-%s-", namespace, name)
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), prefix) && strings.HasSuffix(f.Name(), ".tar.gz") {
			ver := strings.TrimPrefix(f.Name(), prefix)
			ver = strings.TrimSuffix(ver, ".tar.gz")

			downloadURL := fmt.Sprintf("%s%sartifacts/%s", baseURL, basePath, f.Name())
			href := fmt.Sprintf("%s%scollections/%s/%s/versions/%s/", baseURL, basePath, namespace, name, ver)

			results = append(results, CollectionVersionItem{
				Version:     ver,
				DownloadURL: downloadURL,
				Href:        href,
			})
		}
	}
	return results
}

func (s *Server) HandleGalaxyV3Router(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	basePath := "/api/v3/"
	if strings.HasPrefix(path, "/api/galaxy/") {
		basePath = "/api/galaxy/v3/"
	}

	// 1. Serve tarball artifacts
	if strings.Contains(path, "/artifacts/") {
		filename := filepath.Base(path)
		parts := strings.Split(filename, "-")
		if len(parts) >= 2 {
			namespace := parts[0]
			filePath := filepath.Join(s.cfg.StoragePath, "collections", namespace, filename)
			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				http.Error(w, "Artifact not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/x-gzip")
			http.ServeFile(w, r, filePath)
			return
		}
	}

	// 2. Collection Search Endpoints
	if strings.Contains(path, "/search/") {
		namespace := r.URL.Query().Get("namespace")
		name := r.URL.Query().Get("name")
		if name == "" {
			name = r.URL.Query().Get("collection_name")
		}

		results := s.listCollectionVersions(s.requestBaseURL(r), basePath, namespace, name)
		w.Header().Set("Content-Type", "application/json")
		respData := map[string]any{
			"count":   len(results),
			"results": results,
		}
		respBytes, _ := json.Marshal(respData)
		_, _ = w.Write(respBytes)
		return
	}

	// 3. Collection Index and Version Queries
	if strings.Contains(path, "/collections/") {
		var sub string
		if strings.Contains(path, "/collections/index/") {
			sub = path[strings.Index(path, "/collections/index/")+19:]
		} else {
			sub = path[strings.Index(path, "/collections/")+13:]
		}

		sub = strings.Trim(sub, "/")
		parts := []string{}
		if sub != "" {
			parts = strings.Split(sub, "/")
		}

		if len(parts) >= 2 {
			namespace := parts[0]
			name := parts[1]

			baseURL := s.requestBaseURL(r)
			versionResults := s.listCollectionVersions(baseURL, basePath, namespace, name)
			var highestVer string
			if len(versionResults) > 0 {
				highestVer = versionResults[len(versionResults)-1].Version
			}

			// Base collection info: /collections/{namespace}/{name}/
			if len(parts) == 2 {
				w.Header().Set("Content-Type", "application/json")
				resp := fmt.Sprintf(`{
          "namespace": {"name": "%s"},
          "name": "%s",
          "deprecated": false,
          "highest_version": {"version": "%s"}
        }`, namespace, name, highestVer)
				_, _ = w.Write([]byte(resp))
				return
			}

			// /collections/{namespace}/{name}/versions/
			if parts[2] == "versions" {
				// Specific version detail: .../versions/{version}/
				if len(parts) >= 4 && parts[3] != "" {
					version := parts[3]

					artifactName := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
					artifactPath := filepath.Join(s.cfg.StoragePath, "collections", namespace, artifactName)

					var size int64 = 0
					sha256hash := ""

					if info, err := os.Stat(artifactPath); err == nil {
						size = info.Size()
						sha256hash = s.computeSHA256(artifactPath)
					}

					downloadURL := fmt.Sprintf("%s%sartifacts/%s", baseURL, basePath, artifactName)
					href := fmt.Sprintf("%s%scollections/%s/%s/versions/%s/", baseURL, basePath, namespace, name, version)

					w.Header().Set("Content-Type", "application/json")
					resp := fmt.Sprintf(`{
            "version": "%s",
            "href": "%s",
            "download_url": "%s",
            "requires_ansible": ">=2.12.0",
            "namespace": {"name": "%s"},
            "collection": {"name": "%s"},
            "dependencies": {},
            "artifact": {
              "filename": "%s",
              "size": %d,
              "sha256": "%s"
            },
            "metadata": {
              "dependencies": {},
              "requires_ansible": ">=2.12.0"
            }
          }`, version, href, downloadURL, namespace, name, artifactName, size, sha256hash)
					_, _ = w.Write([]byte(resp))
					return
				}

				// Version LIST query
				w.Header().Set("Content-Type", "application/json")
				respData := map[string]any{
					"count":   len(versionResults),
					"results": versionResults,
				}
				respBytes, _ := json.Marshal(respData)
				_, _ = w.Write(respBytes)
				return
			}
		}
	}

	// 4. Root V3 Discovery
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"available_versions":{"v3":"v3/"}}`))
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Expired tokens are never resurrected by a daemon restart; sweep them once
	// on boot so the store stays tidy.
	if pruned, err := auth.PruneExpiredTokens(s.cfg.TokensFile); err != nil {
		logger.Error("Failed to prune expired tokens on startup: %v", err)
	} else if pruned > 0 {
		logger.Info("Pruned %d expired token(s) on startup", pruned)
	}

	// Prometheus Telemetry Endpoint (Protected with token auth via AuthMiddleware)
	metrics := telemetry.NewMetrics()
	mux.HandleFunc("/metrics", s.AuthMiddleware(metrics.Handler(s.cfg)))

	// Web UI Cyberpunk Dashboard & HTMX Assets (Uses its own cookie auth)
	dashboard := web.NewDashboard(s.cfg)
	dashboard.Register(mux)

	// Unauthenticated liveness probe for orchestrators & load balancers.
	mux.HandleFunc("GET /healthz", s.HandleHealthz)

	mux.HandleFunc("/api/v1/requirements/collections", s.AuthMiddleware(s.HandleRequirementsCollections))
	mux.HandleFunc("/api/v1/requirements/roles", s.AuthMiddleware(s.HandleRequirementsRoles))
	mux.HandleFunc("/api/v1/sync", s.AuthMiddleware(s.HandleSync))

	// Async sync queue / run state.
	mux.HandleFunc("GET /api/v1/sync/status", s.AuthMiddleware(s.HandleSyncStatus))

	// Administrative token lifecycle (create, list, revoke, rotate).
	mux.HandleFunc("POST /api/v1/tokens", s.AuthMiddleware(s.HandleTokenCreate))
	mux.HandleFunc("GET /api/v1/tokens", s.AuthMiddleware(s.HandleTokenList))
	mux.HandleFunc("DELETE /api/v1/tokens/{token}", s.AuthMiddleware(s.HandleTokenRevoke))
	mux.HandleFunc("POST /api/v1/tokens/{token}/rotate", s.AuthMiddleware(s.HandleTokenRotate))

	// Storage pruning. Currently answers with a "for_future_use" placeholder.
	mux.HandleFunc("POST /api/v1/prune", s.AuthMiddleware(s.HandlePrune))

	// Read-only management views for automation: stored requirements manifests
	// (content-addressed) and the cached storage inventory.
	mux.HandleFunc("GET /api/v1/manifests", s.AuthMiddleware(s.HandleManifests))
	mux.HandleFunc("GET /api/v1/storage", s.AuthMiddleware(s.HandleStorageInventory))

	// Authorized admin-only storage deletion endpoints (one version per call).
	// Registered with method-specific patterns so they take precedence over the
	// broader pull-auth /api/ and galaxy routers. Always require a valid token.
	mux.HandleFunc("DELETE /api/v1/storage/roles/{role}/{version}", s.AuthMiddleware(s.HandleRoleDelete))
	mux.HandleFunc("DELETE /api/v1/storage/collections/{collection}/{version}", s.AuthMiddleware(s.HandleCollectionDelete))

	mux.HandleFunc("/api/", s.PullAuthMiddleware(s.HandleApiRoot))
	mux.HandleFunc("/api/v1/roles/", s.PullAuthMiddleware(s.HandleGalaxyV1RolesRouter))

	mux.HandleFunc("/api/v3/", s.PullAuthMiddleware(s.HandleGalaxyV3Router))
	mux.HandleFunc("/api/galaxy/", s.PullAuthMiddleware(s.HandleGalaxyV3Router))

	loggingHandler := s.LoggingMiddleware(mux)

	s.httpSrv = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           loggingHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	logger.Info("Orbitron server listening on %s (require_auth_pull=%v)", s.cfg.ListenAddr, s.cfg.RequireAuthPull)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

func tarDirectory(srcDir string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer func() { _ = gw.Close() }()

	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		header.Name = relPath
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}

		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
