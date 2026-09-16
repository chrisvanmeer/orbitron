package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
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
	return os.WriteFile(tokensFile, data, 0600)
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
