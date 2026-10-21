package wallet

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
)

func TestBIP32Vector1(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	cases := map[string]string{
		"m":                      "e8f32e723decf4051aefac8e2c93c9c5b214313817cdb01a1494b917c8436b35",
		"m/0'":                   "edb2e14f9ee77d26dd93b4ecede8d16ed408ce149b6cd80b0715a2d911a0afea",
		"m/0'/1":                 "3c6cb8d0f6a264c91ea8b5030fadaa8e538b020f0a387421a12de9319dc93368",
		"m/0'/1/2'":              "cbce0d719ecf7431d88e6a89fa1483e02e35092af60c042b1df2ff59fa424dca",
		"m/0'/1/2'/2":            "0f479245fb19a38a1954c5c7c0ebab2f9bdfd96a17563ef28a6a4b1a2a764ef4",
		"m/0'/1/2'/2/1000000000": "471b76e389e528d6de6d816857e012c5455051cad6660850e58372a6c3e6e7c8",
	}
	for path, want := range cases {
		priv, err := deriveFromSeed(seed, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := hex.EncodeToString(crypto.FromECDSA(priv)); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}

func TestMnemonicAccounts(t *testing.T) {
	// Hardhat / Foundry default development mnemonic.
	const m = "test test test test test test test test test test test junk"
	want := []string{
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
		"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
	}
	for i, addr := range want {
		w, err := FromMnemonic("  "+strings.ToUpper(m)+" ", "", i)
		if err != nil {
			t.Fatal(err)
		}
		if w.Address().Hex() != addr {
			t.Errorf("index %d: got %s want %s", i, w.Address().Hex(), addr)
		}
	}
	if _, err := FromMnemonic("test test test", "", 0); err == nil {
		t.Fatal("expected invalid mnemonic error")
	}
}

func TestNewMnemonic(t *testing.T) {
	m, err := NewMnemonic(false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(m)); n != 12 || !ValidMnemonic(m) {
		t.Fatalf("bad mnemonic %q (%d words)", m, n)
	}
}

func TestKeystoreRoundTrip(t *testing.T) {
	w, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ks.json")
	if err := w.SaveKeystore(path, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeystore(path, "wrong"); err != ErrWrongPassword {
		t.Fatalf("expected ErrWrongPassword, got %v", err)
	}
	got, err := LoadKeystore(path, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if got.Address() != w.Address() {
		t.Fatal("address mismatch after round trip")
	}
	addr, err := KeystoreAddress(path)
	if err != nil || addr != w.Address() {
		t.Fatalf("KeystoreAddress: %v %v", addr, err)
	}
}

// Our keystore must interoperate with geth in both directions.
func TestKeystoreGethCompat(t *testing.T) {
	w, _ := Generate()
	ours, err := w.Encrypt("pw")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keystore.DecryptKey(ours, "pw")
	if err != nil {
		t.Fatalf("geth cannot read our keystore: %v", err)
	}
	if k.Address != w.Address() {
		t.Fatal("geth decoded a different address")
	}

	priv, _ := crypto.GenerateKey()
	id, _ := uuid.NewRandom()
	theirs, err := keystore.EncryptKey(&keystore.Key{Id: id, Address: crypto.PubkeyToAddress(priv.PublicKey), PrivateKey: priv}, "pw2", keystore.LightScryptN, keystore.LightScryptP)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptKeystore(theirs, "pw2")
	if err != nil {
		t.Fatalf("cannot read geth keystore: %v", err)
	}
	if got.Address() != crypto.PubkeyToAddress(priv.PublicKey) {
		t.Fatal("address mismatch reading geth keystore")
	}
}

func TestFromHex(t *testing.T) {
	w, err := FromHex("0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatal(err)
	}
	if w.Address().Hex() != "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23" {
		t.Fatalf("unexpected address %s", w.Address().Hex())
	}
	if _, err := FromHex("nothex"); err == nil {
		t.Fatal("expected error")
	}
}
