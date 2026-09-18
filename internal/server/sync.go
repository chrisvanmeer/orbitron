package server

import (
	"encoding/json"
	"net/http"

	"orbitron/internal/fetcher"
)

// HandleSyncStatus returns the live state of background role/collection syncs:
// the currently running job (if any) plus a bounded history of finished jobs.
func (s *Server) HandleSyncStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.fetcher == nil {
		_ = json.NewEncoder(w).Encode(fetcher.SyncSnapshot{
			History: []fetcher.SyncJob{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(s.fetcher.StatusSnapshot())
}
