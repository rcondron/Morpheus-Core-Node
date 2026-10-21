package wallet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"golang.org/x/crypto/scrypt"
)

// Web3 Secret Storage v3 (the geth / MetaMask keystore format), implemented
// directly to keep the binary free of go-ethereum's account manager.

type keystoreJSON struct {
	Address string     `json:"address"`
	Crypto  cryptoJSON `json:"crypto"`
	ID      string     `json:"id"`
	Version int        `json:"version"`
}

type cryptoJSON struct {
	Cipher       string         `json:"cipher"`
	CipherText   string         `json:"ciphertext"`
	CipherParams cipherParams   `json:"cipherparams"`
	KDF          string         `json:"kdf"`
	KDFParams    map[string]any `json:"kdfparams"`
	MAC          string         `json:"mac"`
}

type cipherParams struct {
	IV string `json:"iv"`
}

func encryptKeystore(priv *ecdsa.PrivateKey, password string, n, p int) ([]byte, error) {
	salt := make([]byte, 32)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	derived, err := scrypt.Key([]byte(password), salt, n, 8, p, 32)
	if err != nil {
		return nil, err
	}
	keyBytes := crypto.FromECDSA(priv)
	ct, err := aesCTR(derived[:16], keyBytes, iv)
	if err != nil {
		return nil, err
	}
	mac := crypto.Keccak256(derived[16:32], ct)
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	doc := keystoreJSON{
		Address: hex.EncodeToString(crypto.PubkeyToAddress(priv.PublicKey).Bytes()),
		Crypto: cryptoJSON{
			Cipher:       "aes-128-ctr",
			CipherText:   hex.EncodeToString(ct),
			CipherParams: cipherParams{IV: hex.EncodeToString(iv)},
			KDF:          "scrypt",
			KDFParams: map[string]any{
				"dklen": 32, "n": n, "p": p, "r": 8,
				"salt": hex.EncodeToString(salt),
			},
			MAC: hex.EncodeToString(mac),
		},
		ID:      id.String(),
		Version: 3,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func decryptKeystore(data []byte, password string) (*ecdsa.PrivateKey, error) {
	var doc keystoreJSON
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not a keystore file: %w", err)
	}
	if doc.Version != 3 {
		return nil, fmt.Errorf("unsupported keystore version %d", doc.Version)
	}
	c := doc.Crypto
	if !strings.EqualFold(c.Cipher, "aes-128-ctr") {
		return nil, fmt.Errorf("unsupported keystore cipher %q", c.Cipher)
	}
	derived, err := deriveKey(c, password)
	if err != nil {
		return nil, err
	}
	ct, err := hex.DecodeString(c.CipherText)
	if err != nil {
		return nil, fmt.Errorf("invalid ciphertext: %w", err)
	}
	wantMAC, err := hex.DecodeString(c.MAC)
	if err != nil {
		return nil, fmt.Errorf("invalid mac: %w", err)
	}
	if subtle.ConstantTimeCompare(crypto.Keccak256(derived[16:32], ct), wantMAC) != 1 {
		return nil, ErrWrongPassword
	}
	iv, err := hex.DecodeString(c.CipherParams.IV)
	if err != nil {
		return nil, fmt.Errorf("invalid iv: %w", err)
	}
	keyBytes, err := aesCTR(derived[:16], ct, iv)
	if err != nil {
		return nil, err
	}
	priv, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return nil, err
	}
	if doc.Address != "" && common.HexToAddress(doc.Address) != crypto.PubkeyToAddress(priv.PublicKey) {
		return nil, errors.New("keystore address does not match its key")
	}
	return priv, nil
}

func deriveKey(c cryptoJSON, password string) ([]byte, error) {
	num := func(k string) int {
		v, _ := c.KDFParams[k].(float64)
		return int(v)
	}
	saltHex, _ := c.KDFParams["salt"].(string)
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return nil, fmt.Errorf("invalid salt: %w", err)
	}
	dkLen := num("dklen")
	if dkLen < 32 {
		return nil, errors.New("keystore dklen must be at least 32")
	}
	switch strings.ToLower(c.KDF) {
	case "scrypt":
		return scrypt.Key([]byte(password), salt, num("n"), num("r"), num("p"), dkLen)
	case "pbkdf2":
		if prf, _ := c.KDFParams["prf"].(string); prf != "hmac-sha256" {
			return nil, fmt.Errorf("unsupported pbkdf2 prf %q", prf)
		}
		return pbkdf2.Key(sha256.New, password, salt, num("c"), dkLen)
	default:
		return nil, fmt.Errorf("unsupported keystore kdf %q", c.KDF)
	}
}

func aesCTR(key, in, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(in))
	cipher.NewCTR(block, iv).XORKeyStream(out, in)
	return out, nil
}
