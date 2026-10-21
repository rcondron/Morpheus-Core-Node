package morrpc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
)

// Signer is the key material a node signs MOR-RPC messages with.
type Signer interface {
	Address() common.Address
	PublicKey() []byte
	Sign(hash []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

var ErrInvalidSignature = errors.New("invalid signature")

// Now returns the protocol timestamp (unix milliseconds).
func Now() uint64 { return uint64(time.Now().UnixMilli()) }

// SignParams signs keccak256(json(params)). params must have an empty signature field.
func SignParams(params any, s Signer) (HexString, error) {
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return s.Sign(crypto.Keccak256(data))
}

// VerifyParams checks a signature over json(params) against an uncompressed public key.
func VerifyParams(params any, sig HexString, pubKey []byte) bool {
	if len(sig) != 65 || len(pubKey) == 0 {
		return false
	}
	data, err := json.Marshal(params)
	if err != nil {
		return false
	}
	return crypto.VerifySignature(pubKey, crypto.Keccak256(data), sig[:64])
}

// RecoverParams recovers the signer's public key and address from a signature over json(params).
func RecoverParams(params any, sig HexString) ([]byte, common.Address, error) {
	if len(sig) != 65 {
		return nil, common.Address{}, ErrInvalidSignature
	}
	data, err := json.Marshal(params)
	if err != nil {
		return nil, common.Address{}, err
	}
	pub, err := crypto.Ecrecover(crypto.Keccak256(data), sig)
	if err != nil {
		return nil, common.Address{}, ErrInvalidSignature
	}
	return pub, PubKeyAddress(pub), nil
}

// PubKeyAddress derives the Ethereum address of an uncompressed public key.
func PubKeyAddress(pub []byte) common.Address {
	if len(pub) != 65 {
		return common.Address{}
	}
	return common.BytesToAddress(crypto.Keccak256(pub[1:])[12:])
}

// SignEthMessage produces an EIP-191 personal_sign signature (V = 27/28) over
// keccak256(msg), the format the Diamond contract verifies for approvals and receipts.
func SignEthMessage(msg []byte, s Signer) ([]byte, error) {
	sig, err := s.Sign(EthMessageHash(msg))
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

// EthMessageHash is keccak256("\x19Ethereum Signed Message:\n32" || keccak256(msg)).
func EthMessageHash(msg []byte) []byte {
	h := crypto.Keccak256(msg)
	return crypto.Keccak256([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(h))), h)
}

// RecoverEthMessage returns the address that produced an EIP-191 signature.
func RecoverEthMessage(msg, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, ErrInvalidSignature
	}
	s := append([]byte(nil), sig...)
	if s[64] >= 27 {
		s[64] -= 27
	}
	pub, err := crypto.SigToPub(EthMessageHash(msg), s)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// EncryptTo ECIES-encrypts plaintext to an uncompressed secp256k1 public key and
// returns lowercase hex without a 0x prefix (the reference wire format).
func EncryptTo(pubKey []byte, plaintext []byte) (string, error) {
	pub, err := crypto.UnmarshalPubkey(pubKey)
	if err != nil {
		return "", fmt.Errorf("invalid public key: %w", err)
	}
	ct, err := ecies.Encrypt(rand.Reader, ecies.ImportECDSAPublic(pub), plaintext, nil, nil)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ct), nil
}

// DecryptHex decrypts a hex ECIES ciphertext (with or without 0x) using s.
func DecryptHex(s Signer, ciphertextHex string) ([]byte, error) {
	ct, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(ciphertextHex, "0x"), "0X"))
	if err != nil {
		return nil, err
	}
	return s.Decrypt(ct)
}

var (
	approvalArgs = mustArgs("bytes32", "uint256", "address", "uint128")
	reportArgs   = mustArgs("bytes32", "uint256", "uint128", "uint32", "uint32", "uint32", "uint32")
)

func mustArgs(types ...string) abi.Arguments {
	args := make(abi.Arguments, len(types))
	for i, t := range types {
		typ, err := abi.NewType(t, "", nil)
		if err != nil {
			panic(err)
		}
		args[i] = abi.Argument{Type: typ}
	}
	return args
}

// EncodeApproval ABI-encodes a provider session approval.
func EncodeApproval(bidID common.Hash, chainID *big.Int, user common.Address, timestamp uint64) ([]byte, error) {
	return approvalArgs.Pack(bidID, chainID, user, new(big.Int).SetUint64(timestamp))
}

// Approval is a decoded provider approval.
type Approval struct {
	BidID     common.Hash
	ChainID   *big.Int
	User      common.Address
	Timestamp *big.Int
}

// DecodeApproval reverses EncodeApproval.
func DecodeApproval(data []byte) (*Approval, error) {
	vals, err := approvalArgs.Unpack(data)
	if err != nil {
		return nil, err
	}
	return &Approval{
		BidID:     common.Hash(vals[0].([32]byte)),
		ChainID:   vals[1].(*big.Int),
		User:      vals[2].(common.Address),
		Timestamp: vals[3].(*big.Int),
	}, nil
}

// Report carries the session quality statistics a closeout receipt attests to.
type Report struct {
	SessionID     common.Hash
	ChainID       *big.Int
	Timestamp     uint64
	TPSScaled1000 uint32
	TTFTMs        uint32
	InputTokens   uint32
	OutputTokens  uint32
}

// EncodeReport ABI-encodes a session closeout receipt.
func EncodeReport(r Report) ([]byte, error) {
	return reportArgs.Pack(r.SessionID, r.ChainID, new(big.Int).SetUint64(r.Timestamp), r.TPSScaled1000, r.TTFTMs, r.InputTokens, r.OutputTokens)
}

// DecodeReport reverses EncodeReport.
func DecodeReport(data []byte) (*Report, error) {
	vals, err := reportArgs.Unpack(data)
	if err != nil {
		return nil, err
	}
	return &Report{
		SessionID:     common.Hash(vals[0].([32]byte)),
		ChainID:       vals[1].(*big.Int),
		Timestamp:     vals[2].(*big.Int).Uint64(),
		TPSScaled1000: vals[3].(uint32),
		TTFTMs:        vals[4].(uint32),
		InputTokens:   vals[5].(uint32),
		OutputTokens:  vals[6].(uint32),
	}, nil
}

// SignedReport builds and signs a closeout receipt (EIP-191).
func SignedReport(r Report, s Signer) (msg, sig []byte, err error) {
	msg, err = EncodeReport(r)
	if err != nil {
		return nil, nil, err
	}
	sig, err = SignEthMessage(msg, s)
	return msg, sig, err
}
