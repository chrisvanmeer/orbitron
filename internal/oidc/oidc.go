// Package oidc implements a minimal, zero-dependency OpenID Connect relying
// party (authorization-code flow with PKCE and confidential client secret)
// used to authenticate the Orbitron web dashboard against an external IDP
// such as Keycloak. Only the ID token verification surface needed by the
// dashboard is implemented; everything is stdlib.
package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orbitron/internal/config"
	"orbitron/internal/logger"
)

// Audience is the ID token "aud" claim. OIDC providers vary in how they
// encode it: Keycloak emits a single JSON string while the token has exactly
// one audience, and an array once there are several. The custom unmarshaler
// accepts both forms.
type Audience []string

// UnmarshalJSON accepts both a single JSON string and an array of strings.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*a = Audience{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(b, &multi); err != nil {
		return err
	}
	*a = multi
	return nil
}

// Claims are the ID token fields Orbitron validates and surfaces in logs.
type Claims struct {
	Issuer            string   `json:"iss"`
	Subject           string   `json:"sub"`
	Audience          Audience `json:"aud"`
	AudienceLegacy    string   `json:"azp"`
	ExpiresAt         int64    `json:"exp"`
	NotBefore         int64    `json:"nbf"`
	IssuedAt          int64    `json:"iat"`
	Nonce             string   `json:"nonce"`
	Email             string   `json:"email"`
	EmailVerified     *bool    `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	// Groups holds the ID token "groups" claim (Keycloak requires a protocol
	// mapper / "groups" client scope for this claim to be present). It backs
	// the optional allowed_groups access filter.
	Groups []string `json:"groups"`
}

// AuthParams captures the per-login state that must survive the round-trip to
// the IDP and back: the CSRF "state" value, the PKCE code verifier and the
// "nonce" bound to the ID token.
type AuthParams struct {
	State        string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
}

// Client is a single OIDC relying party. It caches the discovery document and
// the IDP JSON Web Key Set, refetching keys on unknown key IDs so rotated
// signing keys keep working. It is safe for concurrent use.
type Client struct {
	issuer   string
	clientID string
	secret   string
	client   *http.Client

	mu        sync.RWMutex
	disc      *DiscoveryDoc
	jwks      map[string]crypto.PublicKey
	jwksKIDs  []string
	lastKeys  time.Time
	lastFetch time.Time
}

// DiscoveryDoc holds the endpoints advertised at the issuer's well-known
// OpenID configuration endpoint.
type DiscoveryDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// NewClient validates the OIDC configuration, performs discovery and fetches
// the initial JWKS. A client with an unreachable issuer fails fast so a
// misconfigured daemon never silently starts without SSO.
func NewClient(cfg config.OIDCConfig, tls config.TLSConfig) (*Client, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is required when OIDC is enabled")
	}
	issuer := strings.TrimSuffix(cfg.Issuer, "/")
	if _, err := url.ParseRequestURI(issuer); err != nil {
		return nil, fmt.Errorf("oidc: invalid issuer URL %q: %w", cfg.Issuer, err)
	}

	tlsClientConfig, err := tls.TLSClientConfig()
	if err != nil {
		return nil, fmt.Errorf("oidc: invalid TLS policy: %w", err)
	}
	transport := http.DefaultTransport
	if tlsClientConfig != nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = tlsClientConfig
		transport = t
	}

	c := &Client{
		issuer:   issuer,
		clientID: cfg.ClientID,
		secret:   cfg.ClientSecret,
		client: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
	}

	if _, err := c.discover(); err != nil {
		return nil, err
	}
	if err := c.fetchKeys(); err != nil {
		return nil, err
	}
	return c, nil
}

// Issuer returns the configured issuer URL.
func (c *Client) Issuer() string { return c.issuer }

// ClientID returns the configured client identifier.
func (c *Client) ClientID() string { return c.clientID }

// httpStatusError marks an unexpected HTTP status from the IDP so callers can
// offer targeted hints (e.g. a Keycloak issuer that is missing /realms/).
type httpStatusError struct {
	status int
	text   string
}

func (e *httpStatusError) Error() string { return e.text }

// discover fetches and caches the discovery document.
func (c *Client) discover() (*DiscoveryDoc, error) {
	c.mu.RLock()
	doc := c.disc
	age := time.Since(c.lastFetch)
	c.mu.RUnlock()
	if doc != nil && age < time.Hour {
		return doc, nil
	}

	wellKnown := c.issuer + "/.well-known/openid-configuration"
	body, err := c.getJSON(wellKnown)
	if err != nil {
		hint := ""
		var se *httpStatusError
		if errors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusForbidden) {
			hint = " (for Keycloak the issuer must be the full realm URL including the /realms/<realm> path segment, e.g. https://keycloak.example.org/realms/orbitron)"
		}
		return nil, fmt.Errorf("oidc: discovery at %s failed: %w%s", wellKnown, err, hint)
	}

	var d DiscoveryDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("oidc: malformed discovery document: %w", err)
	}
	if d.Issuer != c.issuer {
		return nil, fmt.Errorf("oidc: discovery issuer mismatch: got %q want %q", d.Issuer, c.issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, errors.New("oidc: discovery document missing authorization/token/JWKS endpoints")
	}

	c.mu.Lock()
	c.disc = &d
	c.lastFetch = time.Now()
	c.mu.Unlock()
	return &d, nil
}

// AuthURL builds the authorization URL for the login hand-off. The returned
// AuthParams must be kept by the caller and passed to ExchangeCode once the
// IDP redirects back.
func (c *Client) AuthURL(redirectURI, state, nonce, codeVerifier string) (string, error) {
	doc, err := c.discover()
	if err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge(codeVerifier))
	q.Set("code_challenge_method", "S256")
	return doc.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// ExchangeCode trades the authorization code for tokens, validates the
// returned ID token and returns its claims on success. The nonce returned by
// the IDP is checked against the one stored at AuthURL time.
func (c *Client) ExchangeCode(code string, params *AuthParams) (*Claims, error) {
	doc, err := c.discover()
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", params.RedirectURI)
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.secret)
	form.Set("code_verifier", params.CodeVerifier)

	req, err := http.NewRequest(http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: token exchange failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oidc: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e map[string]any
		_ = json.Unmarshal(body, &e)
		desc, _ := e["error_description"].(string)
		return nil, fmt.Errorf("oidc: token endpoint returned %s: %s", resp.Status, desc)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("oidc: malformed token response: %w", err)
	}
	if tok.IDToken == "" {
		return nil, errors.New("oidc: token response contained no id_token")
	}

	return c.VerifyIDToken(tok.IDToken, params.Nonce)
}

// VerifyIDToken validates a JWT ID token's signature against the issuer JWKS
// and its core claims (issuer, audience, expiry, nonce). It returns the parsed
// claims on success.
func (c *Client) VerifyIDToken(idToken, nonce string) (*Claims, error) {
	payload, err := c.validateJWS(idToken)
	if err != nil {
		return nil, err
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("oidc: malformed ID token payload: %w", err)
	}

	if claims.Issuer != c.issuer {
		return nil, fmt.Errorf("oidc: ID token issuer mismatch: got %q want %q", claims.Issuer, c.issuer)
	}
	if !audienceIncludes(&claims, c.clientID) {
		return nil, fmt.Errorf("oidc: ID token audience does not include client %q", c.clientID)
	}

	now := time.Now().Unix()
	if claims.ExpiresAt > 0 && now >= claims.ExpiresAt {
		return nil, errors.New("oidc: ID token expired")
	}
	if claims.NotBefore > 0 && now < claims.NotBefore {
		return nil, errors.New("oidc: ID token not yet valid")
	}
	if claims.IssuedAt > 0 && claims.IssuedAt > now+300 {
		return nil, errors.New("oidc: ID token issued in the future")
	}
	if nonce != "" && claims.Nonce != nonce {
		return nil, errors.New("oidc: ID token nonce mismatch")
	}

	return &claims, nil
}

func audienceIncludes(claims *Claims, clientID string) bool {
	for _, aud := range claims.Audience {
		if aud == clientID {
			return true
		}
	}
	return claims.AudienceLegacy == clientID
}

// validateJWS verifies the detached signature of a compact JWS and returns the
// raw payload segment. Only RS256 and ES256 are accepted.
func (c *Client) validateJWS(tok string) ([]byte, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: ID token is not a compact JWS")
	}

	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed JWS header: %w", err)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hdr, &head); err != nil {
		return nil, fmt.Errorf("oidc: malformed JWS header: %w", err)
	}
	if head.Typ != "" && !strings.EqualFold(head.Typ, "JWT") {
		return nil, fmt.Errorf("oidc: unexpected JWS type %q", head.Typ)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed JWS payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed JWS signature: %w", err)
	}

	key, err := c.lookupKey(head.Kid)
	if err != nil {
		return nil, err
	}

	signingInput := parts[0] + "." + parts[1]
	switch head.Alg {
	case "RS256":
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("oidc: JWKS key is not an RSA key but alg is RS256")
		}
		digest := sha256.Sum256([]byte(signingInput))
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, digest[:], sig); err != nil {
			return nil, errors.New("oidc: ID token signature verification failed")
		}
	case "ES256":
		ecKey, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return nil, errors.New("oidc: JWKS key is not an EC key but alg is ES256")
		}
		digest := sha256.Sum256([]byte(signingInput))
		if !ecdsa.Verify(ecKey, digest[:], parseES256R(sig, ecKey.Params().BitSize), parseES256S(sig, ecKey.Params().BitSize)) {
			return nil, errors.New("oidc: ID token signature verification failed")
		}
	default:
		return nil, fmt.Errorf("oidc: unsupported JWS algorithm %q", head.Alg)
	}

	return payload, nil
}

// lookupKey returns the signing key for a given key ID, refreshing the JWKS
// from the IDP when the key is unknown or the cached set is aged.
func (c *Client) lookupKey(kid string) (crypto.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.jwks[kid]
	neverSeen := !contains(c.jwksKIDs, kid)
	keysAged := time.Since(c.lastKeys) > 24*time.Hour
	c.mu.RUnlock()

	if !ok && (neverSeen || keysAged) {
		if err := c.fetchKeys(); err != nil {
			return nil, err
		}
		c.mu.RLock()
		key, ok = c.jwks[kid]
		c.mu.RUnlock()
	}

	if !ok || key == nil {
		return nil, fmt.Errorf("oidc: no signing key with kid %q in issuer JWKS", kid)
	}
	return key, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// fetchKeys downloads and parses the issuer JWKS, replacing the cached set.
func (c *Client) fetchKeys() error {
	doc, err := c.discover()
	if err != nil {
		return err
	}

	body, err := c.getJSON(doc.JWKSURI)
	if err != nil {
		return fmt.Errorf("oidc: fetching JWKS at %s failed: %w", doc.JWKSURI, err)
	}

	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("oidc: malformed JWKS: %w", err)
	}

	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	kids := []string{}
	for _, j := range set.Keys {
		k, err := j.publicKey()
		if err != nil {
			logger.Warn("oidc: skipping unusable JWKS key kid=%q: %v", j.Kid, err)
			continue
		}
		if j.Kid != "" {
			keys[j.Kid] = k
			kids = append(kids, j.Kid)
		}
	}
	if len(keys) == 0 {
		return errors.New("oidc: issuer JWKS contained no usable keys")
	}

	c.mu.Lock()
	c.jwks = keys
	c.jwksKIDs = kids
	c.lastKeys = time.Now()
	c.mu.Unlock()
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j jwk) publicKey() (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		nb, err := base64.RawURLEncoding.DecodeString(j.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
	case "EC":
		if j.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported curve %q", j.Crv)
		}
		xb, err := base64.RawURLEncoding.DecodeString(j.X)
		if err != nil {
			return nil, err
		}
		yb, err := base64.RawURLEncoding.DecodeString(j.Y)
		if err != nil {
			return nil, err
		}
		// SEC 1 uncompressed point: 0x04 || X || Y
		point := append([]byte{0x04}, xb...)
		point = append(point, yb...)
		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	default:
		return nil, fmt.Errorf("unsupported key type %q", j.Kty)
	}
}

// NewAuthParams generates fresh random state, nonce and PKCE verifier values.
func NewAuthParams(redirectURI string) (*AuthParams, error) {
	state, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	nonce, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	verifier, err := randomHex(48)
	if err != nil {
		return nil, err
	}
	return &AuthParams{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// parseES256R/S split the 2*ceil(bits/8) byte ES256 signature into its two
// big-endian integer components.
func parseES256R(sig []byte, bitSize int) *big.Int {
	n := (bitSize + 7) / 8
	if len(sig) < 2*n {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(sig[:n])
}

func parseES256S(sig []byte, bitSize int) *big.Int {
	n := (bitSize + 7) / 8
	if len(sig) < 2*n {
		return new(big.Int)
	}
	return new(big.Int).SetBytes(sig[n : 2*n])
}

func (c *Client) getJSON(u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{
			status: resp.StatusCode,
			text:   fmt.Sprintf("unexpected status %s", resp.Status),
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mt, _, _ := mime.ParseMediaType(ct); mt != "application/json" {
			return nil, fmt.Errorf("unexpected content type %q", ct)
		}
	}
	return body, nil
}
