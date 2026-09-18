package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStoreFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, content, 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTokensLegacyFormatMigration(t *testing.T) {
	ts := time.Now().Unix()
	path := writeStoreFile(t, []byte(`{"tokens":{"legacy123":`+jsonNumber(ts)+`}}`))

	store, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}

	info, ok := store.Tokens["legacy123"]
	if !ok {
		t.Fatal("expected legacy token to be migrated")
	}
	if info.CreatedAt != ts {
		t.Errorf("created_at = %d, want %d", info.CreatedAt, ts)
	}
	if info.ExpiresAt != 0 {
		t.Errorf("legacy token should never expire, got expires_at=%d", info.ExpiresAt)
	}
	if !store.Valid("legacy123") {
		t.Error("legacy token should be valid")
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestLoadTokensNewFormat(t *testing.T) {
	now := time.Now().Unix()
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))
	store := &TokenStore{Tokens: map[string]TokenInfo{
		"tokenA": {CreatedAt: now, ExpiresAt: now + 3600, Label: "ci"},
	}}
	if err := SaveTokens(path, store); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}

	loaded, err := LoadTokens(path)
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	if len(loaded.Tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(loaded.Tokens))
	}
	info := loaded.Tokens["tokenA"]
	if info.Label != "ci" || info.ExpiresAt != now+3600 || info.CreatedAt != now {
		t.Errorf("unexpected token info: %+v", info)
	}
	if !loaded.Valid("tokenA") {
		t.Error("tokenA should be valid")
	}
}

func TestValidHonoursExpiry(t *testing.T) {
	now := time.Now().Unix()

	store := &TokenStore{Tokens: map[string]TokenInfo{
		"never":   {CreatedAt: now},
		"future":  {CreatedAt: now, ExpiresAt: now + 3600},
		"expired": {CreatedAt: now - 7200, ExpiresAt: now - 3600},
	}}

	cases := []struct {
		token string
		want  bool
	}{
		{"never", true},
		{"future", true},
		{"expired", false},
		{"unknown", false},
	}
	for _, c := range cases {
		if got := store.Valid(c.token); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}

func TestActiveCountIgnoresExpired(t *testing.T) {
	now := time.Now().Unix()
	store := &TokenStore{Tokens: map[string]TokenInfo{
		"a": {CreatedAt: now},
		"b": {CreatedAt: now, ExpiresAt: now + 1},
		"c": {CreatedAt: now - 1, ExpiresAt: now - 10},
	}}
	if got := store.ActiveCount(); got != 2 {
		t.Errorf("ActiveCount = %d, want 2", got)
	}
}

func TestPruneExpired(t *testing.T) {
	now := time.Now().Unix()
	store := &TokenStore{Tokens: map[string]TokenInfo{
		"keep": {CreatedAt: now},
		"drop": {CreatedAt: now - 2, ExpiresAt: now - 1},
	}}

	if removed := store.PruneExpired(); removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, ok := store.Tokens["drop"]; ok {
		t.Error("expired token should have been pruned")
	}
	if store.Valid("keep") == false {
		t.Error("unexpired token should remain valid")
	}
}

func TestGenerateTokenAppliesTTL(t *testing.T) {
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))

	token, info, err := GenerateToken(path, "release", 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if info.Label != "release" {
		t.Errorf("label = %q, want release", info.Label)
	}
	if info.ExpiresAt <= info.CreatedAt {
		t.Errorf("expected future expiry, got created=%d expires=%d", info.CreatedAt, info.ExpiresAt)
	}
}

func TestGenerateTokenNoTTLNeverExpires(t *testing.T) {
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))

	_, info, err := GenerateToken(path, "", 0)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if info.ExpiresAt != 0 {
		t.Errorf("expected no expiry, got %d", info.ExpiresAt)
	}
}

func TestRotateTokenPreservesRemainingLife(t *testing.T) {
	now := time.Now().Unix()
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))

	store := &TokenStore{Tokens: map[string]TokenInfo{
		"oldToken": {CreatedAt: now - 60, ExpiresAt: now - 60 + 7200, Label: "ci"},
	}}
	if err := SaveTokens(path, store); err != nil {
		t.Fatal(err)
	}

	newToken, info, err := RotateToken(path, "oldToken", "")
	if err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if newToken == "" || newToken == "oldToken" {
		t.Errorf("expected a fresh token, got %q", newToken)
	}
	if info.Label != "ci" {
		t.Errorf("label = %q, want ci", info.Label)
	}

	reloaded, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Tokens["oldToken"]; ok {
		t.Error("old token should be revoked after rotation")
	}
	if !reloaded.Valid(newToken) {
		t.Error("rotated token should be valid")
	}
}

func TestRotateTokenMissingReturnsErr(t *testing.T) {
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))
	if _, _, err := RotateToken(path, "nope", ""); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound, got %v", err)
	}
}

func TestRevokeToken(t *testing.T) {
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))

	token, _, err := GenerateToken(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeToken(path, token); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := RevokeToken(path, token); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("second revoke should fail with ErrTokenNotFound, got %v", err)
	}

	reloaded, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Valid(token) {
		t.Error("revoked token should be invalid")
	}
}

func TestPruneExpiredTokensPersists(t *testing.T) {
	now := time.Now().Unix()
	path := writeStoreFile(t, []byte(`{"tokens":{}}`))

	store := &TokenStore{Tokens: map[string]TokenInfo{
		"keep": {CreatedAt: now},
		"drop": {CreatedAt: now - 10, ExpiresAt: now - 5},
	}}
	if err := SaveTokens(path, store); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneExpiredTokens(path)
	if err != nil {
		t.Fatalf("PruneExpiredTokens: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	reloaded, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Tokens) != 1 {
		t.Errorf("expected 1 token persisted, got %d", len(reloaded.Tokens))
	}
}

func TestLoadTokensMissingFileReturnsEmpty(t *testing.T) {
	store, err := LoadTokens(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	if len(store.Tokens) != 0 {
		t.Errorf("expected empty store, got %d entries", len(store.Tokens))
	}
}
