package telemetry

import (
	"fmt"
	"net/http"
	"time"

	"orbitron/internal/auth"
	"orbitron/internal/config"
	"orbitron/internal/inventory"
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
		inv := inventory.Scan(cfg.StoragePath)

		store, _ := auth.LoadTokens(cfg.TokensFile)
		activeTokens := 0
		if store != nil {
			activeTokens = store.ActiveCount()
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		_, _ = fmt.Fprintf(w, "# HELP orbitron_uptime_seconds Total daemon uptime in seconds.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_uptime_seconds counter\n")
		_, _ = fmt.Fprintf(w, "orbitron_uptime_seconds %.2f\n\n", time.Since(m.startTime).Seconds())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_roles_total Number of distinct cached role names.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_roles_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_roles_total %d\n\n", inv.RoleCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_collections_total Number of distinct cached collection names.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_collections_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_collections_total %d\n\n", inv.CollectionCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_role_versions_total Number of cached role versions across all roles.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_role_versions_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_role_versions_total %d\n\n", inv.RoleVersionCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_collection_versions_total Number of cached collection versions across all collections.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_collection_versions_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_collection_versions_total %d\n\n", inv.CollectionVersionCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_cached_versions_total Number of cached versions of roles and collections combined.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_cached_versions_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_cached_versions_total %d\n\n", inv.RoleVersionCount()+inv.CollectionVersionCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_namespaces_total Number of distinct collection namespaces in the cache.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_namespaces_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_namespaces_total %d\n\n", inv.NamespaceCount())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_storage_bytes Block-allocated disk usage of all cached roles and collections in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_storage_bytes gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_storage_bytes %d\n\n", inv.StorageBytes())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_roles_bytes Block-allocated disk usage of cached role versions in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_roles_bytes gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_roles_bytes %d\n\n", inv.RoleBytes())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_collections_bytes Block-allocated disk usage of cached collection archives in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_collections_bytes gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_collections_bytes %d\n\n", inv.CollectionBytes())

		_, _ = fmt.Fprintf(w, "# HELP orbitron_active_tokens_total Number of registered Bearer tokens.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_active_tokens_total gauge\n")
		_, _ = fmt.Fprintf(w, "orbitron_active_tokens_total %d\n\n", activeTokens)

		_, _ = fmt.Fprintf(w, "# HELP orbitron_cached_item_bytes Block-allocated disk usage of a cached role or collection across all of its versions in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_cached_item_bytes gauge\n")
		for _, it := range inv.Roles {
			_, _ = fmt.Fprintf(w, "orbitron_cached_item_bytes{type=\"role\",name=%q} %d\n", it.Name, itemBytes(it))
		}
		for _, it := range inv.Collections {
			_, _ = fmt.Fprintf(w, "orbitron_cached_item_bytes{type=\"collection\",name=%q} %d\n", it.Name, itemBytes(it))
		}

		_, _ = fmt.Fprintf(w, "\n# HELP orbitron_cached_version_bytes Block-allocated disk usage of a single cached role or collection version in bytes.\n")
		_, _ = fmt.Fprintf(w, "# TYPE orbitron_cached_version_bytes gauge\n")
		for _, it := range inv.Roles {
			for _, v := range it.Versions {
				_, _ = fmt.Fprintf(w, "orbitron_cached_version_bytes{type=\"role\",name=%q,version=%q} %d\n",
					it.Name, v.Version, v.SizeBytes)
			}
		}
		for _, it := range inv.Collections {
			for _, v := range it.Versions {
				_, _ = fmt.Fprintf(w, "orbitron_cached_version_bytes{type=\"collection\",name=%q,version=%q} %d\n",
					it.Name, v.Version, v.SizeBytes)
			}
		}
	}
}

func itemBytes(it inventory.Item) int64 {
	var n int64
	for _, v := range it.Versions {
		n += v.SizeBytes
	}
	return n
}
