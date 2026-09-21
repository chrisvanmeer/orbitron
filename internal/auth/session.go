package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionManager tracks in-memory dashboard sessions established through OIDC
// login. Sessions are ephemeral by design: nothing is persisted to disk and
// every daemon restart clears them, forcing users to sign in again. A Session
// grants exactly the same access as a valid administrative token cookie.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

// NewSessionManager returns a manager that expires sessions after ttl.
func NewSessionManager(ttl time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	return &SessionManager{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
	}
}

// Create registers a new session and returns its opaque token. The token is
// only ever kept in memory and in the caller's cookie.
func (s *SessionManager) Create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	now := time.Now()
	s.sweep(now)

	s.mu.Lock()
	s.sessions[token] = now.Add(s.ttl)
	s.mu.Unlock()
	return token, nil
}

// Valid reports whether the token belongs to a live, non-expired session.
func (s *SessionManager) Valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Revoke removes a session token, logging the user out.
func (s *SessionManager) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// Count returns the number of live sessions (helper for tests and metrics).
func (s *SessionManager) Count() int {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for token, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, token)
			continue
		}
		count++
	}
	return count
}

// sweep drops expired sessions. Callers must hold no lock.
func (s *SessionManager) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, token)
		}
	}
}
