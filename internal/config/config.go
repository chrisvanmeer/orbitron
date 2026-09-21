package config

import (
	"os"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// OIDCConfig holds the OpenID Connect (SSO, e.g. Keycloak) settings used to
// authenticate the web dashboard. When Enabled the login page additionally
// offers a "Sign in with SSO" button that runs the OIDC authorization-code
// flow against the configured issuer. It is purely an authentication gate: a
// successfully authenticated session opens the dashboard exactly like an
// administrative token would.
type OIDCConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Issuer is the full OpenID connect issuer URL, e.g.
	// https://keycloak.example.org/realms/orbitron. Orbitron auto-discovers the
	// authorization/token/JWKS endpoints from its well-known configuration.
	Issuer string `yaml:"issuer" json:"issuer"`
	// ClientID is the Keycloak/OIDC client identifier (client name).
	ClientID string `yaml:"client_id" json:"client_id"`
	// ClientSecret is the confidential client secret issued by the IDP.
	ClientSecret string `yaml:"client_secret" json:"-"`
	// SessionTTLHours is how long an authenticated dashboard session stays
	// valid before the user has to log in again. Sessions live in memory only
	// and are lost on daemon restart. 0 falls back to 8 hours.
	SessionTTLHours int `yaml:"session_ttl_hours" json:"session_ttl_hours"`
	// RedirectURI is the callback URL the IDP is allowed to redirect back to.
	// Leave empty to auto-derive it from the incoming request host +
	// X-Forwarded-Proto (e.g. https://orbitron.example.org/ui/oidc/callback),
	// which must match a registered redirect URI in the IDP client. It cannot
	// be relative: only absolute http(s) URLs are accepted.
	RedirectURI string `yaml:"redirect_uri" json:"redirect_uri"`
	// AllowedGroups is an optional access filter: when non-empty, an SSO user
	// must be a member of at least one named group (from the ID token "groups"
	// claim) to gain dashboard access. Users without a matching group are
	// denied. Empty disables the filter entirely.
	AllowedGroups []string `yaml:"allowed_groups" json:"allowed_groups"`
}

func (o *OIDCConfig) EnabledAndConfigured() bool {
	return o != nil && o.Enabled && o.Issuer != "" && o.ClientID != ""
}

// HasAllowedGroups reports whether the optional SSO group filter is active.
func (o *OIDCConfig) HasAllowedGroups() bool {
	return o != nil && len(o.AllowedGroups) > 0
}

// SessionTTL renders the configured session lifetime, defaulting to 8h when 0.
func (o *OIDCConfig) SessionTTL() (ttl time.Duration) {
	ttl = time.Duration(o.SessionTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	return ttl
}

type Config struct {
	ListenAddr      string `yaml:"listen_addr"`
	StoragePath     string `yaml:"storage_path"`
	LogPath         string `yaml:"log_path"`
	TokensFile      string `yaml:"tokens_file"`
	RequireAuthPull bool   `yaml:"require_auth_pull"`
	MaxConcurrency  int    `yaml:"max_concurrency"`
	// TokenTTLDays is the default lifetime for newly generated administrative
	// tokens, in days. 0 disables expiry so tokens never expire.
	TokenTTLDays int `yaml:"token_ttl_days"`
	// HTTPProxy, HTTPSProxy and NoProxy configure the forward proxy used for
	// outbound Galaxy API calls, collection downloads and git clones (e.g. a
	// Squid proxy). Empty values fall back to the process HTTP_PROXY /
	// HTTPS_PROXY / NO_PROXY environment variables.
	HTTPProxy  string `yaml:"http_proxy"`
	HTTPSProxy string `yaml:"https_proxy"`
	NoProxy    string `yaml:"no_proxy"`
	// OIDC optionally enables SSO login for the web dashboard.
	OIDC OIDCConfig `yaml:"oidc"`
}

func GetDefaultConfigYML() string {
	return `listen_addr: "127.0.0.1:8080"
storage_path: "/var/lib/orbitron/storage"
log_path: "/var/log/orbitron/orbitron.log"
tokens_file: "/etc/orbitron/tokens.json"

# Set to true to require authentication for pulling/downloading roles and collections
# Supports Bearer tokens and Basic Auth (e.g., https://token:<TOKEN>@orbitron.local)
require_auth_pull: false

# Maximum number of concurrent download/clone workers spawned during syncs
max_concurrency: 4

# Default lifetime of newly generated administrative tokens in days.
# 0 disables expiry so tokens never expire. Per-token TTLs can be
# overridden when creating tokens via the HTTP token API.
token_ttl_days: 0

# Optional forward proxy for outbound Galaxy API calls, collection downloads
# and git clones (e.g. a Squid proxy). Leave empty to fall back to the
# process HTTP_PROXY / HTTPS_PROXY / NO_PROXY environment variables.
http_proxy: ""
https_proxy: ""
no_proxy: ""

# Optional OpenID Connect (SSO) authentication for the web dashboard, e.g.
# against Keycloak. When enabled the /ui login page gains a "Sign in with SSO"
# button next to the regular access-token login.
#
# In Keycloak: Clients -> Create client -> OpenID Connect, set a client id,
# tick "Client authentication", Valid redirect URIs:
#   https://<your-orbitron-host>/ui/oidc/callback
# The issuer must be the full realm URL including the /realms/ path segment,
# e.g.:
#   issuer: "https://keycloak.example.org/realms/orbitron"
# Credentials tab shows the client secret.
oidc:
  enabled: false
  issuer: ""
  client_id: ""
  client_secret: ""
  # How long an authenticated dashboard session lives (in hours) before the
  # user must log in again. Sessions are in-memory only and reset on restart.
  # 0 falls back to 8 hours.
  session_ttl_hours: 8
  # Canonical redirect URI to advertise to the IDP. Leave empty to auto-derive
  # it from the request (honoring X-Forwarded-Proto / TLS) as
  # <scheme>://<host>/ui/oidc/callback. If set, it must exactly match a
  # registered redirect URI in the Keycloak client, e.g.:
  #   redirect_uri: "https://orbitron.example.org/ui/oidc/callback"
  redirect_uri: ""
  # Optional access filter: only users who are a member of at least one of
  # these groups may log in (compared against the ID token "groups" claim).
  # Empty disables the filter. Keycloak: add the built-in "groups" client
  # scope to the client, or create a "Group Membership" protocol mapper with
  # "Add to ID token: ON", so the ID token carries the claim.
  allowed_groups: []
  #   e.g. allowed_groups: ["orbitron-admins", "orbitron-ops"]
`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		ListenAddr:      "127.0.0.1:8080",
		StoragePath:     "/var/lib/orbitron/storage",
		LogPath:         "/var/log/orbitron/orbitron.log",
		TokensFile:      "/etc/orbitron/tokens.json",
		RequireAuthPull: false,
		MaxConcurrency:  4,
		TokenTTLDays:    0,
		OIDC:            OIDCConfig{SessionTTLHours: 8},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil
	}

	err = yaml.Unmarshal(data, cfg)
	return cfg, err
}
