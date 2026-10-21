// Package wallet holds the node's Ethereum key: creation, import, encrypted
// storage (Web3 Secret Storage v3, compatible with geth/MetaMask exports) and signing.
package wallet

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"

	"github.com/rcondron/Morpheus-Core-Node/internal/store"
)

// Scrypt parameters: 64 MiB / ~0.2s per unlock keeps the node light on small
// machines while staying far beyond practical brute force for real passwords.
const (
	scryptN = 1 << 16
	scryptP = 1
)

// ErrWrongPassword is returned when the keystore cannot be decrypted.
var ErrWrongPassword = errors.New("wrong wallet password")

// Wallet is an unlocked key.
type Wallet struct {
	priv *ecdsa.PrivateKey
	addr common.Address
	pub  []byte
}

// New wraps an ECDSA private key.
func New(priv *ecdsa.PrivateKey) *Wallet {
	return &Wallet{
		priv: priv,
		addr: crypto.PubkeyToAddress(priv.PublicKey),
		pub:  crypto.FromECDSAPub(&priv.PublicKey),
	}
}

// Generate creates a random key.
func Generate() (*Wallet, error) {
	priv, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	return New(priv), nil
}

// FromHex imports a hex private key (with or without 0x).
func FromHex(s string) (*Wallet, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	priv, err := crypto.HexToECDSA(s)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	return New(priv), nil
}

// FromMnemonic derives the wallet at account index from a recovery phrase.
func FromMnemonic(mnemonic, passphrase string, index int) (*Wallet, error) {
	priv, err := DeriveKey(mnemonic, passphrase, AccountPath(index))
	if err != nil {
		return nil, err
	}
	return New(priv), nil
}

func (w *Wallet) Address() common.Address          { return w.addr }
func (w *Wallet) PublicKey() []byte                { return w.pub }
func (w *Wallet) PrivateKey() *ecdsa.PrivateKey    { return w.priv }
func (w *Wallet) PrivateKeyBytes() []byte          { return crypto.FromECDSA(w.priv) }
func (w *Wallet) PrivateKeyHex() string            { return "0x" + common.Bytes2Hex(w.PrivateKeyBytes()) }
func (w *Wallet) Sign(hash []byte) ([]byte, error) { return crypto.Sign(hash, w.priv) }

// Decrypt opens an ECIES ciphertext addressed to this wallet's public key.
func (w *Wallet) Decrypt(ciphertext []byte) ([]byte, error) {
	return ecies.ImportECDSA(w.priv).Decrypt(ciphertext, nil, nil)
}

// Encrypt seals the wallet into a Web3 Secret Storage v3 JSON document.
func (w *Wallet) Encrypt(password string) ([]byte, error) {
	return encryptKeystore(w.priv, password, scryptN, scryptP)
}

// SaveKeystore encrypts and writes the wallet to path (0600).
func (w *Wallet) SaveKeystore(path, password string) error {
	data, err := w.Encrypt(password)
	if err != nil {
		return err
	}
	return store.WriteFile(path, data, 0o600)
}

// LoadKeystore decrypts a keystore file.
func LoadKeystore(path, password string) (*Wallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecryptKeystore(data, password)
}

// DecryptKeystore decrypts keystore JSON.
func DecryptKeystore(data []byte, password string) (*Wallet, error) {
	priv, err := decryptKeystore(data, password)
	if err != nil {
		return nil, err
	}
	return New(priv), nil
}

// KeystoreAddress reads the (unencrypted) address field of a keystore file.
func KeystoreAddress(path string) (common.Address, error) {
	var doc struct {
		Address string `json:"address"`
	}
	ok, err := store.ReadJSON(path, &doc)
	if err != nil {
		return common.Address{}, err
	}
	if !ok {
		return common.Address{}, os.ErrNotExist
	}
	if !common.IsHexAddress(doc.Address) {
		return common.Address{}, fmt.Errorf("keystore has no address")
	}
	return common.HexToAddress(doc.Address), nil
}
