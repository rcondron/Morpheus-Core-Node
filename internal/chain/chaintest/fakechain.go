// Package chaintest provides an in-memory imitation of BASE running the
// Morpheus Diamond, the MOR token and Multicall3, served over JSON-RPC. It is
// used by tests only and is never linked into the node binary.
package chaintest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

var (
	Diamond = common.HexToAddress("0x6aBE1d282f72B474E54527D93b979A4f64d3030a")
	MOR     = common.HexToAddress("0x7431aDa8a591C955a994a21710752EF9b882b8e3")
	ether   = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// MORs returns n whole MOR in wei.
func MORs(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), ether) }

// Chain is the fake chain state.
type Chain struct {
	mu      sync.Mutex
	ChainID *big.Int
	Server  *httptest.Server

	eth       map[common.Address]*big.Int
	mor       map[common.Address]*big.Int
	allowance map[[2]common.Address]*big.Int
	nonces    map[common.Address]uint64
	receipts  map[common.Hash]map[string]any
	block     uint64

	models       map[common.Hash]*chain.Model
	modelOrder   []common.Hash
	providers    map[common.Address]*chain.Provider
	bids         map[common.Hash]*chain.Bid
	bidOrder     []common.Hash
	bidNonces    map[[2]common.Hash]uint64
	sessions     map[common.Hash]*chain.Session
	userSessions map[common.Address][]common.Hash
	provSessions map[common.Address][]common.Hash
	usedApproval map[string]bool
	sessionNonce uint64

	Supply, Budget, BidFee, MinStake, ModelMinStake *big.Int
	MaxDuration                                     int64

	// Calls counts eth_call invocations per method name.
	Calls map[string]int
	// TxCount counts mined transactions per method name.
	TxCount map[string]int
}

// New starts a fake chain with marketplace economics where 1 MOR of stake
// buys exactly 1 MOR of compute (supply == budget).
func New(chainID int64) *Chain {
	c := &Chain{
		ChainID:   big.NewInt(chainID),
		eth:       map[common.Address]*big.Int{},
		mor:       map[common.Address]*big.Int{},
		allowance: map[[2]common.Address]*big.Int{},
		nonces:    map[common.Address]uint64{},
		receipts:  map[common.Hash]map[string]any{},
		block:     100,
		models:    map[common.Hash]*chain.Model{}, providers: map[common.Address]*chain.Provider{},
		bids: map[common.Hash]*chain.Bid{}, bidNonces: map[[2]common.Hash]uint64{},
		sessions: map[common.Hash]*chain.Session{}, userSessions: map[common.Address][]common.Hash{},
		provSessions: map[common.Address][]common.Hash{}, usedApproval: map[string]bool{},
		Supply: MORs(1_000_000), Budget: MORs(1_000_000), BidFee: new(big.Int).Div(MORs(3), big.NewInt(10)),
		MinStake: new(big.Int).Div(MORs(2), big.NewInt(10)), ModelMinStake: new(big.Int).Div(MORs(1), big.NewInt(10)),
		MaxDuration: 86400,
		Calls:       map[string]int{}, TxCount: map[string]int{},
	}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

// URL is the JSON-RPC endpoint.
func (c *Chain) URL() string { return c.Server.URL }

// Close stops the server.
func (c *Chain) Close() { c.Server.Close() }

// Fund credits ETH and MOR.
func (c *Chain) Fund(addr common.Address, ethWei, morWei *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eth[addr] = new(big.Int).Add(c.balance(c.eth, addr), ethWei)
	c.mor[addr] = new(big.Int).Add(c.balance(c.mor, addr), morWei)
}

// MORBalance returns an account's MOR.
func (c *Chain) MORBalance(addr common.Address) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.balance(c.mor, addr))
}

func (c *Chain) balance(m map[common.Address]*big.Int, a common.Address) *big.Int {
	if v := m[a]; v != nil {
		return v
	}
	return new(big.Int)
}

// AddModel registers a model directly (no transaction).
func (c *Chain) AddModel(name string, tags ...string) common.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := crypto.Keccak256Hash([]byte(name))
	c.models[id] = &chain.Model{Fee: new(big.Int), Stake: new(big.Int), Owner: common.HexToAddress("0x01"), Name: name, Tags: tags, CreatedAt: big.NewInt(time.Now().Unix()), ID: id}
	c.modelOrder = append(c.modelOrder, id)
	return id
}

// AddProvider registers a provider directly.
func (c *Chain) AddProvider(addr common.Address, endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.providers[addr] = &chain.Provider{Endpoint: endpoint, Stake: c.MinStake, CreatedAt: big.NewInt(time.Now().Unix()), LimitPeriodEnd: new(big.Int), LimitPeriodEarned: new(big.Int), Address: addr}
}

// AddBid posts a bid directly and returns its id.
func (c *Chain) AddBid(provider common.Address, model common.Hash, pricePerSecond *big.Int) common.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.postBid(provider, model, pricePerSecond)
}

func (c *Chain) postBid(provider common.Address, model common.Hash, price *big.Int) common.Hash {
	key := [2]common.Hash{common.BytesToHash(provider.Bytes()), model}
	nonce := c.bidNonces[key]
	c.bidNonces[key] = nonce + 1
	id := chain.BidID(provider, model, new(big.Int).SetUint64(nonce))
	if nonce > 0 {
		old := chain.BidID(provider, model, new(big.Int).SetUint64(nonce-1))
		if b := c.bids[old]; b != nil && b.Active() {
			b.DeletedAt = big.NewInt(time.Now().Unix())
		}
	}
	c.bids[id] = &chain.Bid{Provider: provider, ModelId: model, PricePerSecond: price, Nonce: new(big.Int).SetUint64(nonce), CreatedAt: big.NewInt(time.Now().Unix()), DeletedAt: new(big.Int), ID: id}
	c.bidOrder = append(c.bidOrder, id)
	return id
}

// Session returns a copy of an on-chain session.
func (c *Chain) Session(id common.Hash) *chain.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[id]
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// SetMaxDuration changes the longest session the marketplace grants, in
// seconds.
func (c *Chain) SetMaxDuration(sec int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MaxDuration = sec
}

// ExpireSession moves a session's end into the past.
func (c *Chain) ExpireSession(id common.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.sessions[id]; s != nil {
		s.EndsAt = big.NewInt(time.Now().Unix() - 1)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC
// ---------------------------------------------------------------------------

type revert struct{ data []byte }

func (r *revert) Error() string { return "execution reverted" }

func revertWith(name string, args ...any) error {
	for _, set := range []abi.ABI{chain.DiamondABI, extra} {
		if e, ok := set.Errors[name]; ok {
			data, _ := e.Inputs.Pack(args...)
			return &revert{data: append(append([]byte(nil), e.ID[:4]...), data...)}
		}
	}
	return &revert{data: abi.ABI{}.Methods["x"].ID}
}

var extra, _ = abi.JSON(strings.NewReader(`[
 {"type":"error","name":"ERC20InsufficientBalance","inputs":[{"name":"sender","type":"address"},{"name":"balance","type":"uint256"},{"name":"needed","type":"uint256"}]},
 {"type":"error","name":"ERC20InsufficientAllowance","inputs":[{"name":"spender","type":"address"},{"name":"allowance","type":"uint256"},{"name":"needed","type":"uint256"}]}
]`))

func (c *Chain) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := c.dispatch(req.Method, req.Params)
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	var rv *revert
	switch {
	case errors.As(err, &rv):
		out["error"] = map[string]any{"code": 3, "message": "execution reverted", "data": hexutil.Encode(rv.data)}
	case err != nil:
		out["error"] = map[string]any{"code": -32000, "message": err.Error()}
	default:
		out["result"] = res
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type callArg struct {
	From  *common.Address `json:"from"`
	To    *common.Address `json:"to"`
	Data  hexutil.Bytes   `json:"data"`
	Input hexutil.Bytes   `json:"input"`
}

func (c *Chain) dispatch(method string, params []json.RawMessage) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	str := func(i int) string {
		var s string
		if i < len(params) {
			_ = json.Unmarshal(params[i], &s)
		}
		return s
	}
	switch method {
	case "eth_chainId":
		return hexutil.EncodeBig(c.ChainID), nil
	case "eth_blockNumber":
		return hexutil.EncodeUint64(c.block), nil
	case "eth_getBalance":
		return hexutil.EncodeBig(c.balance(c.eth, common.HexToAddress(str(0)))), nil
	case "eth_getCode":
		return "0x", nil
	case "eth_gasPrice":
		return hexutil.EncodeBig(big.NewInt(1_000_000)), nil
	case "eth_maxPriorityFeePerGas":
		return hexutil.EncodeBig(big.NewInt(1_000)), nil
	case "eth_getBlockByNumber":
		return map[string]any{"number": hexutil.EncodeUint64(c.block), "baseFeePerGas": hexutil.EncodeBig(big.NewInt(5_000_000))}, nil
	case "eth_getTransactionCount":
		return hexutil.EncodeUint64(c.nonces[common.HexToAddress(str(0))]), nil
	case "eth_call", "eth_estimateGas":
		var arg callArg
		if err := json.Unmarshal(params[0], &arg); err != nil {
			return nil, err
		}
		data := arg.Data
		if len(data) == 0 {
			data = arg.Input
		}
		from := common.Address{}
		if arg.From != nil {
			from = *arg.From
		}
		if method == "eth_estimateGas" {
			snap := c.snapshot()
			_, _, err := c.exec(from, *arg.To, data, true)
			c.restore(snap)
			if err != nil {
				return nil, err
			}
			return hexutil.EncodeUint64(150_000), nil
		}
		out, err := c.call(*arg.To, data)
		if err != nil {
			return nil, err
		}
		return hexutil.Encode(out), nil
	case "eth_sendRawTransaction":
		return c.sendRaw(common.FromHex(str(0)))
	case "eth_getTransactionReceipt":
		if r, ok := c.receipts[common.HexToHash(str(0))]; ok {
			return r, nil
		}
		return nil, nil
	}
	return nil, fmt.Errorf("method %s not supported by fake chain", method)
}

func (c *Chain) sendRaw(raw []byte) (any, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, err
	}
	from, err := types.Sender(types.LatestSignerForChainID(c.ChainID), &tx)
	if err != nil {
		return nil, err
	}
	if tx.ChainId().Cmp(c.ChainID) != 0 {
		return nil, errors.New("wrong chain id")
	}
	if tx.Nonce() != c.nonces[from] {
		return nil, fmt.Errorf("nonce too low/high: have %d want %d", tx.Nonce(), c.nonces[from])
	}
	c.nonces[from]++
	c.block++
	logs, name, err := c.exec(from, *tx.To(), tx.Data(), false)
	status := "0x1"
	if err != nil {
		status = "0x0"
		logs = nil
	} else {
		c.TxCount[name]++
	}
	jl := []map[string]any{}
	for _, l := range logs {
		jl = append(jl, map[string]any{"address": l.Address, "topics": l.Topics, "data": hexutil.Encode(l.Data)})
	}
	c.receipts[tx.Hash()] = map[string]any{
		"transactionHash": tx.Hash(), "status": status, "blockNumber": hexutil.EncodeUint64(c.block),
		"gasUsed": hexutil.EncodeUint64(100_000), "logs": jl,
	}
	return tx.Hash().Hex(), nil
}

type snap struct {
	mor       map[common.Address]*big.Int
	allowance map[[2]common.Address]*big.Int
}

func (c *Chain) snapshot() snap {
	s := snap{mor: map[common.Address]*big.Int{}, allowance: map[[2]common.Address]*big.Int{}}
	for k, v := range c.mor {
		s.mor[k] = new(big.Int).Set(v)
	}
	for k, v := range c.allowance {
		s.allowance[k] = new(big.Int).Set(v)
	}
	return s
}

func (c *Chain) restore(s snap) { c.mor, c.allowance = s.mor, s.allowance }

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func lookup(a abi.ABI, data []byte) (*abi.Method, []any, error) {
	if len(data) < 4 {
		return nil, nil, errors.New("short calldata")
	}
	m, err := a.MethodById(data[:4])
	if err != nil {
		return nil, nil, err
	}
	args, err := m.Inputs.Unpack(data[4:])
	return m, args, err
}

func (c *Chain) call(to common.Address, data []byte) ([]byte, error) {
	switch to {
	case chain.Multicall3Address:
		return c.multicall(data)
	case MOR:
		m, args, err := lookup(chain.ERC20ABI, data)
		if err != nil {
			return nil, err
		}
		c.Calls[m.Name]++
		switch m.Name {
		case "balanceOf":
			return m.Outputs.Pack(c.balance(c.mor, args[0].(common.Address)))
		case "allowance":
			return m.Outputs.Pack(c.allowanceOf(args[0].(common.Address), args[1].(common.Address)))
		case "decimals":
			return m.Outputs.Pack(uint8(18))
		case "symbol":
			return m.Outputs.Pack("MOR")
		}
	case Diamond:
		m, args, err := lookup(chain.DiamondABI, data)
		if err != nil {
			return nil, err
		}
		c.Calls[m.Name]++
		out, err := c.view(m.Name, args)
		if err != nil {
			return nil, err
		}
		return m.Outputs.Pack(out...)
	}
	if bytes.Equal(to.Bytes(), common.Address{}.Bytes()) {
		return nil, errors.New("call to zero address")
	}
	return nil, nil
}

func (c *Chain) multicall(data []byte) ([]byte, error) {
	m, args, err := lookup(chain.MulticallABI, data)
	if err != nil {
		return nil, err
	}
	type call3 struct {
		Target       common.Address
		AllowFailure bool
		CallData     []byte
	}
	type result struct {
		Success    bool
		ReturnData []byte
	}
	calls := *abi.ConvertType(args[0], new([]call3)).(*[]call3)
	results := make([]result, len(calls))
	for i, cl := range calls {
		out, err := c.call(cl.Target, cl.CallData)
		if err != nil && !cl.AllowFailure {
			return nil, err
		}
		results[i] = result{Success: err == nil, ReturnData: out}
	}
	c.Calls["aggregate3"]++
	return m.Outputs.Pack(results)
}

func page(ids []common.Hash, offset, limit *big.Int) ([][32]byte, *big.Int) {
	total := big.NewInt(int64(len(ids)))
	out := [][32]byte{}
	start := int(offset.Int64())
	for i := start; i < len(ids) && i < start+int(limit.Int64()); i++ {
		out = append(out, ids[i])
	}
	return out, total
}

func (c *Chain) view(name string, a []any) ([]any, error) {
	switch name {
	case "getSession":
		s := c.sessions[common.Hash(a[0].([32]byte))]
		if s == nil {
			s = &chain.Session{BidId: [32]byte{}, Stake: new(big.Int), CloseoutReceipt: []byte{}, CloseoutType: new(big.Int), ProviderWithdrawnAmount: new(big.Int), OpenedAt: new(big.Int), EndsAt: new(big.Int), ClosedAt: new(big.Int)}
		}
		return []any{*s}, nil
	case "getBid":
		b := c.bids[common.Hash(a[0].([32]byte))]
		if b == nil {
			b = &chain.Bid{PricePerSecond: new(big.Int), Nonce: new(big.Int), CreatedAt: new(big.Int), DeletedAt: new(big.Int)}
		}
		return []any{*b}, nil
	case "getProvider":
		p := c.providers[a[0].(common.Address)]
		if p == nil {
			p = &chain.Provider{Stake: new(big.Int), CreatedAt: new(big.Int), LimitPeriodEnd: new(big.Int), LimitPeriodEarned: new(big.Int)}
		}
		return []any{*p}, nil
	case "getModel":
		m := c.models[common.Hash(a[0].([32]byte))]
		if m == nil {
			m = &chain.Model{Fee: new(big.Int), Stake: new(big.Int), Tags: []string{}, CreatedAt: new(big.Int)}
		}
		return []any{*m}, nil
	case "getModelStats":
		return []any{chain.ModelStats{}}, nil
	case "getProviderModelStats":
		return []any{chain.ProviderModelStats{}}, nil
	case "totalMORSupply":
		return []any{c.Supply}, nil
	case "getTodaysBudget":
		return []any{c.Budget}, nil
	case "getProviderMinimumStake":
		return []any{c.MinStake}, nil
	case "getModelMinimumStake":
		return []any{c.ModelMinStake}, nil
	case "getBidFee":
		return []any{c.BidFee}, nil
	case "getMaxSessionDuration":
		return []any{big.NewInt(c.MaxDuration)}, nil
	case "getMinMaxBidPricePerSecond":
		return []any{big.NewInt(1), MORs(1)}, nil
	case "getUserStakesOnHold":
		return []any{new(big.Int), new(big.Int)}, nil
	case "isRightsDelegated":
		return []any{false}, nil
	case "getIsProviderActive":
		p := c.providers[a[0].(common.Address)]
		return []any{p != nil && !p.IsDeleted}, nil
	case "getActiveModelIds":
		ids, total := page(c.modelOrder, a[0].(*big.Int), a[1].(*big.Int))
		return []any{ids, total}, nil
	case "getModelActiveBids", "getProviderActiveBids":
		var ids []common.Hash
		for _, id := range c.bidOrder {
			b := c.bids[id]
			if !b.Active() {
				continue
			}
			if name == "getModelActiveBids" && common.Hash(b.ModelId) == common.Hash(a[0].([32]byte)) {
				ids = append(ids, id)
			}
			if name == "getProviderActiveBids" && b.Provider == a[0].(common.Address) {
				ids = append(ids, id)
			}
		}
		out, total := page(ids, a[1].(*big.Int), a[2].(*big.Int))
		return []any{out, total}, nil
	case "getUserSessions":
		out, total := page(c.userSessions[a[0].(common.Address)], a[1].(*big.Int), a[2].(*big.Int))
		return []any{out, total}, nil
	case "getProviderSessions":
		out, total := page(c.provSessions[a[0].(common.Address)], a[1].(*big.Int), a[2].(*big.Int))
		return []any{out, total}, nil
	}
	return nil, fmt.Errorf("view %s not implemented in fake chain", name)
}

func (c *Chain) allowanceOf(owner, spender common.Address) *big.Int {
	if v := c.allowance[[2]common.Address{owner, spender}]; v != nil {
		return v
	}
	return new(big.Int)
}

// pull moves MOR from user to the Diamond, honoring allowance like an OZ ERC20.
func (c *Chain) pull(from common.Address, amount *big.Int) error {
	al := c.allowanceOf(from, Diamond)
	if al.Cmp(amount) < 0 {
		return revertWith("ERC20InsufficientAllowance", Diamond, al, amount)
	}
	bal := c.balance(c.mor, from)
	if bal.Cmp(amount) < 0 {
		return revertWith("ERC20InsufficientBalance", from, bal, amount)
	}
	c.allowance[[2]common.Address{from, Diamond}] = new(big.Int).Sub(al, amount)
	c.mor[from] = new(big.Int).Sub(bal, amount)
	c.mor[Diamond] = new(big.Int).Add(c.balance(c.mor, Diamond), amount)
	return nil
}

func (c *Chain) push(to common.Address, amount *big.Int) {
	c.mor[Diamond] = new(big.Int).Sub(c.balance(c.mor, Diamond), amount)
	c.mor[to] = new(big.Int).Add(c.balance(c.mor, to), amount)
}

// exec applies a transaction (or, with dry=true, only validates it).
func (c *Chain) exec(from, to common.Address, data []byte, dry bool) ([]*chain.Log, string, error) {
	if to == MOR {
		m, args, err := lookup(chain.ERC20ABI, data)
		if err != nil {
			return nil, "", err
		}
		if m.Name == "approve" {
			c.allowance[[2]common.Address{from, args[0].(common.Address)}] = new(big.Int).Set(args[1].(*big.Int))
			return nil, "approve", nil
		}
		return nil, m.Name, fmt.Errorf("token method %s not implemented", m.Name)
	}
	if to != Diamond {
		return nil, "", errors.New("unknown contract")
	}
	m, a, err := lookup(chain.DiamondABI, data)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().Unix()
	ev := chain.DiamondABI.Events
	switch m.Name {
	case "openSession":
		user, amount, direct := a[0].(common.Address), a[1].(*big.Int), a[2].(bool)
		approval, sig := a[3].([]byte), a[4].([]byte)
		if user != from {
			return nil, m.Name, revertWith("InsufficientRightsForOperation", user, from)
		}
		ap, err := morrpc.DecodeApproval(approval)
		if err != nil {
			return nil, m.Name, err
		}
		if ap.ChainID.Cmp(c.ChainID) != 0 {
			return nil, m.Name, revertWith("SesssionApprovedForAnotherChainId")
		}
		bid := c.bids[ap.BidID]
		if bid == nil || !bid.Active() {
			return nil, m.Name, revertWith("SessionBidNotFound")
		}
		signer, err := morrpc.RecoverEthMessage(approval, sig)
		if err != nil || signer != bid.Provider {
			return nil, m.Name, revertWith("SessionProviderSignatureMismatch")
		}
		if c.usedApproval[string(approval)] {
			return nil, m.Name, revertWith("SessionDuplicateApproval")
		}
		stipend := new(big.Int).Div(new(big.Int).Mul(amount, c.Budget), c.Supply)
		dur := new(big.Int).Div(stipend, bid.PricePerSecond).Int64()
		if dur > c.MaxDuration {
			dur = c.MaxDuration
		}
		if dur < 300 {
			return nil, m.Name, revertWith("SessionTooShort")
		}
		if err := c.pull(from, amount); err != nil {
			return nil, m.Name, err
		}
		if dry {
			return nil, m.Name, nil
		}
		c.usedApproval[string(approval)] = true
		id := crypto.Keccak256Hash(user.Bytes(), bid.Provider.Bytes(), ap.BidID.Bytes(), common.LeftPadBytes(new(big.Int).SetUint64(c.sessionNonce).Bytes(), 32))
		c.sessionNonce++
		c.sessions[id] = &chain.Session{User: user, BidId: ap.BidID, Stake: amount, CloseoutReceipt: []byte{}, CloseoutType: new(big.Int),
			ProviderWithdrawnAmount: new(big.Int), OpenedAt: big.NewInt(now), EndsAt: big.NewInt(now + dur), ClosedAt: new(big.Int),
			IsActive: true, IsDirectPaymentFromUser: direct, ID: id}
		c.userSessions[user] = append(c.userSessions[user], id)
		c.provSessions[bid.Provider] = append(c.provSessions[bid.Provider], id)
		e := ev["SessionOpened"]
		return []*chain.Log{{Address: Diamond, Topics: []common.Hash{e.ID, common.BytesToHash(user.Bytes()), id, common.BytesToHash(bid.Provider.Bytes())}}}, m.Name, nil

	case "closeSession":
		receipt := a[0].([]byte)
		rep, err := morrpc.DecodeReport(receipt)
		if err != nil {
			return nil, m.Name, err
		}
		s := c.sessions[rep.SessionID]
		if s == nil {
			return nil, m.Name, revertWith("SessionNotEndedOrNotExist")
		}
		if s.User != from {
			return nil, m.Name, revertWith("InsufficientRightsForOperation", s.User, from)
		}
		if s.Closed() {
			return nil, m.Name, revertWith("SessionAlreadyClosed")
		}
		if dry {
			return nil, m.Name, nil
		}
		s.ClosedAt, s.IsActive = big.NewInt(now), false
		s.CloseoutReceipt = receipt
		bid := c.bids[s.BidId]
		if signer, err := morrpc.RecoverEthMessage(receipt, a[1].([]byte)); err != nil || signer != bid.Provider {
			s.CloseoutType = big.NewInt(1) // dispute
		}
		c.push(s.User, s.Stake)
		e := ev["SessionClosed"]
		return []*chain.Log{{Address: Diamond, Topics: []common.Hash{e.ID, common.BytesToHash(s.User.Bytes()), rep.SessionID, common.BytesToHash(bid.Provider.Bytes())}}}, m.Name, nil

	case "withdrawUserStakes":
		return nil, m.Name, revertWith("SessionUserAmountToWithdrawIsZero")

	case "claimForProvider":
		return nil, m.Name, nil

	case "providerRegister":
		prov, amount, endpoint := a[0].(common.Address), a[1].(*big.Int), a[2].(string)
		if prov != from {
			return nil, m.Name, revertWith("InsufficientRightsForOperation", prov, from)
		}
		p := c.providers[prov]
		stake := new(big.Int).Set(amount)
		if p != nil {
			stake.Add(stake, p.Stake)
		}
		if stake.Cmp(c.MinStake) < 0 {
			return nil, m.Name, revertWith("ProviderStakeTooLow", stake, c.MinStake)
		}
		if err := c.pull(from, amount); err != nil {
			return nil, m.Name, err
		}
		if dry {
			return nil, m.Name, nil
		}
		c.providers[prov] = &chain.Provider{Endpoint: endpoint, Stake: stake, CreatedAt: big.NewInt(now), LimitPeriodEnd: new(big.Int), LimitPeriodEarned: new(big.Int), Address: prov}
		return nil, m.Name, nil

	case "postModelBid":
		prov, model, price := a[0].(common.Address), common.Hash(a[1].([32]byte)), a[2].(*big.Int)
		if c.providers[prov] == nil {
			return nil, m.Name, revertWith("MarketplaceProviderNotFound")
		}
		if c.models[model] == nil {
			return nil, m.Name, revertWith("MarketplaceModelNotFound")
		}
		if err := c.pull(from, c.BidFee); err != nil {
			return nil, m.Name, err
		}
		if dry {
			return nil, m.Name, nil
		}
		key := [2]common.Hash{common.BytesToHash(prov.Bytes()), model}
		nonce := c.bidNonces[key]
		c.postBid(prov, model, price)
		e := ev["MarketplaceBidPosted"]
		data, _ := e.Inputs.NonIndexed().Pack(new(big.Int).SetUint64(nonce))
		return []*chain.Log{{Address: Diamond, Topics: []common.Hash{e.ID, common.BytesToHash(prov.Bytes()), model}, Data: data}}, m.Name, nil

	case "deleteModelBid":
		b := c.bids[common.Hash(a[0].([32]byte))]
		if b == nil || !b.Active() {
			return nil, m.Name, revertWith("MarketplaceActiveBidNotFound")
		}
		if !dry {
			b.DeletedAt = big.NewInt(now)
		}
		return nil, m.Name, nil

	case "modelRegister":
		owner, base, stake, name, tags := a[0].(common.Address), common.Hash(a[1].([32]byte)), a[4].(*big.Int), a[5].(string), a[6].([]string)
		if stake.Cmp(c.ModelMinStake) < 0 {
			return nil, m.Name, revertWith("ModelStakeTooLow", stake, c.ModelMinStake)
		}
		if err := c.pull(from, stake); err != nil {
			return nil, m.Name, err
		}
		if dry {
			return nil, m.Name, nil
		}
		id := chain.ModelID(owner, base)
		if c.models[id] == nil {
			c.modelOrder = append(c.modelOrder, id)
		}
		c.models[id] = &chain.Model{IpfsCID: a[2].([32]byte), Fee: new(big.Int), Stake: stake, Owner: owner, Name: name, Tags: tags, CreatedAt: big.NewInt(now), ID: id}
		return nil, m.Name, nil
	}
	return nil, m.Name, fmt.Errorf("transaction %s not implemented in fake chain", m.Name)
}

// Hex is a small helper for tests.
func Hex(b []byte) string { return "0x" + hex.EncodeToString(b) }
