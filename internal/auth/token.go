package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"strconv"
)

type TokenStore struct {
	Tokens map[string]bool `json:"tokens"`
}

func LoadTokens(tokensFile string) (*TokenStore, error) {
	store := &TokenStore{Tokens: make(map[string]bool)}
	data, err := os.ReadFile(tokensFile)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, store); err != nil {
		return nil, err
	}
	return store, nil
}

func SaveTokens(tokensFile string, store *TokenStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	// Set permissions to 0640 so group members (orbitron) can read the file
	if err := os.WriteFile(tokensFile, data, 0640); err != nil {
		return err
	}

	// Ensure ownership is transferred to the orbitron system user/group when run via sudo
	if u, err := user.Lookup("orbitron"); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		_ = os.Chown(tokensFile, uid, gid)
	}

	return nil
}

func GenerateToken(tokensFile string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	store, err := LoadTokens(tokensFile)
	if err != nil {
		return "", err
	}

	store.Tokens[token] = true
	if err := SaveTokens(tokensFile, store); err != nil {
		return "", err
	}

	return token, nil
}

func RevokeToken(tokensFile string, token string) error {
	store, err := LoadTokens(tokensFile)
	if err != nil {
		return err
	}

	delete(store.Tokens, token)
	return SaveTokens(tokensFile, store)
}
