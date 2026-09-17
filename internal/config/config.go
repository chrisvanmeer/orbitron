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
}

func GetDefaultConfigYML() string {
	return `listen_addr: "127.0.0.1:8080"
storage_path: "/var/lib/orbitron/storage"
log_path: "/var/log/orbitron/orbitron.log"
tokens_file: "/etc/orbitron/tokens.json"

# Set to true to require authentication for pulling/downloading roles and collections
# Supports Bearer tokens and Basic Auth (e.g., https://token:<TOKEN>@orbitron.local)
require_auth_pull: false
`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		ListenAddr:      "127.0.0.1:8080",
		StoragePath:     "/var/lib/orbitron/storage",
		LogPath:         "/var/log/orbitron/orbitron.log",
		TokensFile:      "/etc/orbitron/tokens.json",
		RequireAuthPull: false,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil
	}

	err = yaml.Unmarshal(data, cfg)
	return cfg, err
}

func (c *Config) SetupProxy() {
	// Respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables
}

func EnsureDirExists(path string) error {
	return os.MkdirAll(path, 0750)
}
