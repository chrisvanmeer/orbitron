package server

import (
	"encoding/json"
	"net/http"

	"orbitron/internal/fetcher"
	"orbitron/internal/logger"
)

// manifestList groups stored requirements manifests by type for the JSON API.
type manifestList struct {
	Roles       []fetcher.ManifestMeta `json:"roles"`
	Collections []fetcher.ManifestMeta `json:"collections"`
}

// HandleManifests returns every stored requirements manifest with its content
// hash and parsed entries. It is the read-only counterpart of the
// POST /api/v1/requirements/{roles,collections} endpoints, letting automation
// detect whether a manifest is already present before re-submitting it.
func (s *Server) HandleManifests(w http.ResponseWriter, r *http.Request) {
	if s.fetcher == nil {
		http.Error(w, "fetcher not initialized", http.StatusInternalServerError)
		return
	}

	metas, err := s.fetcher.ListManifests()
	if err != nil {
		logger.Error("Failed to list manifests: %v", err)
		http.Error(w, "Failed to list manifests", http.StatusInternalServerError)
		return
	}

	list := manifestList{}
	for _, m := range metas {
		if m.Type == "collections" {
			list.Collections = append(list.Collections, m)
		} else {
			list.Roles = append(list.Roles, m)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}
