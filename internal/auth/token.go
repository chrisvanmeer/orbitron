package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"time"
)

const tokenByteLength = 16

// TokenInfo holds the lifecycle metadata for a single administrative token.
// ExpiresAt is 0 when the token never expires.
type TokenInfo struct {
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Label     string `json:"label,omitempty"`
}

// TokenStore is the on-disk registry of administrative tokens.
type TokenStore struct {
	Tokens map[string]TokenInfo `json:"tokens"`
}

// LoadTokens reads the tokens file, transparently migrating the legacy
// "map[string]int64" (created_at only) format into TokenInfo entries.
// A missing file yields an empty store.
func LoadTokens(tokensFile string) (*TokenStore, error) {
	store := &TokenStore{Tokens: make(map[string]TokenInfo)}
	data, err := os.ReadFile(tokensFile)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}

	var raw struct {
		Tokens map[string]json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	for token, rawInfo := range raw.Tokens {
		// Legacy format stores the created-at timestamp directly as a number.
		var legacy int64
		if err := json.Unmarshal(rawInfo, &legacy); err == nil {
			store.Tokens[token] = TokenInfo{CreatedAt: legacy}
			continue
		}

		var info TokenInfo
		if err := json.Unmarshal(rawInfo, &info); err != nil {
			return nil, fmt.Errorf("invalid token entry for %q: %w", token, err)
		}
		store.Tokens[token] = info
	}

	return store, nil
}

func SaveTokens(tokensFile string, store *TokenStore) error {
	if store == nil {
		store = &TokenStore{Tokens: make(map[string]TokenInfo)}
	}
	if store.Tokens == nil {
		store.Tokens = make(map[string]TokenInfo)
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(tokensFile, data, 0640); err != nil {
		return err
	}

	if u, err := user.Lookup("orbitron"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(tokensFile, uid, gid)
	}

	return nil
}

// GenerateToken appends a new random administrative token to the store and
// returns the raw token string together with its lifecycle metadata. A
// non-positive ttl yields a token that never expires.
func GenerateToken(tokensFile, label string, ttl time.Duration) (string, TokenInfo, error) {
	b := make([]byte, tokenByteLength)
	if _, err := rand.Read(b); err != nil {
		return "", TokenInfo{}, err
	}
	token := hex.EncodeToString(b)

	store, err := LoadTokens(tokensFile)
	if err != nil {
		return "", TokenInfo{}, err
	}

	now := time.Now().Unix()
	info := TokenInfo{CreatedAt: now, Label: label}
	if ttl > 0 {
		info.ExpiresAt = now + int64(ttl.Seconds())
	}

	store.Tokens[token] = info
	if err := SaveTokens(tokensFile, store); err != nil {
		return "", TokenInfo{}, err
	}

	return token, info, nil
}

// RotateToken replaces an existing token with a freshly generated one that
// preserves the remaining lifetime (and label) of the original. The old token
// is revoked in the same store write so a gap never allows both to coexist
// indefinitely. Returns the new token and its lifecycle metadata.
func RotateToken(tokensFile, oldToken, newLabel string) (string, TokenInfo, error) {
	store, err := LoadTokens(tokensFile)
	if err != nil {
		return "", TokenInfo{}, err
	}

	old, ok := store.Tokens[oldToken]
	if !ok {
		return "", TokenInfo{}, ErrTokenNotFound
	}

	if newLabel == "" {
		newLabel = old.Label
	}

	var ttl time.Duration
	if old.ExpiresAt > 0 {
		remaining := old.ExpiresAt - time.Now().Unix()
		if remaining > 0 {
			ttl = time.Duration(remaining) * time.Second
		}
	}

	b := make([]byte, tokenByteLength)
	if _, err := rand.Read(b); err != nil {
		return "", TokenInfo{}, err
	}
	newToken := hex.EncodeToString(b)

	now := time.Now().Unix()
	info := TokenInfo{CreatedAt: now, Label: newLabel}
	if ttl > 0 {
		info.ExpiresAt = now + int64(ttl.Seconds())
	}

	store.Tokens[newToken] = info
	delete(store.Tokens, oldToken)

	if err := SaveTokens(tokensFile, store); err != nil {
		return "", TokenInfo{}, err
	}

	return newToken, info, nil
}

// RevokeToken removes a token from the store.
func RevokeToken(tokensFile string, token string) error {
	store, err := LoadTokens(tokensFile)
	if err != nil {
		return err
	}

	if _, ok := store.Tokens[token]; !ok {
		return ErrTokenNotFound
	}

	delete(store.Tokens, token)
	return SaveTokens(tokensFile, store)
}

// PruneExpiredTokens removes every token whose expiry has passed and persists
// the store only when at least one token was removed. Returns the number of
// removed tokens.
func PruneExpiredTokens(tokensFile string) (int, error) {
	store, err := LoadTokens(tokensFile)
	if err != nil {
		return 0, err
	}

	pruned := store.PruneExpired()
	if pruned == 0 {
		return 0, nil
	}
	if err := SaveTokens(tokensFile, store); err != nil {
		return 0, err
	}
	return pruned, nil
}

// Valid reports whether token exists and has not expired yet.
func (s *TokenStore) Valid(token string) bool {
	info, ok := s.Tokens[token]
	if !ok {
		return false
	}
	if info.ExpiresAt > 0 && info.ExpiresAt <= time.Now().Unix() {
		return false
	}
	return true
}

// ActiveCount returns the number of tokens that have not expired.
func (s *TokenStore) ActiveCount() int {
	now := time.Now().Unix()
	count := 0
	for _, info := range s.Tokens {
		if info.ExpiresAt == 0 || info.ExpiresAt > now {
			count++
		}
	}
	return count
}

// PruneExpired removes expired tokens in memory and returns how many were removed.
func (s *TokenStore) PruneExpired() int {
	now := time.Now().Unix()
	removed := 0
	for token, info := range s.Tokens {
		if info.ExpiresAt > 0 && info.ExpiresAt <= now {
			delete(s.Tokens, token)
			removed++
		}
	}
	return removed
}

// ErrTokenNotFound is returned when an operation targets a token that is not
// present in the store.
var ErrTokenNotFound = errors.New("token not found")
