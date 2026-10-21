// Package chain talks to BASE: a small failover JSON-RPC client, typed reads of
// the Morpheus Diamond marketplace contract, and a serialized transaction sender.
//
// It deliberately avoids go-ethereum's rpc/ethclient/core-types packages (which
// pull in tracing, metrics, websockets and KZG) to keep the binary small.
package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// RevertData returns ABI-encoded revert data attached to the error, if any.
func (e *RPCError) RevertData() []byte {
	s, ok := e.Data.(string)
	if !ok {
		return nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil
	}
	return b
}

// HTTPError is a non-200 response from an endpoint.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// CallMsg describes an eth_call / eth_estimateGas request.
type CallMsg struct {
	From  common.Address
	To    *common.Address
	Data  []byte
	Value *big.Int
}

func (m CallMsg) toArg() map[string]any {
	arg := map[string]any{"data": hexutil.Bytes(m.Data)}
	if m.From != (common.Address{}) {
		arg["from"] = m.From
	}
	if m.To != nil {
		arg["to"] = *m.To
	}
	if m.Value != nil && m.Value.Sign() > 0 {
		arg["value"] = (*hexutil.Big)(m.Value)
	}
	return arg
}

// Log is a contract event log.
type Log struct {
	Address common.Address `json:"address"`
	Topics  []common.Hash  `json:"topics"`
	Data    hexutil.Bytes  `json:"data"`
}

// Receipt is the subset of a transaction receipt the node uses.
type Receipt struct {
	TxHash      common.Hash    `json:"transactionHash"`
	Status      hexutil.Uint64 `json:"status"`
	BlockNumber *hexutil.Big   `json:"blockNumber"`
	GasUsed     hexutil.Uint64 `json:"gasUsed"`
	Logs        []Log          `json:"logs"`
}

// Succeeded reports a status-1 receipt.
func (r *Receipt) Succeeded() bool { return r.Status == 1 }

// Client is a JSON-RPC client that rotates across endpoints when one is down,
// rate-limited or refuses a method.
type Client struct {
	urls    []string
	http    *http.Client
	cur     atomic.Int32
	nextID  atomic.Uint64
	timeout time.Duration
	log     *slog.Logger
}

// NewClient prepares a client for the given endpoints.
func NewClient(urls []string, log *slog.Logger) (*Client, error) {
	if len(urls) == 0 {
		return nil, errors.New("no RPC endpoints configured")
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return nil, fmt.Errorf("RPC endpoint %q must be an http(s) URL", Redact(u))
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 8
	return &Client{
		urls:    urls,
		http:    &http.Client{Transport: tr},
		timeout: 20 * time.Second,
		log:     log,
	}, nil
}

// Endpoint returns the currently preferred endpoint (redacted).
func (c *Client) Endpoint() string { return Redact(c.urls[int(c.cur.Load())%len(c.urls)]) }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Call performs a JSON-RPC call with endpoint failover and decodes the result into out.
func (c *Client) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	start := int(c.cur.Load())
	var lastErr error
	for i := 0; i < len(c.urls); i++ {
		idx := (start + i) % len(c.urls)
		cctx, cancel := context.WithTimeout(ctx, c.timeout)
		err := c.callOne(cctx, c.urls[idx], out, method, params)
		cancel()
		if err == nil {
			if idx != start {
				c.cur.Store(int32(idx))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
		lastErr = err
		if len(c.urls) > 1 {
			c.log.Debug("rpc endpoint failed, trying next", "url", Redact(c.urls[idx]), "method", method, "err", err)
		}
	}
	if len(c.urls) == 1 {
		return lastErr
	}
	return fmt.Errorf("all %d RPC endpoints failed: %w", len(c.urls), lastErr)
}

func (c *Client) callOne(ctx context.Context, endpoint string, out any, method string, params []any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&r); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if r.Error != nil {
		return r.Error
	}
	if out == nil {
		return nil
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return errNullResult
	}
	return json.Unmarshal(r.Result, out)
}

var errNullResult = errors.New("null result")

// retryable reports whether err is an endpoint problem (rather than a
// deterministic answer such as a revert) worth retrying elsewhere.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == 429 || he.Status >= 500 || he.Status == 401 || he.Status == 403 ||
			he.Status == 404 || he.Status == 405 || he.Status == 410
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var re *RPCError
	if errors.As(err, &re) {
		if re.Code == 3 || re.RevertData() != nil {
			return false // contract revert: same answer everywhere
		}
		if re.Code == -32005 || re.Code == -32097 || re.Code == -32601 || re.Code == -32603 {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"execution reverted", "insufficient funds", "nonce too low", "already known", "underpriced"} {
		if strings.Contains(msg, s) {
			return false
		}
	}
	for _, s := range []string{"rate limit", "too many", "limit exceeded", "capacity", "timeout", "unavailable",
		"not allowed", "not supported", "unauthorized", "forbidden", "method not found", "busy", "eof", "connection"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Redact strips credentials and paths (API keys) from an RPC URL for display.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.Path != "" && u.Path != "/" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return u.Scheme + "://" + u.Host
}

func (c *Client) bigCall(ctx context.Context, method string, params ...any) (*big.Int, error) {
	var v hexutil.Big
	if err := c.Call(ctx, &v, method, params...); err != nil {
		return nil, err
	}
	return (*big.Int)(&v), nil
}

func (c *Client) ChainID(ctx context.Context) (*big.Int, error) { return c.bigCall(ctx, "eth_chainId") }

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var v hexutil.Uint64
	err := c.Call(ctx, &v, "eth_blockNumber")
	return uint64(v), err
}

func (c *Client) BalanceAt(ctx context.Context, addr common.Address) (*big.Int, error) {
	return c.bigCall(ctx, "eth_getBalance", addr, "latest")
}

func (c *Client) CodeAt(ctx context.Context, addr common.Address) ([]byte, error) {
	var v hexutil.Bytes
	err := c.Call(ctx, &v, "eth_getCode", addr, "latest")
	return v, err
}

func (c *Client) CallContract(ctx context.Context, msg CallMsg) ([]byte, error) {
	var v hexutil.Bytes
	err := c.Call(ctx, &v, "eth_call", msg.toArg(), "latest")
	return v, err
}

func (c *Client) EstimateGas(ctx context.Context, msg CallMsg) (uint64, error) {
	var v hexutil.Uint64
	err := c.Call(ctx, &v, "eth_estimateGas", msg.toArg())
	return uint64(v), err
}

func (c *Client) PendingNonceAt(ctx context.Context, addr common.Address) (uint64, error) {
	var v hexutil.Uint64
	err := c.Call(ctx, &v, "eth_getTransactionCount", addr, "pending")
	return uint64(v), err
}

func (c *Client) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return c.bigCall(ctx, "eth_maxPriorityFeePerGas")
}

func (c *Client) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return c.bigCall(ctx, "eth_gasPrice")
}

// BaseFee returns the latest block's base fee (nil on pre-London chains).
func (c *Client) BaseFee(ctx context.Context) (*big.Int, error) {
	var h struct {
		BaseFee *hexutil.Big `json:"baseFeePerGas"`
	}
	if err := c.Call(ctx, &h, "eth_getBlockByNumber", "latest", false); err != nil {
		return nil, err
	}
	if h.BaseFee == nil {
		return nil, nil
	}
	return (*big.Int)(h.BaseFee), nil
}

// SendRawTransaction broadcasts a signed transaction.
func (c *Client) SendRawTransaction(ctx context.Context, raw []byte) error {
	err := c.Call(ctx, nil, "eth_sendRawTransaction", hexutil.Bytes(raw))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already known") {
		return nil
	}
	return err
}

// TransactionReceipt returns (nil, nil) while the transaction is pending.
func (c *Client) TransactionReceipt(ctx context.Context, hash common.Hash) (*Receipt, error) {
	var r Receipt
	err := c.Call(ctx, &r, "eth_getTransactionReceipt", hash)
	if errors.Is(err, errNullResult) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
