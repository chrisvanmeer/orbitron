package config

import (
	"os"

	yaml "gopkg.in/yaml.v3"
)

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
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil
	}

	err = yaml.Unmarshal(data, cfg)
	return cfg, err
}
