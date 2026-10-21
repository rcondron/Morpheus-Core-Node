package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ensureAllowanceLocked lets the Diamond pull at least need MOR, approving
// target (>= need) when the current allowance is short. It reports whether an
// approval transaction was sent.
func (t *TxSender) ensureAllowanceLocked(ctx context.Context, need, target *big.Int) (bool, error) {
	if need.Sign() == 0 {
		return false, nil
	}
	cur, err := t.k.Allowance(ctx, t.key.Address(), t.k.Diamond)
	if err != nil {
		return false, err
	}
	if cur.Cmp(need) >= 0 {
		return false, nil
	}
	if target == nil || target.Cmp(need) < 0 {
		target = need
	}
	data, err := ERC20ABI.Pack("approve", t.k.Diamond, target)
	if err != nil {
		return false, err
	}
	if _, err := t.SendLocked(ctx, t.k.MOR, data, "approve MOR"); err != nil {
		return true, err
	}
	return true, nil
}

// sendAfterApproval retries reverting simulations briefly: right after an
// approval, a load-balanced RPC may not have seen the new allowance yet.
func (t *TxSender) sendAfterApproval(ctx context.Context, approved bool, to common.Address, data []byte, label string) (*Receipt, error) {
	attempts := 1
	if approved {
		attempts = 4
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i*2) * time.Second):
			}
		}
		r, err := t.SendLocked(ctx, to, data, label)
		if err == nil {
			return r, nil
		}
		lastErr = err
		var re *RevertError
		if !errors.As(err, &re) || re.Name != "ERC20InsufficientAllowance" {
			return r, err
		}
	}
	return nil, lastErr
}

// OpenSession escrows stake and opens a session with a provider approval.
// allowanceTarget lets callers pre-approve headroom for future sessions.
func (t *TxSender) OpenSession(ctx context.Context, stake *big.Int, direct bool, approval, approvalSig []byte, allowanceTarget *big.Int) (common.Hash, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	approved, err := t.ensureAllowanceLocked(ctx, stake, allowanceTarget)
	if err != nil {
		return common.Hash{}, err
	}
	data, err := DiamondABI.Pack("openSession", t.key.Address(), stake, direct, approval, approvalSig)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.sendAfterApproval(ctx, approved, t.k.Diamond, data, "open session")
	if err != nil {
		return common.Hash{}, err
	}
	ev := DiamondABI.Events["SessionOpened"]
	for _, l := range r.Logs {
		if l.Address == t.k.Diamond && len(l.Topics) == 4 && l.Topics[0] == ev.ID {
			return l.Topics[2], nil
		}
	}
	return common.Hash{}, fmt.Errorf("session opened (tx %s) but no SessionOpened event found", r.TxHash.Hex())
}

// CloseSession submits a closeout receipt (provider- or user-signed).
func (t *TxSender) CloseSession(ctx context.Context, receipt, signature []byte) (common.Hash, error) {
	data, err := DiamondABI.Pack("closeSession", receipt, signature)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.Send(ctx, t.k.Diamond, data, "close session")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// WithdrawUserStakes claims MOR whose post-session hold period has elapsed.
func (t *TxSender) WithdrawUserStakes(ctx context.Context) (common.Hash, error) {
	data, err := DiamondABI.Pack("withdrawUserStakes", t.key.Address(), uint8(255))
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.Send(ctx, t.k.Diamond, data, "withdraw stakes")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// ClaimForProvider collects provider earnings from a session.
func (t *TxSender) ClaimForProvider(ctx context.Context, sessionID common.Hash) (common.Hash, error) {
	data, err := DiamondABI.Pack("claimForProvider", sessionID)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.Send(ctx, t.k.Diamond, data, "claim provider earnings")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// ProviderRegister registers (or tops up / updates the endpoint of) this wallet as a provider.
func (t *TxSender) ProviderRegister(ctx context.Context, stake *big.Int, endpoint string) (common.Hash, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	approved, err := t.ensureAllowanceLocked(ctx, stake, nil)
	if err != nil {
		return common.Hash{}, err
	}
	data, err := DiamondABI.Pack("providerRegister", t.key.Address(), stake, endpoint)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.sendAfterApproval(ctx, approved, t.k.Diamond, data, "register provider")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// ProviderDeregister removes this wallet's provider registration and returns its stake.
func (t *TxSender) ProviderDeregister(ctx context.Context) (common.Hash, error) {
	data, err := DiamondABI.Pack("providerDeregister", t.key.Address())
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.Send(ctx, t.k.Diamond, data, "deregister provider")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// PostModelBid offers a model at pricePerSecond (MOR wei); it replaces any
// previous bid by this provider for the same model. Returns the new bid id.
func (t *TxSender) PostModelBid(ctx context.Context, modelID common.Hash, pricePerSecond *big.Int) (common.Hash, error) {
	fee, err := t.k.BidFee(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	approved, err := t.ensureAllowanceLocked(ctx, fee, nil)
	if err != nil {
		return common.Hash{}, err
	}
	data, err := DiamondABI.Pack("postModelBid", t.key.Address(), modelID, pricePerSecond)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.sendAfterApproval(ctx, approved, t.k.Diamond, data, "post bid")
	if err != nil {
		return common.Hash{}, err
	}
	ev := DiamondABI.Events["MarketplaceBidPosted"]
	for _, l := range r.Logs {
		if l.Address == t.k.Diamond && len(l.Topics) == 3 && l.Topics[0] == ev.ID {
			vals, err := ev.Inputs.NonIndexed().Unpack(l.Data)
			if err != nil || len(vals) == 0 {
				break
			}
			return BidID(t.key.Address(), modelID, vals[0].(*big.Int)), nil
		}
	}
	return common.Hash{}, fmt.Errorf("bid posted (tx %s) but no MarketplaceBidPosted event found", r.TxHash.Hex())
}

// DeleteModelBid withdraws a bid.
func (t *TxSender) DeleteModelBid(ctx context.Context, bidID common.Hash) (common.Hash, error) {
	data, err := DiamondABI.Pack("deleteModelBid", bidID)
	if err != nil {
		return common.Hash{}, err
	}
	r, err := t.Send(ctx, t.k.Diamond, data, "delete bid")
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// ModelRegister registers a model; the on-chain id is keccak256(owner, baseModelID).
func (t *TxSender) ModelRegister(ctx context.Context, baseModelID, ipfsCID common.Hash, stake *big.Int, name string, tags []string) (common.Hash, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	approved, err := t.ensureAllowanceLocked(ctx, stake, nil)
	if err != nil {
		return common.Hash{}, err
	}
	if tags == nil {
		tags = []string{}
	}
	data, err := DiamondABI.Pack("modelRegister", t.key.Address(), baseModelID, ipfsCID, big.NewInt(0), stake, name, tags)
	if err != nil {
		return common.Hash{}, err
	}
	if _, err := t.sendAfterApproval(ctx, approved, t.k.Diamond, data, "register model"); err != nil {
		return common.Hash{}, err
	}
	return ModelID(t.key.Address(), baseModelID), nil
}

// BidID mirrors Marketplace.getBidId.
func BidID(provider common.Address, modelID common.Hash, nonce *big.Int) common.Hash {
	return crypto.Keccak256Hash(provider.Bytes(), modelID.Bytes(), common.LeftPadBytes(nonce.Bytes(), 32))
}

// ModelID mirrors ModelRegistry.getModelId.
func ModelID(owner common.Address, baseModelID common.Hash) common.Hash {
	return crypto.Keccak256Hash(owner.Bytes(), baseModelID.Bytes())
}
