// Package config loads, validates and persists the node configuration (config.toml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

const (
	FileName       = "config.toml"
	KeystoreFile   = "keystore.json"
	AdminTokenFile = "admin.token"
	APIKeysFile    = "apikeys.json"
	SessionsFile   = "sessions.json"
	ProviderFile   = "provider_state.json"
	LockFile       = "node.lock"
	LogDir         = "logs"

	RoleConsumer = "consumer"
	RoleProvider = "provider"
	RoleBoth     = "both"
)

// Duration is a time.Duration that (un)marshals as a human string like "1h30m".
type Duration struct{ time.Duration }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// D is shorthand for building a Duration.
func D(v time.Duration) Duration { return Duration{v} }

type Config struct {
	Network string `toml:"network"`
	Role    string `toml:"role"`

	Chain    ChainConfig    `toml:"chain"`
	Wallet   WalletConfig   `toml:"wallet"`
	Gateway  GatewayConfig  `toml:"gateway"`
	Sessions SessionConfig  `toml:"sessions"`
	Provider ProviderConfig `toml:"provider"`
	Log      LogConfig      `toml:"log"`

	// Home is the directory this config was loaded from. Not persisted.
	Home string `toml:"-"`
}

type ChainConfig struct {
	RPCURLs         []string `toml:"rpc_urls"`
	ChainID         int64    `toml:"chain_id,omitempty"`
	DiamondAddress  string   `toml:"diamond_address,omitempty"`
	MORTokenAddress string   `toml:"mor_token_address,omitempty"`
	LegacyTx        bool     `toml:"legacy_tx"`
}

type WalletConfig struct {
	Keystore     string `toml:"keystore"`
	Address      string `toml:"address"`
	PasswordFile string `toml:"password_file,omitempty"`
}

type GatewayConfig struct {
	Enabled     bool     `toml:"enabled"`
	Listen      string   `toml:"listen"`
	CORSOrigins []string `toml:"cors_origins"`
}

type SessionConfig struct {
	Duration          Duration `toml:"duration"`
	MaxPerModel       int      `toml:"max_per_model"`
	MaxTotal          int      `toml:"max_total"`
	MaxStake          string   `toml:"max_stake"`
	IdleTimeout       Duration `toml:"idle_timeout"`
	RenewBefore       Duration `toml:"renew_before"`
	CloseOnShutdown   bool     `toml:"close_on_shutdown"`
	DirectPayment     bool     `toml:"direct_payment"`
	ProviderTimeout   Duration `toml:"provider_timeout"`
	QueueTimeout      Duration `toml:"queue_timeout"`
	HealthPolicy      string   `toml:"health_policy"`
	ProviderAllowlist []string `toml:"provider_allowlist"`
	ProviderDenylist  []string `toml:"provider_denylist"`
	AutoWithdraw      bool     `toml:"auto_withdraw"`
}

type ProviderConfig struct {
	Enabled        bool            `toml:"enabled"`
	Listen         string          `toml:"listen"`
	PublicEndpoint string          `toml:"public_endpoint"`
	Models         []ProviderModel `toml:"models"`
}

type ProviderModel struct {
	ModelID      string `toml:"model_id"`
	Name         string `toml:"name"`
	APIURL       string `toml:"api_url"`
	APIKey       string `toml:"api_key,omitempty"`
	BackendModel string `toml:"backend_model,omitempty"`
	MaxSessions  int    `toml:"max_sessions"`
}

type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
	File   string `toml:"file,omitempty"`
}

// Default returns a complete configuration for the given network.
func Default(network string) *Config {
	n, err := LookupNetwork(network)
	if err != nil {
		n, _ = LookupNetwork("mainnet")
	}
	return &Config{
		Network: n.Name,
		Role:    RoleConsumer,
		Chain: ChainConfig{
			RPCURLs: append([]string(nil), n.PublicRPCs...),
		},
		Wallet: WalletConfig{Keystore: KeystoreFile},
		Gateway: GatewayConfig{
			Enabled: true,
			Listen:  "127.0.0.1:8082",
		},
		Sessions: SessionConfig{
			Duration:        D(time.Hour),
			MaxPerModel:     2,
			MaxTotal:        6,
			MaxStake:        "0",
			IdleTimeout:     D(30 * time.Minute),
			RenewBefore:     D(2 * time.Minute),
			CloseOnShutdown: true,
			ProviderTimeout: D(2 * time.Minute),
			QueueTimeout:    D(2 * time.Minute),
			HealthPolicy:    "permissive",
			AutoWithdraw:    true,
		},
		Provider: ProviderConfig{
			Listen: "0.0.0.0:3333",
		},
		Log: LogConfig{Level: "info", Format: "auto"},
	}
}

// DefaultHome resolves the node home directory: $MORCORE_HOME, else the OS
// user config dir (e.g. ~/.config/morcore, ~/Library/Application Support/morcore,
// %AppData%\morcore).
func DefaultHome() string {
	if h := os.Getenv("MORCORE_HOME"); h != "" {
		return h
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "morcore")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".morcore")
	}
	return ".morcore"
}

// Exists reports whether a config file is present in home.
func Exists(home string) bool {
	return store.Exists(filepath.Join(home, FileName))
}

// ErrNotInitialized is returned when no config exists yet.
var ErrNotInitialized = errors.New("node is not initialized")

// Load reads home/config.toml, fills in network defaults and validates it.
func Load(home string) (*Config, error) {
	path := filepath.Join(home, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	network := "mainnet"
	var probe struct {
		Network string `toml:"network"`
	}
	if _, err := toml.Decode(string(data), &probe); err == nil && probe.Network != "" {
		network = probe.Network
	}
	cfg := Default(network)
	cfg.Chain.RPCURLs = nil
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Home = home
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return cfg, nil
}

// applyEnv lets container deployments override secrets and endpoints.
func (c *Config) applyEnv() {
	if v := os.Getenv("MORCORE_RPC_URLS"); v != "" {
		c.Chain.RPCURLs = splitList(v)
	}
	if v := os.Getenv("MORCORE_GATEWAY_LISTEN"); v != "" {
		c.Gateway.Listen = v
	}
	if v := os.Getenv("MORCORE_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Save writes the config to home/config.toml with a commented header.
func (c *Config) Save(home string) error {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := toml.NewEncoder(&buf)
	enc.Indent = "  "
	if err := enc.Encode(c); err != nil {
		return err
	}
	return store.WriteFile(filepath.Join(home, FileName), buf.Bytes(), 0o600)
}

const header = `# Morpheus Core Node configuration.
# Edit with care; run "morcore init" to regenerate interactively.
# Durations use Go syntax (e.g. "45m", "1h30m"); MOR amounts are decimal strings.

`

// NetworkInfo returns the network definition with any chain overrides applied.
func (c *Config) NetworkInfo() Network {
	n, _ := LookupNetwork(c.Network)
	if c.Chain.ChainID != 0 {
		n.ChainID = c.Chain.ChainID
	}
	if c.Chain.DiamondAddress != "" {
		n.DiamondAddress = c.Chain.DiamondAddress
	}
	if c.Chain.MORTokenAddress != "" {
		n.MORTokenAddress = c.Chain.MORTokenAddress
	}
	if len(c.Chain.RPCURLs) > 0 {
		n.PublicRPCs = c.Chain.RPCURLs
	}
	return n
}

func (c *Config) ConsumerEnabled() bool {
	return c.Role == RoleConsumer || c.Role == RoleBoth
}

func (c *Config) ProviderEnabled() bool {
	return c.Role == RoleProvider || c.Role == RoleBoth
}

// Path joins name onto the home directory.
func (c *Config) Path(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(c.Home, name)
}

// Validate checks the configuration for mistakes that would stop the node.
func (c *Config) Validate() error {
	var errs []error
	if _, err := LookupNetwork(c.Network); err != nil {
		errs = append(errs, err)
	}
	switch c.Role {
	case RoleConsumer, RoleProvider, RoleBoth:
	default:
		errs = append(errs, fmt.Errorf("role must be consumer, provider or both (got %q)", c.Role))
	}
	if len(c.Chain.RPCURLs) == 0 {
		n, _ := LookupNetwork(c.Network)
		c.Chain.RPCURLs = append([]string(nil), n.PublicRPCs...)
	}
	if c.ConsumerEnabled() && c.Gateway.Enabled {
		if _, _, err := net.SplitHostPort(c.Gateway.Listen); err != nil {
			errs = append(errs, fmt.Errorf("gateway.listen: %w", err))
		}
	}
	s := &c.Sessions
	if s.Duration.Duration < 5*time.Minute {
		errs = append(errs, fmt.Errorf("sessions.duration must be at least 5m (contract minimum)"))
	}
	if s.MaxPerModel < 1 {
		s.MaxPerModel = 1
	}
	if s.MaxTotal < s.MaxPerModel {
		s.MaxTotal = s.MaxPerModel
	}
	if _, err := units.ParseToken(s.MaxStake); err != nil {
		errs = append(errs, fmt.Errorf("sessions.max_stake: %w", err))
	}
	switch s.HealthPolicy {
	case "", "permissive", "preferred", "strict":
	default:
		errs = append(errs, fmt.Errorf("sessions.health_policy must be permissive, preferred or strict"))
	}
	if c.ProviderEnabled() {
		if _, _, err := net.SplitHostPort(c.Provider.Listen); err != nil {
			errs = append(errs, fmt.Errorf("provider.listen: %w", err))
		}
		for i, m := range c.Provider.Models {
			if m.ModelID == "" || m.APIURL == "" {
				errs = append(errs, fmt.Errorf("provider.models[%d]: model_id and api_url are required", i))
			}
		}
	}
	return errors.Join(errs...)
}

// MaxStakeWei returns the configured total stake cap (0 = unlimited).
func (c *Config) MaxStakeWei() string { return c.Sessions.MaxStake }
