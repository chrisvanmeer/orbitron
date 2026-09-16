package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"orbitron/auth"
	"orbitron/config"
	"orbitron/fetcher"
	"orbitron/logger"
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

// Helper: Genereer een deterministisch ID op basis van rolnaam
func generateRoleID(roleName string) string {
	h := fnv.New32a()
	h.Write([]byte(roleName))
	return fmt.Sprintf("%d", h.Sum32())
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

func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticateRequest(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) PullAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.RequireAuthPull && !s.authenticateRequest(r) {
			http.Error(w, "Unauthorized: pull authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) HandleRequirementsCollections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	reqs, _ := fetcher.ParseRequirements(body)
	_ = s.fetcher.SaveManifest("collections_requirements.yml", body)

	go func() {
		for _, col := range reqs.Collections {
			_ = s.fetcher.ProcessCollection(col)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"status":"collections_sync_started"}`))
}

func (s *Server) HandleRequirementsRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	reqs, _ := fetcher.ParseRequirements(body)
	_ = s.fetcher.SaveManifest("roles_requirements.yml", body)

	go func() {
		for _, role := range reqs.Roles {
			_ = s.fetcher.ProcessRole(role)
		}
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

// Galaxy Version Discovery
func (s *Server) HandleApiRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"available_versions":{"v1":"v1/","v2":"v2/","v3":"v3/"}}`))
}

// Galaxy V1 Role Router
func (s *Server) HandleGalaxyV1RolesRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasPrefix(path, "/api/v1/roles/download/") {
		s.HandleRoleDownload(w, r)
		return
	}

	// Route requests like: /api/v1/roles/12345/versions/
	if strings.Contains(path, "/versions") {
		parts := strings.Split(path, "/")
		if len(parts) >= 6 {
			roleID := parts[4]
			s.HandleGalaxyV1RoleVersions(w, r, roleID)
			return
		}
	}

	// Base search query
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

// Gives Galaxy the exact versions for the requested role ID
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
	results := []VersionResult{} // Always empty slice instead of nil

	for _, rEntry := range roleEntries {
		if !rEntry.IsDir() {
			continue
		}
		roleName := rEntry.Name()

		// Alleen matchen op de rol die Galaxy zojuist heeft opgevraagd
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
		http.Error(w, "Role version not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.tar.gz", roleName, version))

	_ = tarDirectory(targetDir, w)
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/requirements/collections", s.AuthMiddleware(s.HandleRequirementsCollections))
	mux.HandleFunc("/api/v1/requirements/roles", s.AuthMiddleware(s.HandleRequirementsRoles))
	mux.HandleFunc("/api/v1/sync", s.AuthMiddleware(s.HandleSync))

	mux.HandleFunc("/api/", s.PullAuthMiddleware(s.HandleApiRoot))
	mux.HandleFunc("/api/v1/roles/", s.PullAuthMiddleware(s.HandleGalaxyV1RolesRouter))

	s.httpSrv = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: mux,
	}

	logger.Info("Orbitron server listening on %s", s.cfg.ListenAddr)
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
