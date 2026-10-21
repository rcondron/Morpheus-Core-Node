package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	cfg := Default("testnet")
	cfg.Role = RoleBoth
	cfg.Wallet.Address = "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
	cfg.Sessions.Duration = D(4 * time.Hour)
	cfg.Sessions.MaxStake = "25.5"
	cfg.Provider.PublicEndpoint = "node.example.com:3333"
	cfg.Provider.Models = []ProviderModel{{ModelID: "0x" + strings.Repeat("ab", 32), Name: "m", APIURL: "http://localhost:8000/v1/chat/completions", MaxSessions: 4}}
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, FileName)); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("config is group/world readable: %v", st.Mode())
	}
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != "testnet" || got.Role != RoleBoth || got.Sessions.Duration.Duration != 4*time.Hour ||
		got.Sessions.MaxStake != "25.5" || len(got.Provider.Models) != 1 || got.NetworkInfo().ChainID != 84532 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	data, _ := os.ReadFile(filepath.Join(home, FileName))
	if strings.Contains(string(data), "chain_id") {
		t.Fatal("zero chain_id override should be omitted")
	}
}

func TestValidate(t *testing.T) {
	cfg := Default("mainnet")
	cfg.Sessions.Duration = D(time.Minute)
	cfg.Role = "miner"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "5m") || !strings.Contains(err.Error(), "role") {
		t.Fatalf("expected duration and role errors, got %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir()); err != ErrNotInitialized {
		t.Fatalf("expected ErrNotInitialized, got %v", err)
	}
}

func TestEnvOverride(t *testing.T) {
	home := t.TempDir()
	if err := Default("mainnet").Save(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORCORE_RPC_URLS", "https://a.example, https://b.example")
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Chain.RPCURLs) != 2 || cfg.Chain.RPCURLs[1] != "https://b.example" {
		t.Fatalf("env override not applied: %v", cfg.Chain.RPCURLs)
	}
}
