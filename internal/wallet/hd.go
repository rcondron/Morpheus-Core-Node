package wallet

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tyler-smith/go-bip39"
)

// Minimal BIP-32 private derivation, enough for BIP-44 Ethereum accounts.
// Keeping this local avoids pulling in a full bitcoin library.

const hardenedOffset = 0x80000000

var (
	curveN        = crypto.S256().Params().N
	errInvalidKey = errors.New("derived key is invalid (astronomically unlikely); try the next index")
)

// DefaultPath is the derivation path MetaMask and most wallets use for account 0.
const DefaultPath = "m/44'/60'/0'/0/0"

// AccountPath returns the standard Ethereum path for an account index.
func AccountPath(index int) string { return fmt.Sprintf("m/44'/60'/0'/0/%d", index) }

// NewMnemonic generates a fresh BIP-39 recovery phrase (12 words by default, 24 with strong=true).
func NewMnemonic(strong bool) (string, error) {
	bits := 128
	if strong {
		bits = 256
	}
	entropy, err := bip39.NewEntropy(bits)
	if err != nil {
		return "", err
	}
	return bip39.NewMnemonic(entropy)
}

// NormalizeMnemonic lowercases and collapses whitespace.
func NormalizeMnemonic(m string) string {
	return strings.Join(strings.Fields(strings.ToLower(m)), " ")
}

// ValidMnemonic reports whether m is a checksummed BIP-39 phrase.
func ValidMnemonic(m string) bool { return bip39.IsMnemonicValid(NormalizeMnemonic(m)) }

// DeriveKey derives the private key at path from a mnemonic (+ optional BIP-39 passphrase).
func DeriveKey(mnemonic, passphrase, path string) (*ecdsa.PrivateKey, error) {
	mnemonic = NormalizeMnemonic(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("invalid recovery phrase (check spelling and word count)")
	}
	seed := bip39.NewSeed(mnemonic, passphrase)
	return deriveFromSeed(seed, path)
}

func deriveFromSeed(seed []byte, path string) (*ecdsa.PrivateKey, error) {
	indices, err := ParsePath(path)
	if err != nil {
		return nil, err
	}
	k, err := masterKey(seed)
	if err != nil {
		return nil, err
	}
	for _, i := range indices {
		if k, err = k.child(i); err != nil {
			return nil, err
		}
	}
	return crypto.ToECDSA(k.key)
}

// ParsePath parses a BIP-32 path like m/44'/60'/0'/0/0.
func ParsePath(path string) ([]uint32, error) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	if len(parts) == 0 || (parts[0] != "m" && parts[0] != "M") {
		return nil, fmt.Errorf("derivation path %q must start with m/", path)
	}
	out := make([]uint32, 0, len(parts)-1)
	for _, p := range parts[1:] {
		hardened := strings.HasSuffix(p, "'") || strings.HasSuffix(p, "h") || strings.HasSuffix(p, "H")
		p = strings.TrimRight(p, "'hH")
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || n >= hardenedOffset {
			return nil, fmt.Errorf("invalid path component %q in %q", p, path)
		}
		idx := uint32(n)
		if hardened {
			idx += hardenedOffset
		}
		out = append(out, idx)
	}
	return out, nil
}

type extKey struct {
	key   []byte
	chain []byte
}

func masterKey(seed []byte) (*extKey, error) {
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	k := new(big.Int).SetBytes(sum[:32])
	if k.Sign() == 0 || k.Cmp(curveN) >= 0 {
		return nil, errInvalidKey
	}
	return &extKey{key: sum[:32], chain: sum[32:]}, nil
}

func (e *extKey) child(i uint32) (*extKey, error) {
	var data []byte
	if i >= hardenedOffset {
		data = append([]byte{0}, e.key...)
	} else {
		priv, err := crypto.ToECDSA(e.key)
		if err != nil {
			return nil, err
		}
		data = crypto.CompressPubkey(&priv.PublicKey)
	}
	data = binary.BigEndian.AppendUint32(data, i)
	mac := hmac.New(sha512.New, e.chain)
	mac.Write(data)
	sum := mac.Sum(nil)
	il := new(big.Int).SetBytes(sum[:32])
	if il.Cmp(curveN) >= 0 {
		return nil, errInvalidKey
	}
	k := il.Add(il, new(big.Int).SetBytes(e.key))
	k.Mod(k, curveN)
	if k.Sign() == 0 {
		return nil, errInvalidKey
	}
	return &extKey{key: math.PaddedBigBytes(k, 32), chain: sum[32:]}, nil
}
