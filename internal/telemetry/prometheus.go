package telemetry

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"orbitron/internal/auth"
	"orbitron/internal/config"
)

type Metrics struct {
	startTime time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
	}
}

// Handler returns an http.HandlerFunc serving Prometheus exposition metrics.
// Authentication is handled centrally by server.AuthMiddleware.
func (m *Metrics) Handler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := time.Since(m.startTime).Seconds()
		rolesCount := countItems(filepath.Join(cfg.StoragePath, "roles"), true)
		collectionsCount := countItems(filepath.Join(cfg.StoragePath, "collections"), false)
		manifestsCount := countItems(filepath.Join(cfg.StoragePath, "manifests"), false)

		store, _ := auth.LoadTokens(cfg.TokensFile)
		activeTokensCount := 0
		if store != nil {
			activeTokensCount = len(store.Tokens)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintf(w, "# HELP orbitron_uptime_seconds Total daemon uptime in seconds.\n")
		fmt.Fprintf(w, "# TYPE orbitron_uptime_seconds counter\n")
		fmt.Fprintf(w, "orbitron_uptime_seconds %.2f\n\n", uptime)

		fmt.Fprintf(w, "# HELP orbitron_roles_total Total number of stored Ansible role directories.\n")
		fmt.Fprintf(w, "# TYPE orbitron_roles_total gauge\n")
		fmt.Fprintf(w, "orbitron_roles_total %d\n\n", rolesCount)

		fmt.Fprintf(w, "# HELP orbitron_collections_total Total number of stored Ansible collection archives.\n")
		fmt.Fprintf(w, "# TYPE orbitron_collections_total gauge\n")
		fmt.Fprintf(w, "orbitron_collections_total %d\n\n", collectionsCount)

		fmt.Fprintf(w, "# HELP orbitron_manifests_total Total number of stored requirement manifests.\n")
		fmt.Fprintf(w, "# TYPE orbitron_manifests_total gauge\n")
		fmt.Fprintf(w, "orbitron_manifests_total %d\n\n", manifestsCount)

		fmt.Fprintf(w, "# HELP orbitron_active_tokens_total Total number of registered Bearer tokens.\n")
		fmt.Fprintf(w, "# TYPE orbitron_active_tokens_total gauge\n")
		fmt.Fprintf(w, "orbitron_active_tokens_total %d\n", activeTokensCount)
	}
}

func countItems(dirPath string, countDirectories bool) int {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if countDirectories && entry.IsDir() {
			count++
		} else if !countDirectories && !entry.IsDir() {
			count++
		}
	}
	return count
}
