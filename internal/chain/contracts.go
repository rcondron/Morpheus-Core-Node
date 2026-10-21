package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Delegation rule identifiers used by the Diamond.
var (
	RulesProvider    = crypto.Keccak256Hash([]byte("delegation.rules.provider"))
	RulesMarketplace = crypto.Keccak256Hash([]byte("delegation.rules.marketplace"))
	RulesSession     = crypto.Keccak256Hash([]byte("delegation.rules.session"))
)

// Contracts provides typed access to the Diamond marketplace and MOR token.
type Contracts struct {
	C       *Client
	Diamond common.Address
	MOR     common.Address
	ChainID *big.Int

	noMulticall atomic.Bool
}

func NewContracts(c *Client, diamond, mor common.Address, chainID *big.Int) *Contracts {
	return &Contracts{C: c, Diamond: diamond, MOR: mor, ChainID: chainID}
}

func (k *Contracts) call(ctx context.Context, to common.Address, a *abi.ABI, method string, args ...any) ([]any, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	out, err := k.C.CallContract(ctx, CallMsg{To: &to, Data: data})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, DecodeError(err))
	}
	vals, err := a.Unpack(method, out)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	return vals, nil
}

func (k *Contracts) diamond(ctx context.Context, method string, args ...any) ([]any, error) {
	return k.call(ctx, k.Diamond, &DiamondABI, method, args...)
}

func convert[T any](v any) *T {
	return abi.ConvertType(v, new(T)).(*T)
}

func (k *Contracts) GetSession(ctx context.Context, id common.Hash) (*Session, error) {
	out, err := k.diamond(ctx, "getSession", id)
	if err != nil {
		return nil, err
	}
	s := convert[Session](out[0])
	s.ID = id
	if !s.Exists() {
		return nil, fmt.Errorf("session %s not found", id.Hex())
	}
	return s, nil
}

func (k *Contracts) GetBid(ctx context.Context, id common.Hash) (*Bid, error) {
	out, err := k.diamond(ctx, "getBid", id)
	if err != nil {
		return nil, err
	}
	b := convert[Bid](out[0])
	b.ID = id
	if b.Provider == (common.Address{}) {
		return nil, fmt.Errorf("bid %s not found", id.Hex())
	}
	return b, nil
}

func (k *Contracts) GetProvider(ctx context.Context, addr common.Address) (*Provider, error) {
	out, err := k.diamond(ctx, "getProvider", addr)
	if err != nil {
		return nil, err
	}
	p := convert[Provider](out[0])
	p.Address = addr
	return p, nil
}

func (k *Contracts) GetModel(ctx context.Context, id common.Hash) (*Model, error) {
	out, err := k.diamond(ctx, "getModel", id)
	if err != nil {
		return nil, err
	}
	m := convert[Model](out[0])
	m.ID = id
	return m, nil
}

func (k *Contracts) GetModelStats(ctx context.Context, id common.Hash) (*ModelStats, error) {
	out, err := k.diamond(ctx, "getModelStats", id)
	if err != nil {
		return nil, err
	}
	return convert[ModelStats](out[0]), nil
}

func (k *Contracts) bigCall(ctx context.Context, method string, args ...any) (*big.Int, error) {
	out, err := k.diamond(ctx, method, args...)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func now128() *big.Int { return big.NewInt(time.Now().Unix()) }

// TotalMORSupply is the supply figure used to convert stake into stipend.
func (k *Contracts) TotalMORSupply(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "totalMORSupply", now128())
}

// TodaysBudget is today's compute emission budget.
func (k *Contracts) TodaysBudget(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "getTodaysBudget", now128())
}

func (k *Contracts) ProviderMinimumStake(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "getProviderMinimumStake")
}

func (k *Contracts) ModelMinimumStake(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "getModelMinimumStake")
}

func (k *Contracts) BidFee(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "getBidFee")
}

func (k *Contracts) MaxSessionDuration(ctx context.Context) (*big.Int, error) {
	return k.bigCall(ctx, "getMaxSessionDuration")
}

// BidPriceBounds returns the marketplace min/max price per second.
func (k *Contracts) BidPriceBounds(ctx context.Context) (min, max *big.Int, err error) {
	out, err := k.diamond(ctx, "getMinMaxBidPricePerSecond")
	if err != nil {
		return nil, nil, err
	}
	return out[0].(*big.Int), out[1].(*big.Int), nil
}

// UserStakesOnHold returns MOR released from closed sessions that is
// withdrawable now (available) or still time-locked (hold).
func (k *Contracts) UserStakesOnHold(ctx context.Context, user common.Address) (available, hold *big.Int, err error) {
	out, err := k.diamond(ctx, "getUserStakesOnHold", user, uint8(255))
	if err != nil {
		return nil, nil, err
	}
	return out[0].(*big.Int), out[1].(*big.Int), nil
}

func (k *Contracts) IsRightsDelegated(ctx context.Context, delegatee, delegator common.Address, rights common.Hash) (bool, error) {
	out, err := k.diamond(ctx, "isRightsDelegated", delegatee, delegator, rights)
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

func (k *Contracts) IsProviderActive(ctx context.Context, addr common.Address) (bool, error) {
	out, err := k.diamond(ctx, "getIsProviderActive", addr)
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

func (k *Contracts) MORBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	out, err := k.call(ctx, k.MOR, &ERC20ABI, "balanceOf", addr)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func (k *Contracts) Allowance(ctx context.Context, owner, spender common.Address) (*big.Int, error) {
	out, err := k.call(ctx, k.MOR, &ERC20ABI, "allowance", owner, spender)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// ---------------------------------------------------------------------------
// Paginated id listings
// ---------------------------------------------------------------------------

const pageSize = 100

// pagedIDs walks a (offset, limit) -> (ids, total) getter.
func (k *Contracts) pagedIDs(ctx context.Context, method string, prefix ...any) ([]common.Hash, error) {
	var all []common.Hash
	for offset := int64(0); ; offset += pageSize {
		args := append(append([]any(nil), prefix...), big.NewInt(offset), big.NewInt(pageSize))
		out, err := k.diamond(ctx, method, args...)
		if err != nil {
			return nil, err
		}
		ids := out[0].([][32]byte)
		total := out[1].(*big.Int).Int64()
		for _, id := range ids {
			all = append(all, common.Hash(id))
		}
		if len(ids) < pageSize || int64(len(all)) >= total {
			return all, nil
		}
	}
}

// lastIDs returns the most recent n ids of an append-only (offset, limit) set.
func (k *Contracts) lastIDs(ctx context.Context, method string, n int, prefix ...any) ([]common.Hash, error) {
	args := append(append([]any(nil), prefix...), big.NewInt(0), big.NewInt(0))
	out, err := k.diamond(ctx, method, args...)
	if err != nil {
		return nil, err
	}
	total := out[1].(*big.Int).Int64()
	offset := total - int64(n)
	if offset < 0 {
		offset = 0
	}
	args = append(append([]any(nil), prefix...), big.NewInt(offset), big.NewInt(int64(n)))
	out, err = k.diamond(ctx, method, args...)
	if err != nil {
		return nil, err
	}
	ids := out[0].([][32]byte)
	res := make([]common.Hash, len(ids))
	for i, id := range ids {
		res[len(ids)-1-i] = common.Hash(id) // newest first
	}
	return res, nil
}

func (k *Contracts) ActiveModelIDs(ctx context.Context) ([]common.Hash, error) {
	return k.pagedIDs(ctx, "getActiveModelIds")
}

func (k *Contracts) ModelActiveBidIDs(ctx context.Context, modelID common.Hash) ([]common.Hash, error) {
	return k.pagedIDs(ctx, "getModelActiveBids", modelID)
}

func (k *Contracts) ProviderActiveBidIDs(ctx context.Context, provider common.Address) ([]common.Hash, error) {
	return k.pagedIDs(ctx, "getProviderActiveBids", provider)
}

func (k *Contracts) RecentUserSessionIDs(ctx context.Context, user common.Address, n int) ([]common.Hash, error) {
	return k.lastIDs(ctx, "getUserSessions", n, user)
}

func (k *Contracts) RecentProviderSessionIDs(ctx context.Context, provider common.Address, n int) ([]common.Hash, error) {
	return k.lastIDs(ctx, "getProviderSessions", n, provider)
}

// ---------------------------------------------------------------------------
// Batched reads (Multicall3 with per-call fallback)
// ---------------------------------------------------------------------------

type mcCall struct {
	Target       common.Address
	AllowFailure bool
	CallData     []byte
}

type mcResult struct {
	Success    bool
	ReturnData []byte
}

// batch runs method(args[i]...) for every i against the Diamond and returns
// the unpacked first output of each call.
func (k *Contracts) batch(ctx context.Context, method string, args [][]any) ([]any, error) {
	out := make([]any, len(args))
	if len(args) == 0 {
		return out, nil
	}
	if !k.noMulticall.Load() {
		err := k.multicall(ctx, method, args, out)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		k.noMulticall.Store(true) // endpoint likely rejects large calls; degrade gracefully
	}
	for i, a := range args {
		vals, err := k.diamond(ctx, method, a...)
		if err != nil {
			return nil, err
		}
		out[i] = vals[0]
	}
	return out, nil
}

func (k *Contracts) multicall(ctx context.Context, method string, args [][]any, out []any) error {
	const chunk = 100
	for start := 0; start < len(args); start += chunk {
		end := min(start+chunk, len(args))
		calls := make([]mcCall, 0, end-start)
		for _, a := range args[start:end] {
			data, err := DiamondABI.Pack(method, a...)
			if err != nil {
				return err
			}
			calls = append(calls, mcCall{Target: k.Diamond, AllowFailure: false, CallData: data})
		}
		res, err := k.call(ctx, Multicall3Address, &MulticallABI, "aggregate3", calls)
		if err != nil {
			return err
		}
		results := *abi.ConvertType(res[0], new([]mcResult)).(*[]mcResult)
		if len(results) != len(calls) {
			return errors.New("multicall result length mismatch")
		}
		for i, r := range results {
			if !r.Success {
				return fmt.Errorf("multicall %s failed", method)
			}
			vals, err := DiamondABI.Unpack(method, r.ReturnData)
			if err != nil {
				return err
			}
			out[start+i] = vals[0]
		}
	}
	return nil
}

func (k *Contracts) GetBids(ctx context.Context, ids []common.Hash) ([]*Bid, error) {
	args := make([][]any, len(ids))
	for i, id := range ids {
		args[i] = []any{id}
	}
	raw, err := k.batch(ctx, "getBid", args)
	if err != nil {
		return nil, err
	}
	out := make([]*Bid, len(raw))
	for i, r := range raw {
		out[i] = convert[Bid](r)
		out[i].ID = ids[i]
	}
	return out, nil
}

func (k *Contracts) GetProviders(ctx context.Context, addrs []common.Address) ([]*Provider, error) {
	args := make([][]any, len(addrs))
	for i, a := range addrs {
		args[i] = []any{a}
	}
	raw, err := k.batch(ctx, "getProvider", args)
	if err != nil {
		return nil, err
	}
	out := make([]*Provider, len(raw))
	for i, r := range raw {
		out[i] = convert[Provider](r)
		out[i].Address = addrs[i]
	}
	return out, nil
}

func (k *Contracts) GetModels(ctx context.Context, ids []common.Hash) ([]*Model, error) {
	args := make([][]any, len(ids))
	for i, id := range ids {
		args[i] = []any{id}
	}
	raw, err := k.batch(ctx, "getModel", args)
	if err != nil {
		return nil, err
	}
	out := make([]*Model, len(raw))
	for i, r := range raw {
		out[i] = convert[Model](r)
		out[i].ID = ids[i]
	}
	return out, nil
}

func (k *Contracts) GetSessions(ctx context.Context, ids []common.Hash) ([]*Session, error) {
	args := make([][]any, len(ids))
	for i, id := range ids {
		args[i] = []any{id}
	}
	raw, err := k.batch(ctx, "getSession", args)
	if err != nil {
		return nil, err
	}
	out := make([]*Session, len(raw))
	for i, r := range raw {
		out[i] = convert[Session](r)
		out[i].ID = ids[i]
	}
	return out, nil
}

func (k *Contracts) GetProviderModelStats(ctx context.Context, modelID common.Hash, providers []common.Address) ([]*ProviderModelStats, error) {
	args := make([][]any, len(providers))
	for i, p := range providers {
		args[i] = []any{modelID, p}
	}
	raw, err := k.batch(ctx, "getProviderModelStats", args)
	if err != nil {
		return nil, err
	}
	out := make([]*ProviderModelStats, len(raw))
	for i, r := range raw {
		out[i] = convert[ProviderModelStats](r)
	}
	return out, nil
}
