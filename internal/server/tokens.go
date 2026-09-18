package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"orbitron/internal/auth"
	"orbitron/internal/logger"
)

// tokenRequestBody is the accepted payload for creating or rotating tokens. The
// pointer TTLDays distinguishes an explicit "0" (never expire) from "unset".
type tokenRequestBody struct {
	TTLDays *int   `json:"ttl_days"`
	Label   string `json:"label"`
}

// tokenResponse is the serializable lifecycle of a token as returned by the API.
type tokenResponse struct {
	Token     string `json:"token,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Label     string `json:"label,omitempty"`
}

// HandleTokenCreate generates a new administrative token. An optional JSON body
// may override the global token_ttl_days and attach a label.
func (s *Server) HandleTokenCreate(w http.ResponseWriter, r *http.Request) {
	_, ttl, label, err := s.parseTokenRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	token, info, err := auth.GenerateToken(s.cfg.TokensFile, label, ttl)
	if err != nil {
		http.Error(w, "Failed to generate token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Generated new administrative token via API (label=%q)", label)
	writeTokenResponse(w, token, info)
}

// HandleTokenList returns metadata for every registered token. Raw token values
// are omitted unless the ?full=true query parameter is supplied.
func (s *Server) HandleTokenList(w http.ResponseWriter, r *http.Request) {
	store, err := auth.LoadTokens(s.cfg.TokensFile)
	if err != nil {
		http.Error(w, "Failed to load token store: "+err.Error(), http.StatusInternalServerError)
		return
	}

	includeSecrets := r.URL.Query().Get("full") == "true"

	results := make([]tokenResponse, 0, len(store.Tokens))
	for token, info := range store.Tokens {
		entry := tokenResponse{
			CreatedAt: info.CreatedAt,
			ExpiresAt: info.ExpiresAt,
			Label:     info.Label,
		}
		if includeSecrets {
			entry.Token = token
		}
		results = append(results, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": results})
}

// HandleTokenRevoke removes a token by value.
func (s *Server) HandleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	if err := auth.RevokeToken(s.cfg.TokensFile, token); err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			http.Error(w, "Token not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to revoke token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Revoked token via API")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"revoked"}`))
}

// HandleTokenRotate replaces a token with a freshly generated one carrying the
// same label and remaining lifetime, and revokes the original.
func (s *Server) HandleTokenRotate(w http.ResponseWriter, r *http.Request) {
	oldToken := r.PathValue("token")
	if oldToken == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	_, _, label, err := s.parseTokenRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	newToken, info, err := auth.RotateToken(s.cfg.TokensFile, oldToken, label)
	if err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) {
			http.Error(w, "Token not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to rotate token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Rotated token via API")
	writeTokenResponse(w, newToken, info)
}

// parseTokenRequest reads an optional JSON body and resolves the effective TTL:
// a per-request ttl_days always wins over the configured token_ttl_days default.
func (s *Server) parseTokenRequest(r *http.Request) ([]byte, time.Duration, string, error) {
	var body []byte
	if r.Body != nil {
		limited, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			return nil, 0, "", err
		}
		body = limited
	}

	ttlDays := s.cfg.TokenTTLDays
	label := ""

	if len(body) > 0 {
		var req tokenRequestBody
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 0, "", errors.New("invalid JSON body: " + err.Error())
		}
		if req.TTLDays != nil {
			if *req.TTLDays < 0 {
				return nil, 0, "", errors.New("ttl_days must not be negative")
			}
			ttlDays = *req.TTLDays
		}
		if req.Label != "" {
			label = req.Label
		}
	}

	return body, tokenDuration(ttlDays), label, nil
}

// tokenDuration converts a day count into a time.Duration. A non-positive value
// yields 0, meaning the token never expires.
func tokenDuration(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func writeTokenResponse(w http.ResponseWriter, token string, info auth.TokenInfo) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		Token:     token,
		CreatedAt: info.CreatedAt,
		ExpiresAt: info.ExpiresAt,
		Label:     info.Label,
	})
}
