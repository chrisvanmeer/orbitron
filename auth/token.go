package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"sync"
)

const TokensFilePath = "/etc/orbitron/tokens.json"

type TokenStore struct {
	Tokens map[string]bool `json:"tokens"`
	mu     sync.RWMutex
}

func GenerateRandomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func LoadTokens(path string) (*TokenStore, error) {
	store := &TokenStore{Tokens: make(map[string]bool)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	} else if err != nil {
		return nil, err
	}

	err = json.Unmarshal(data, &store.Tokens)
	return store, err
}

func (ts *TokenStore) Save(path string) error {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	data, err := json.MarshalIndent(ts.Tokens, "", "  ")
	if err != nil {
		return err
	}

	// Write file readable by owner and group
	if err := os.WriteFile(path, data, 0640); err != nil {
		return err
	}

	// Chown to orbitron:orbitron if the system user exists
	if u, err := user.Lookup("orbitron"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(path, uid, gid)
	}

	return nil
}

func IssueToken(path string) (string, error) {
	store, err := LoadTokens(path)
	if err != nil {
		return "", err
	}

	token, err := GenerateRandomToken()
	if err != nil {
		return "", err
	}

	store.Tokens[token] = true
	if err := store.Save(path); err != nil {
		return "", err
	}

	return token, nil
}

func RevokeToken(path string, token string) error {
	store, err := LoadTokens(path)
	if err != nil {
		return err
	}

	if _, exists := store.Tokens[token]; !exists {
		return fmt.Errorf("token does not exist")
	}

	delete(store.Tokens, token)
	return store.Save(path)
}
