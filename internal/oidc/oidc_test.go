package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbitron/internal/config"
)

// fakeIDP is a minimal Keycloak-shaped OIDC server for tests: it serves a
// discovery document, a JWKS and a token endpoint that mints signed ID tokens.
type fakeIDP struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	clientID string
	code     string
	nonce    string
	groups   []string
}

func newFakeIDP(t *testing.T, clientID string) *fakeIDP {
	return newFakeIDPServer(t, clientID, false)
}

// newFakeIDPServer builds the fake IDP on either a plain HTTP server or a
// self-signed TLS server (tls true), used to exercise the outbound TLS policy.
func newFakeIDPServer(t *testing.T, clientID string, tls bool) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{key: key, kid: "testkey", clientID: clientID}
	f.code = "goodcode"

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := map[string]any{
			"issuer":                 f.issuer,
			"authorization_endpoint": f.issuer + "/auth",
			"token_endpoint":         f.issuer + "/token",
			"jwks_uri":               f.issuer + "/jwks",
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{f.publicJWK()}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != f.code {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		tok := f.signIDToken(f.clientID, f.nonce)
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": tok})
	})

	if tls {
		f.server = httptest.NewTLSServer(mux)
	} else {
		f.server = httptest.NewServer(mux)
	}
	f.issuer = f.server.URL
	return f
}

func (f *fakeIDP) close() { f.server.Close() }

func (f *fakeIDP) publicJWK() map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": f.kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

func (f *fakeIDP) signJWT(claims map[string]any) string {
	hdr, _ := json.Marshal(jwtHeader{Alg: "RS256", Kid: f.kid, Typ: "JWT"})
	payload, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signingInput := enc(hdr) + "." + enc(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + enc(sig)
}

func (f *fakeIDP) signIDToken(clientID, nonce string) string {
	now := time.Now().Unix()
	claims := map[string]any{
		"iss":   f.issuer,
		"sub":   "user-1234",
		"aud":   clientID,
		"exp":   now + 300,
		"iat":   now,
		"email": "operator@example.org",
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if len(f.groups) > 0 {
		claims["groups"] = f.groups
	}
	return f.signJWT(claims)
}

func newTestClient(t *testing.T, idp *fakeIDP) *Client {
	t.Helper()
	c, err := NewClient(config.OIDCConfig{
		Enabled:      true,
		Issuer:       idp.issuer,
		ClientID:     idp.clientID,
		ClientSecret: "s3cret",
	}, config.TLSConfig{})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return c
}

func TestNewClientRejectsMissingIssuer(t *testing.T) {
	if _, err := NewClient(config.OIDCConfig{Enabled: true}, config.TLSConfig{}); err == nil {
		t.Fatal("expected error for empty issuer")
	}
}

func TestNewClientRejectsMalformedIssuer(t *testing.T) {
	if _, err := NewClient(config.OIDCConfig{Enabled: true, Issuer: "not a url"}, config.TLSConfig{}); err == nil {
		t.Fatal("expected error for malformed issuer")
	}
}

func TestNewClientRejectsUnreachableIssuer(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	unreachable := strings.ReplaceAll(idp.issuer, "://", "://unreachable.")
	_, err := NewClient(config.OIDCConfig{Enabled: true, Issuer: unreachable, ClientID: "orbitron"}, config.TLSConfig{})
	if err == nil {
		t.Fatal("expected error for unreachable issuer")
	}
}

func TestNewClientHonorsTLSPolicy(t *testing.T) {
	idp := newFakeIDPServer(t, "orbitron", true)
	defer idp.close()

	// Without a TLS policy the self-signed IDP must be rejected.
	if _, err := NewClient(config.OIDCConfig{Enabled: true, Issuer: idp.issuer, ClientID: "orbitron"}, config.TLSConfig{}); err == nil {
		t.Fatal("expected TLS error for self-signed IDP without configured CA")
	}

	// Pinning the IDP certificate as a CA must make discovery succeed.
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.server.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(config.OIDCConfig{Enabled: true, Issuer: idp.issuer, ClientID: "orbitron"}, config.TLSConfig{CAFile: caFile})
	if err != nil {
		t.Fatalf("discovery against TLS IDP with pinned CA failed: %v", err)
	}
	if got := c.Issuer(); got != idp.issuer {
		t.Fatalf("issuer mismatch: got %q want %q", got, idp.issuer)
	}
}

func TestVerifyIDTokenHappyPath(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, err := NewAuthParams("https://orbitron.local/ui/oidc/callback")
	if err != nil {
		t.Fatal(err)
	}
	tok := idp.signIDToken(idp.clientID, params.Nonce)
	claims, err := c.VerifyIDToken(tok, params.Nonce)
	if err != nil {
		t.Fatalf("VerifyIDToken failed: %v", err)
	}
	if claims.Subject != "user-1234" {
		t.Fatalf("subject = %q, want user-1234", claims.Subject)
	}
	if claims.Email != "operator@example.org" {
		t.Fatalf("email = %q", claims.Email)
	}
}

func TestVerifyIDTokenParsesGroupsClaim(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	idp.groups = []string{"orbitron-admins", "ops"}
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	tok := idp.signIDToken(idp.clientID, params.Nonce)
	claims, err := c.VerifyIDToken(tok, params.Nonce)
	if err != nil {
		t.Fatalf("VerifyIDToken failed: %v", err)
	}
	if len(claims.Groups) != 2 || claims.Groups[0] != "orbitron-admins" || claims.Groups[1] != "ops" {
		t.Fatalf("groups = %q, want [orbitron-admins ops]", claims.Groups)
	}
}

func TestVerifyIDTokenRejectsWrongIssuer(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	now := time.Now().Unix()
	tok := idp.signJWT(map[string]any{
		"iss": "https://evil.example.org", "sub": "user-1",
		"aud": []string{idp.clientID}, "exp": now + 300, "iat": now, "nonce": params.Nonce,
	})
	if _, err := c.VerifyIDToken(tok, params.Nonce); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestVerifyIDTokenRejectsWrongAudience(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	now := time.Now().Unix()
	tok := idp.signJWT(map[string]any{
		"iss": idp.issuer, "sub": "user-1",
		"aud": "some-other-client", "exp": now + 300, "iat": now, "nonce": params.Nonce,
	})
	if _, err := c.VerifyIDToken(tok, params.Nonce); err == nil {
		t.Fatal("expected audience error")
	}
}

func TestVerifyIDTokenAcceptsArrayAudience(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	now := time.Now().Unix()
	// Some providers emit the "aud" claim as an array once a token has more
	// than one audience; the client's id must be among the entries.
	tok := idp.signJWT(map[string]any{
		"iss": idp.issuer, "sub": "user-1",
		"aud": []string{idp.clientID, "another-client"}, "exp": now + 300, "iat": now,
		"nonce": params.Nonce,
	})
	if _, err := c.VerifyIDToken(tok, params.Nonce); err != nil {
		t.Fatalf("array-form audience should verify: %v", err)
	}
}

func TestVerifyIDTokenRejectsExpired(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	now := time.Now().Unix()
	tok := idp.signJWT(map[string]any{
		"iss": idp.issuer, "sub": "user-1",
		"aud": []string{idp.clientID}, "exp": now - 10, "iat": now - 60, "nonce": params.Nonce,
	})
	if _, err := c.VerifyIDToken(tok, params.Nonce); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestVerifyIDTokenRejectsWrongNonce(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	_ = params
	tok := idp.signIDToken(idp.clientID, "some-other-nonce")
	if _, err := c.VerifyIDToken(tok, "expected-nonce"); err == nil {
		t.Fatal("expected nonce mismatch error")
	}
}

func TestVerifyIDTokenRejectsTamperedSignature(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	tok := idp.signIDToken(idp.clientID, "")
	parts := strings.Split(tok, ".")
	// Flip a character in the signature.
	sig := []byte(parts[2])
	sig[0] ^= 0x01
	parts[2] = string(sig)
	if _, err := c.VerifyIDToken(strings.Join(parts, "."), ""); err == nil {
		t.Fatal("expected signature failure")
	}
}

func TestAuthURLContainsPKCEAndParams(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("https://orbitron.local/ui/oidc/callback")
	u, err := c.AuthURL("https://orbitron.local/ui/oidc/callback", params.State, params.Nonce, params.CodeVerifier)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type=%q", q.Get("response_type"))
	}
	if q.Get("client_id") != "orbitron" {
		t.Fatalf("client_id=%q", q.Get("client_id"))
	}
	if q.Get("state") != params.State || q.Get("nonce") != params.Nonce {
		t.Fatal("state/nonce not propagated")
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatal("PKCE challenge missing")
	}
	if q.Get("redirect_uri") != "https://orbitron.local/ui/oidc/callback" {
		t.Fatalf("redirect_uri=%q", q.Get("redirect_uri"))
	}
	if parsed.Host != idp.issuer[len("http://"):] {
		t.Fatalf("auth URL host = %q, want the issuer's authorization endpoint", parsed.Host)
	}
}

func TestExchangeCodeHappyPath(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	idp.nonce = params.Nonce
	claims, err := c.ExchangeCode(idp.code, params)
	if err != nil {
		t.Fatalf("ExchangeCode failed: %v", err)
	}
	if claims.Subject != "user-1234" {
		t.Fatalf("subject=%q", claims.Subject)
	}
}

func TestExchangeCodeRejectsBadCode(t *testing.T) {
	idp := newFakeIDP(t, "orbitron")
	defer idp.close()
	c := newTestClient(t, idp)

	params, _ := NewAuthParams("http://x/cb")
	if _, err := c.ExchangeCode("bogus", params); err == nil {
		t.Fatal("expected exchange error for bad code")
	}
}

func TestPkceChallengeStable(t *testing.T) {
	verifier := "abc123"
	a := pkceChallenge(verifier)
	b := pkceChallenge(verifier)
	if a != b {
		t.Fatalf("pkceChallenge is unstable: %q vs %q", a, b)
	}
	if pkceChallenge("") == "" {
		t.Fatal("empty challenge for empty verifier")
	}
}
