package chain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// KeySigner is the wallet capability the transaction sender needs.
type KeySigner interface {
	Address() common.Address
	PrivateKey() *ecdsa.PrivateKey
}

// TxSender serializes every on-chain write from the node's wallet. Holding one
// lock across build → send → mine removes nonce races between concurrent
// session opens and keeps allowance checks consistent.
type TxSender struct {
	k       *Contracts
	key     KeySigner
	legacy  bool
	timeout time.Duration
	log     *slog.Logger
	mu      sync.Mutex
}

func NewTxSender(k *Contracts, key KeySigner, legacy bool, log *slog.Logger) *TxSender {
	return &TxSender{k: k, key: key, legacy: legacy, timeout: 3 * time.Minute, log: log}
}

// Address is the sending wallet.
func (t *TxSender) Address() common.Address { return t.key.Address() }

// Send builds, signs and broadcasts a contract call and waits for it to be mined.
func (t *TxSender) Send(ctx context.Context, to common.Address, data []byte, label string) (*Receipt, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.SendLocked(ctx, to, data, label)
}

// SendLocked is Send for callers already holding the wallet lock.
func (t *TxSender) SendLocked(ctx context.Context, to common.Address, data []byte, label string) (*Receipt, error) {
	c := t.k.C
	from := t.key.Address()
	msg := CallMsg{From: from, To: &to, Data: data}

	gas, err := c.EstimateGas(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("%s would fail: %w", label, DecodeError(err))
	}
	gas = gas*13/10 + 10_000

	nonce, err := c.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("%s: nonce: %w", label, err)
	}

	var raw []byte
	var hash common.Hash
	if t.legacy {
		price, err := c.SuggestGasPrice(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: gas price: %w", label, err)
		}
		raw, hash, err = SignLegacyTx(t.key.PrivateKey(), t.k.ChainID, nonce, price, gas, to, nil, data)
		if err != nil {
			return nil, err
		}
	} else {
		tip, err := c.SuggestGasTipCap(ctx)
		if err != nil || tip == nil {
			tip = big.NewInt(1_000_000) // 0.001 gwei; BASE priority fees are tiny
		}
		base, err := c.BaseFee(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: base fee: %w", label, err)
		}
		if base == nil {
			base = new(big.Int)
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
		raw, hash, err = SignDynamicFeeTx(t.key.PrivateKey(), t.k.ChainID, nonce, tip, feeCap, gas, to, nil, data)
		if err != nil {
			return nil, err
		}
	}

	if err := c.SendRawTransaction(ctx, raw); err != nil {
		return nil, fmt.Errorf("%s: send: %w", label, DecodeError(err))
	}
	t.log.Info("transaction sent", "action", label, "tx", hash.Hex())

	receipt, err := t.wait(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("%s (tx %s): %w", label, hash.Hex(), err)
	}
	if !receipt.Succeeded() {
		reason := ""
		if _, cerr := c.CallContract(ctx, msg); cerr != nil {
			reason = ": " + DecodeError(cerr).Error()
		}
		return receipt, fmt.Errorf("%s reverted (tx %s)%s", label, hash.Hex(), reason)
	}
	t.log.Info("transaction confirmed", "action", label, "tx", hash.Hex(), "gas", uint64(receipt.GasUsed))
	return receipt, nil
}

func (t *TxSender) wait(ctx context.Context, hash common.Hash) (*Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := t.k.C.TransactionReceipt(ctx, hash)
		if err == nil && r != nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("not mined after %s; it may still confirm", t.timeout)
		case <-tick.C:
		}
	}
}

// SignDynamicFeeTx RLP-encodes and signs an EIP-1559 (type 2) transaction.
func SignDynamicFeeTx(priv *ecdsa.PrivateKey, chainID *big.Int, nonce uint64, tip, feeCap *big.Int, gas uint64, to common.Address, value *big.Int, data []byte) ([]byte, common.Hash, error) {
	if value == nil {
		value = new(big.Int)
	}
	unsigned, err := rlp.EncodeToBytes([]any{chainID, nonce, tip, feeCap, gas, to, value, data, []any{}})
	if err != nil {
		return nil, common.Hash{}, err
	}
	sig, err := crypto.Sign(crypto.Keccak256(append([]byte{2}, unsigned...)), priv)
	if err != nil {
		return nil, common.Hash{}, err
	}
	r, s, v := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64]), uint64(sig[64])
	signed, err := rlp.EncodeToBytes([]any{chainID, nonce, tip, feeCap, gas, to, value, data, []any{}, v, r, s})
	if err != nil {
		return nil, common.Hash{}, err
	}
	raw := append([]byte{2}, signed...)
	return raw, crypto.Keccak256Hash(raw), nil
}

// SignLegacyTx RLP-encodes and signs an EIP-155 legacy transaction.
func SignLegacyTx(priv *ecdsa.PrivateKey, chainID *big.Int, nonce uint64, gasPrice *big.Int, gas uint64, to common.Address, value *big.Int, data []byte) ([]byte, common.Hash, error) {
	if value == nil {
		value = new(big.Int)
	}
	unsigned, err := rlp.EncodeToBytes([]any{nonce, gasPrice, gas, to, value, data, chainID, uint(0), uint(0)})
	if err != nil {
		return nil, common.Hash{}, err
	}
	sig, err := crypto.Sign(crypto.Keccak256(unsigned), priv)
	if err != nil {
		return nil, common.Hash{}, err
	}
	v := new(big.Int).Add(new(big.Int).Mul(chainID, big.NewInt(2)), big.NewInt(35+int64(sig[64])))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64])
	raw, err := rlp.EncodeToBytes([]any{nonce, gasPrice, gas, to, value, data, v, r, s})
	if err != nil {
		return nil, common.Hash{}, err
	}
	return raw, crypto.Keccak256Hash(raw), nil
}

// ---------------------------------------------------------------------------
// Revert decoding
// ---------------------------------------------------------------------------

// RevertError is a decoded contract revert.
type RevertError struct {
	Name string
	Args []any
	Hint string
}

func (e *RevertError) Error() string {
	s := "execution reverted: " + e.Name
	if len(e.Args) > 0 {
		s += fmt.Sprint(e.Args)
	}
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

var extraErrors = mustABI(`[
 {"type":"error","name":"ERC20InsufficientBalance","inputs":[{"name":"sender","type":"address"},{"name":"balance","type":"uint256"},{"name":"needed","type":"uint256"}]},
 {"type":"error","name":"ERC20InsufficientAllowance","inputs":[{"name":"spender","type":"address"},{"name":"allowance","type":"uint256"},{"name":"needed","type":"uint256"}]}
]`)

var revertHints = map[string]string{
	"ERC20InsufficientBalance":            "not enough MOR in the wallet",
	"ERC20InsufficientAllowance":          "MOR allowance for the marketplace is too low",
	"SessionTooShort":                     "stake buys less than the 5 minute minimum session",
	"SessionBidNotFound":                  "the provider withdrew this bid",
	"SessionProviderSignatureMismatch":    "provider approval was not signed by the bid's provider",
	"SessionDuplicateApproval":            "this provider approval was already used",
	"SesssionApproveExpired":              "provider approval expired; retry",
	"SessionAlreadyClosed":                "session is already closed",
	"SessionUserAmountToWithdrawIsZero":   "nothing to withdraw yet",
	"ProviderStakeTooLow":                 "provider stake is below the marketplace minimum",
	"ModelStakeTooLow":                    "model stake is below the marketplace minimum",
	"MarketplaceProviderNotFound":         "register as a provider first",
	"MarketplaceModelNotFound":            "model is not registered or was deregistered",
	"MarketplaceBidPricePerSecondInvalid": "price is outside the marketplace min/max",
	"InsufficientRightsForOperation":      "wallet is not allowed to act for this account",
	"ProviderHasActiveBids":               "delete the provider's bids first",
}

// DecodeError turns an RPC error carrying revert data into a RevertError.
func DecodeError(err error) error {
	var re *RPCError
	if !errors.As(err, &re) {
		return err
	}
	raw := re.RevertData()
	if len(raw) < 4 {
		return err
	}
	if reason, uerr := abi.UnpackRevert(raw); uerr == nil {
		return &RevertError{Name: reason}
	}
	for _, set := range []*abi.ABI{&DiamondABI, &extraErrors} {
		for _, e := range set.Errors {
			if !bytes.Equal(e.ID[:4], raw[:4]) {
				continue
			}
			args, _ := e.Inputs.Unpack(raw[4:])
			return &RevertError{Name: e.Name, Args: args, Hint: revertHints[e.Name]}
		}
	}
	return err
}
