package server

import (
	"encoding/json"
	"net/http"

	"orbitron/internal/pruner"
)

// pruneAPIEnabled gates the HTTP prune endpoint. The full implementation is
// wired up below; until the flag flips to true, callers receive a
// "for_future_use" 501 response and nothing is deleted.
const pruneAPIEnabled = false

// HandlePrune exposes storage pruning over HTTP. While pruneAPIEnabled is false
// the endpoint answers with a "for future use" placeholder so automation can
// discover the contract without any side effects.
func (s *Server) HandlePrune(w http.ResponseWriter, r *http.Request) {
	if !pruneAPIEnabled {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "for_future_use",
			"message": "Orbitron's HTTP prune API is not yet available.",
		})
		return
	}

	var req struct {
		DryRun bool `json:"dry_run"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	p := pruner.NewPruner(s.cfg.StoragePath)
	result, err := p.RunPruneAPI(req.DryRun)
	if err != nil {
		http.Error(w, "Prune failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
