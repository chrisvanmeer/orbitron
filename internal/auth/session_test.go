package auth

import (
	"testing"
	"time"
)

func TestSessionManagerCreateAndValidate(t *testing.T) {
	m := NewSessionManager(time.Hour)
	tok, err := m.Create()
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty session token")
	}
	if !m.Valid(tok) {
		t.Fatal("fresh session reported invalid")
	}
	if m.Valid("bogus") {
		t.Fatal("bogus token reported valid")
	}
	if m.Valid("") {
		t.Fatal("empty token reported valid")
	}
}

func TestSessionManagerRevoke(t *testing.T) {
	m := NewSessionManager(time.Hour)
	tok, _ := m.Create()
	m.Revoke(tok)
	if m.Valid(tok) {
		t.Fatal("revoked session reported valid")
	}
	if got := m.Count(); got != 0 {
		t.Fatalf("count = %d after revoke, want 0", got)
	}
}

func TestSessionManagerExpiry(t *testing.T) {
	m := NewSessionManager(50 * time.Millisecond)
	tok, _ := m.Create()
	if !m.Valid(tok) {
		t.Fatal("fresh session not valid")
	}
	time.Sleep(120 * time.Millisecond)
	if m.Valid(tok) {
		t.Fatal("expired session still valid")
	}
	if got := m.Count(); got != 0 {
		t.Fatalf("count = %d after expiry, want 0", got)
	}
}

func TestSessionManagerScopedToInstance(t *testing.T) {
	m := NewSessionManager(time.Hour)
	tok, _ := m.Create()
	other := NewSessionManager(time.Hour)
	if other.Valid(tok) {
		t.Fatal("session leaked across managers")
	}
}

func TestSessionManagerZeroTTLDefaults(t *testing.T) {
	m := NewSessionManager(0)
	tok, _ := m.Create()
	if !m.Valid(tok) {
		t.Fatal("zero-TTL session should default and be valid")
	}
}
