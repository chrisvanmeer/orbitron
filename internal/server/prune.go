package server

import (
	"encoding/json"
	"net/http"

	"orbitron/internal/pruner"
)

// pruneAPIEnabled gates the HTTP prune endpoint. It is enabled once the pruner
// has been migrated to the access-based retention model: the endpoint accepts a
// {"dry_run": true, "days": N} payload and relays it directly to the
// structured RUN/DRY-RUN pruner API.
const pruneAPIEnabled = true

// HandlePrune exposes storage pruning over HTTP. A JSON body of
// {"dry_run": true, "days": N} reports what would be pruned without deleting;
// {"dry_run": false} (or omitted) deletes for real. If days is omitted the
// default retention window of 90 days is used.
func (s *Server) HandlePrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DryRun bool `json:"dry_run"`
		Days   int  `json:"days"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Days <= 0 {
		req.Days = 90
	}

	if !pruneAPIEnabled {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "for_future_use",
			"message": "Orbitron's HTTP prune API is not yet available.",
		})
		return
	}

	p := pruner.NewPruner(s.cfg.StoragePath)
	result, err := p.RunPruneAPI(req.DryRun, req.Days)
	if err != nil {
		http.Error(w, "Prune failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
