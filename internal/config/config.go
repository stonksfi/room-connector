// Package config holds roomd's on-disk settings and paths, all under ~/.room
// (or $ROOM_HOME).
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// DefaultBaseURL is used until `roomd login --server` names another. Release
// builds set it to the deployed room-be with
// -ldflags "-X github.com/stonksfi/room-connector/internal/config.DefaultBaseURL=…".
var DefaultBaseURL = "http://localhost:8888"

// Config is roomd's persisted state. The device token is not here: it lives
// in the credential store.
type Config struct {
	BaseURL    string `json:"base_url"`
	DeviceID   int64  `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	// Providers holds each provider's own settings by provider name, e.g. the
	// agent binary recorded at install because launchd starts roomd with a
	// minimal PATH.
	Providers map[string]map[string]string `json:"providers,omitempty"`
}

// ProviderSetting returns one of a provider's settings, or "".
func (c Config) ProviderSetting(provider, key string) string {
	return c.Providers[provider][key]
}

// SetProviderSetting records one of a provider's settings.
func (c *Config) SetProviderSetting(provider, key, value string) {
	if c.Providers == nil {
		c.Providers = map[string]map[string]string{}
	}
	if c.Providers[provider] == nil {
		c.Providers[provider] = map[string]string{}
	}
	c.Providers[provider][key] = value
}

// Home is roomd's directory.
func Home() string {
	if h := os.Getenv("ROOM_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".room"
	}
	return filepath.Join(home, ".room")
}

// SocketPath is the local API socket the hooks and in-session script use.
// Unix socket paths are limited to about 104 bytes, so it stays short.
func SocketPath() string {
	if p := os.Getenv("ROOM_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(Home(), "roomd.sock")
}

func path() string {
	return filepath.Join(Home(), "config.json")
}

// Load reads the config, returning defaults when there is none.
func Load() (Config, error) {
	cfg := Config{BaseURL: DefaultBaseURL}
	raw, err := os.ReadFile(path())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return cfg, nil
}

// Save writes the config, readable only by its owner.
func Save(cfg Config) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(), raw, 0o600)
}
