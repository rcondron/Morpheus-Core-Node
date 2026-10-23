// Package config loads, validates and persists the node configuration (config.toml).
package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	StorageFile    = "storage.json"          // the user's stored files and leases
	StorageState   = "storage_provider.json" // files this provider stores for users
	TempDir        = "tmp"
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
	Storage  StorageConfig  `toml:"storage"`
	Provider ProviderConfig `toml:"provider"`
	Log      LogConfig      `toml:"log"`

	// Home is the directory this config was loaded from. Not persisted.
	Home string `toml:"-"`
}

type ChainConfig struct {
	RPCURLs         []string `toml:"rpc_urls"`
	ChainID         int64    `toml:"chain_id,omitzero"`
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

// StorageConfig is the consumer side of marketplace storage: files kept on
// IPFS by storage providers, paid per second through storage sessions.
type StorageConfig struct {
	Lease         Duration `toml:"lease"`        // length of each storage session (capped by the contract)
	RenewBefore   Duration `toml:"renew_before"` // open the next session this long before one ends
	Replicas      int      `toml:"replicas"`     // providers that keep each file
	Encrypt       bool     `toml:"encrypt"`      // encrypt new files to the wallet key
	AuditInterval Duration `toml:"audit_interval"`
	MaxStake      string   `toml:"max_stake"` // cap on MOR escrowed in storage sessions ("0" = none)
	Providers     []string `toml:"providers"` // optional allowlist of storage providers
}

type ProviderConfig struct {
	Enabled        bool                  `toml:"enabled"`
	Listen         string                `toml:"listen"`
	PublicEndpoint string                `toml:"public_endpoint"`
	Models         []ProviderModel       `toml:"models"`
	Agents         []ProviderAgent       `toml:"agents,omitempty"`
	Storage        ProviderStorageConfig `toml:"storage"`
}

// ProviderStorageConfig sells storage: files are pinned on a Kubo (IPFS)
// node and paid for through sessions on storage listings.
type ProviderStorageConfig struct {
	Enabled       bool     `toml:"enabled"`
	IPFSAPI       string   `toml:"ipfs_api"`            // Kubo RPC API URL
	IPFSAuth      string   `toml:"ipfs_auth,omitempty"` // Authorization header for the Kubo API
	Listings      []string `toml:"listings"`            // storage listing ids this node serves
	MaxFileSize   string   `toml:"max_file_size"`       // e.g. "4GiB"
	MaxTotal      string   `toml:"max_total"`           // cap on stored bytes ("0" = Kubo's StorageMax)
	Grace         Duration `toml:"grace"`               // keep files this long after coverage ends
	AcceptStipend bool     `toml:"accept_stipend"`      // serve sessions paid from the compute budget
	Gateway       string   `toml:"gateway,omitempty"`   // public IPFS gateway to advertise
}

// Backend API types a provider model can be served through. They match the
// reference node's apiType values so models-config.json imports unchanged.
const (
	APIOpenAI       = "openai"        // any OpenAI-compatible endpoint (chat, embeddings, audio)
	APIClaude       = "claudeai"      // Anthropic Messages API
	APIProdiaSD     = "prodia-sd"     // Prodia v1 Stable Diffusion
	APIProdiaSDXL   = "prodia-sdxl"   // Prodia v1 SDXL
	APIProdiaV2     = "prodia-v2"     // Prodia v2 jobs (image or video)
	APIHyperbolicSD = "hyperbolic-sd" // Hyperbolic image generation
	APIDecisions    = "decisions"     // TypeSafe / OpenRouter Decisions
)

// APITypes lists every supported backend API type.
var APITypes = []string{APIOpenAI, APIClaude, APIProdiaSD, APIProdiaSDXL, APIProdiaV2, APIHyperbolicSD, APIDecisions}

// Capacity policies decide which sessions count against max_sessions.
const (
	CapacitySimple      = "simple"       // every unexpired session
	CapacityIdleTimeout = "idle_timeout" // only sessions used in the last 15 minutes
)

type ProviderModel struct {
	ModelID        string            `toml:"model_id"`
	Name           string            `toml:"name"`
	APIType        string            `toml:"api_type,omitempty"`
	APIURL         string            `toml:"api_url"`
	APIKey         string            `toml:"api_key,omitempty"`
	BackendModel   string            `toml:"backend_model,omitempty"`
	MaxSessions    int               `toml:"max_sessions"`
	CapacityPolicy string            `toml:"capacity_policy,omitempty"`
	Parameters     map[string]string `toml:"parameters,omitempty"`
}

// Type returns the backend API type, defaulting to OpenAI-compatible.
func (m ProviderModel) Type() string {
	if m.APIType == "" {
		return APIOpenAI
	}
	return m.APIType
}

// ProviderAgent is an MCP tool server sold on the marketplace: consumers with
// a session for AgentID list and call its tools (agent.get_tools / agent.call_tool).
type ProviderAgent struct {
	AgentID        string            `toml:"agent_id"`
	Name           string            `toml:"name"`
	Command        string            `toml:"command"`
	Args           []string          `toml:"args,omitempty"`
	Env            map[string]string `toml:"env,omitempty"`
	MaxSessions    int               `toml:"max_sessions"`
	CapacityPolicy string            `toml:"capacity_policy,omitempty"`
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
		Storage: StorageConfig{
			Lease:         D(24 * time.Hour),
			RenewBefore:   D(30 * time.Minute),
			Replicas:      1,
			Encrypt:       true,
			AuditInterval: D(6 * time.Hour),
			MaxStake:      "0",
		},
		Provider: ProviderConfig{
			Listen: "0.0.0.0:3333",
			Storage: ProviderStorageConfig{
				IPFSAPI:     "http://127.0.0.1:5001",
				MaxFileSize: "4GiB",
				MaxTotal:    "0",
				Grace:       D(72 * time.Hour),
			},
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
	if err := cfg.applyProviderEnv(); err != nil {
		return nil, err
	}
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

// applyProviderEnv replaces the served models/agents with inline JSON in the
// reference node's models-config / agents-config shape, the way containers
// (Akash, TEE, Kubernetes secrets) usually deliver them.
func (c *Config) applyProviderEnv() error {
	if v := strings.TrimSpace(os.Getenv("MORCORE_PROVIDER_MODELS")); v != "" {
		models, err := ImportModelsConfig([]byte(v))
		if err != nil {
			return fmt.Errorf("MORCORE_PROVIDER_MODELS: %w", err)
		}
		c.Provider.Models = models
	}
	if v := strings.TrimSpace(os.Getenv("MORCORE_PROVIDER_AGENTS")); v != "" {
		agents, err := ImportAgentsConfig([]byte(v))
		if err != nil {
			return fmt.Errorf("MORCORE_PROVIDER_AGENTS: %w", err)
		}
		c.Provider.Agents = agents
	}
	return nil
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
	st := &c.Storage
	if st.Lease.Duration < 5*time.Minute {
		errs = append(errs, errors.New("storage.lease must be at least 5m (contract minimum)"))
	}
	if st.Replicas < 1 {
		st.Replicas = 1
	}
	if _, err := units.ParseToken(st.MaxStake); err != nil {
		errs = append(errs, fmt.Errorf("storage.max_stake: %w", err))
	}
	if ps := c.Provider.Storage; ps.Enabled {
		if u, err := url.Parse(ps.IPFSAPI); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider.storage.ipfs_api %q is not a URL", ps.IPFSAPI))
		}
		if _, err := ParseBytes(ps.MaxFileSize); err != nil {
			errs = append(errs, fmt.Errorf("provider.storage.max_file_size: %w", err))
		}
		if _, err := ParseBytes(ps.MaxTotal); err != nil {
			errs = append(errs, fmt.Errorf("provider.storage.max_total: %w", err))
		}
		for _, l := range ps.Listings {
			if !isHash(l) {
				errs = append(errs, fmt.Errorf("provider.storage.listings: %q is not a 0x-prefixed 32-byte id", l))
			}
		}
	}
	if c.ProviderEnabled() {
		if _, _, err := net.SplitHostPort(c.Provider.Listen); err != nil {
			errs = append(errs, fmt.Errorf("provider.listen: %w", err))
		}
		ids := map[string]bool{}
		for i, m := range c.Provider.Models {
			if err := m.validate(); err != nil {
				errs = append(errs, fmt.Errorf("provider.models[%d]: %w", i, err))
			}
			if ids[strings.ToLower(m.ModelID)] {
				errs = append(errs, fmt.Errorf("provider.models[%d]: model_id %s is configured twice", i, m.ModelID))
			}
			ids[strings.ToLower(m.ModelID)] = true
		}
		for i, a := range c.Provider.Agents {
			if err := a.validate(); err != nil {
				errs = append(errs, fmt.Errorf("provider.agents[%d]: %w", i, err))
			}
			if ids[strings.ToLower(a.AgentID)] {
				errs = append(errs, fmt.Errorf("provider.agents[%d]: agent_id %s is already configured", i, a.AgentID))
			}
			ids[strings.ToLower(a.AgentID)] = true
		}
	}
	return errors.Join(errs...)
}

// NeedsURL reports whether an API type has no default endpoint.
func NeedsURL(apiType string) bool {
	switch apiType {
	case APIProdiaSD, APIProdiaSDXL, APIProdiaV2, APIHyperbolicSD:
		return false
	}
	return true
}

func (m ProviderModel) validate() error {
	if !isHash(m.ModelID) {
		return fmt.Errorf("model_id must be a 0x-prefixed 32-byte hex id (got %q)", m.ModelID)
	}
	if !slices.Contains(APITypes, m.Type()) {
		return fmt.Errorf("api_type must be one of %s (got %q)", strings.Join(APITypes, ", "), m.APIType)
	}
	if m.APIURL == "" && NeedsURL(m.Type()) {
		return errors.New("api_url is required")
	}
	if m.APIURL != "" {
		if u, err := url.Parse(m.APIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("api_url %q is not an http(s) URL", m.APIURL)
		}
	}
	return validCapacity(m.CapacityPolicy, m.MaxSessions)
}

func (a ProviderAgent) validate() error {
	if !isHash(a.AgentID) {
		return fmt.Errorf("agent_id must be a 0x-prefixed 32-byte hex id (got %q)", a.AgentID)
	}
	if strings.TrimSpace(a.Command) == "" {
		return errors.New("command is required")
	}
	return validCapacity(a.CapacityPolicy, a.MaxSessions)
}

func validCapacity(policy string, maxSessions int) error {
	switch policy {
	case "", CapacitySimple, CapacityIdleTimeout:
	default:
		return fmt.Errorf("capacity_policy must be simple or idle_timeout (got %q)", policy)
	}
	if maxSessions < 0 {
		return errors.New("max_sessions cannot be negative")
	}
	return nil
}

func isHash(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(strings.ToLower(s), "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

// MaxStakeWei returns the configured total stake cap (0 = unlimited).
func (c *Config) MaxStakeWei() string { return c.Sessions.MaxStake }
