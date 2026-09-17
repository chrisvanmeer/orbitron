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

	"orbitron/internal/auth"
	"orbitron/internal/config"
	"orbitron/internal/fetcher"
	"orbitron/internal/logger"
	"orbitron/internal/telemetry"
)

type Server struct {
	cfg     *config.Config
	fetcher *fetcher.Fetcher
	httpSrv *http.Server
}

func NewServer(cfg *config.Config) *Server {
	return &Server{
		cfg:     cfg,
		fetcher: fetcher.NewFetcher(cfg.StoragePath),
	}
}

func generateRoleID(roleName string) string {
	h := fnv.New32a()
	h.Write([]byte(roleName))
	return fmt.Sprintf("%d", h.Sum32())
}

func computeSHA256(filePath string) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *Server) authenticateRequest(r *http.Request) bool {
	store, err := auth.LoadTokens(s.cfg.TokensFile)
	if err != nil {
		logger.Error("Failed to read token store at %s: %v", s.cfg.TokensFile, err)
		return false
	}

	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if store.Tokens[token] {
			return true
		}
	}

	user, pass, ok := r.BasicAuth()
	if ok {
		if store.Tokens[pass] || store.Tokens[user] {
			return true
		}
	}

	return false
}

func (s *Server) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("HTTP %s %s (from %s)", r.Method, r.URL.RequestURI(), r.RemoteAddr)
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

	_ = s.fetcher.SaveManifest("collections_requirements.yml", body)
	logger.Info("Received collections manifest (%d collection(s) queued for sync)", len(reqs.Collections))

	go func() {
		var wg sync.WaitGroup
		for _, col := range reqs.Collections {
			wg.Add(1)
			go func(c fetcher.CollectionItem) {
				defer wg.Done()
				if err := s.fetcher.ProcessCollection(c); err != nil {
					logger.Error("Error processing collection (%s): %v", c.Name, err)
				}
			}(col)
		}
		wg.Wait()
		logger.Info("Immediate collection requirements sync completed.")
	}()

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"collections_sync_started"}`))
}

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

	_ = s.fetcher.SaveManifest("roles_requirements.yml", body)
	logger.Info("Received roles manifest (%d role(s) queued for sync)", len(reqs.Roles))

	go func() {
		var wg sync.WaitGroup
		for _, role := range reqs.Roles {
			wg.Add(1)
			go func(r fetcher.RoleItem) {
				defer wg.Done()
				if err := s.fetcher.ProcessRole(r); err != nil {
					logger.Error("Error processing role (%s): %v", r.Name, err)
				}
			}(role)
		}
		wg.Wait()
		logger.Info("Immediate role requirements sync completed.")
	}()

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"roles_sync_started"}`))
}

func (s *Server) HandleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	go s.fetcher.SyncAll()

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"full_sync_triggered"}`))
}

func (s *Server) HandleApiRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"available_versions":{"v1":"v1/","v2":"v2/","v3":"v3/"}}`))
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

	w.Write([]byte(resp))
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
			downloadURL := fmt.Sprintf("http://%s/api/v1/roles/download/%s/%s.tar.gz", r.Host, roleName, ver)
			results = append(results, VersionResult{
				Name:        ver,
				DownloadURL: downloadURL,
			})
		}
		break
	}

	respBytes, _ := json.Marshal(map[string]any{"results": results})
	w.Write(respBytes)
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

func (s *Server) listCollectionVersions(host, basePath, namespace, name string) []CollectionVersionItem {
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

			downloadURL := fmt.Sprintf("http://%s%sartifacts/%s", host, basePath, f.Name())
			href := fmt.Sprintf("http://%s%scollections/%s/%s/versions/%s/", host, basePath, namespace, name, ver)

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

		results := s.listCollectionVersions(r.Host, basePath, namespace, name)
		w.Header().Set("Content-Type", "application/json")
		respData := map[string]any{
			"count":   len(results),
			"results": results,
		}
		respBytes, _ := json.Marshal(respData)
		w.Write(respBytes)
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

			versionResults := s.listCollectionVersions(r.Host, basePath, namespace, name)
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
				w.Write([]byte(resp))
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
					var sha256hash string = ""

					if info, err := os.Stat(artifactPath); err == nil {
						size = info.Size()
						sha256hash = computeSHA256(artifactPath)
					}

					downloadURL := fmt.Sprintf("http://%s%sartifacts/%s", r.Host, basePath, artifactName)
					href := fmt.Sprintf("http://%s%scollections/%s/%s/versions/%s/", r.Host, basePath, namespace, name, version)

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
					w.Write([]byte(resp))
					return
				}

				// Version LIST query
				w.Header().Set("Content-Type", "application/json")
				respData := map[string]any{
					"count":   len(versionResults),
					"results": versionResults,
				}
				respBytes, _ := json.Marshal(respData)
				w.Write(respBytes)
				return
			}
		}
	}

	// 4. Root V3 Discovery
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"available_versions":{"v3":"v3/"}}`))
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Prometheus Telemetry Endpoint (Beveiligd met token auth via AuthMiddleware)
	metrics := telemetry.NewMetrics()
	mux.HandleFunc("/metrics", s.AuthMiddleware(metrics.Handler(s.cfg)))

	mux.HandleFunc("/api/v1/requirements/collections", s.AuthMiddleware(s.HandleRequirementsCollections))
	mux.HandleFunc("/api/v1/requirements/roles", s.AuthMiddleware(s.HandleRequirementsRoles))
	mux.HandleFunc("/api/v1/sync", s.AuthMiddleware(s.HandleSync))

	mux.HandleFunc("/api/", s.PullAuthMiddleware(s.HandleApiRoot))
	mux.HandleFunc("/api/v1/roles/", s.PullAuthMiddleware(s.HandleGalaxyV1RolesRouter))

	mux.HandleFunc("/api/v3/", s.PullAuthMiddleware(s.HandleGalaxyV3Router))
	mux.HandleFunc("/api/galaxy/", s.PullAuthMiddleware(s.HandleGalaxyV3Router))

	loggingHandler := s.LoggingMiddleware(mux)

	s.httpSrv = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: loggingHandler,
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
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

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
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}
