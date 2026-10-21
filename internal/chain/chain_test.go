package chain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// Our minimal encoders must produce exactly what go-ethereum would.
func TestSignDynamicFeeTxMatchesGeth(t *testing.T) {
	priv, _ := crypto.GenerateKey()
	chainID := big.NewInt(8453)
	to := common.HexToAddress("0x6aBE1d282f72B474E54527D93b979A4f64d3030a")
	data := []byte{0xde, 0xad, 0xbe, 0xef}
	raw, hash, err := SignDynamicFeeTx(priv, chainID, 7, big.NewInt(1_000_000), big.NewInt(50_000_000), 123456, to, nil, data)
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("geth cannot decode our tx: %v", err)
	}
	if tx.Hash() != hash {
		t.Fatalf("hash mismatch: geth %s ours %s", tx.Hash(), hash)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(chainID), &tx)
	if err != nil || sender != crypto.PubkeyToAddress(priv.PublicKey) {
		t.Fatalf("sender %s (%v)", sender.Hex(), err)
	}
	if tx.Nonce() != 7 || tx.Gas() != 123456 || *tx.To() != to || string(tx.Data()) != string(data) || tx.ChainId().Cmp(chainID) != 0 {
		t.Fatal("decoded fields differ")
	}
	want, err := types.SignNewTx(priv, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: 7, GasTipCap: big.NewInt(1_000_000), GasFeeCap: big.NewInt(50_000_000), Gas: 123456, To: &to, Value: new(big.Int), Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want.Hash() != hash {
		t.Fatal("geth-signed transaction hash differs from ours")
	}
}

func TestSignLegacyTxMatchesGeth(t *testing.T) {
	priv, _ := crypto.GenerateKey()
	chainID := big.NewInt(84532)
	to := common.HexToAddress("0x5C80Ddd187054E1E4aBBfFCD750498e81d34FfA3")
	raw, hash, err := SignLegacyTx(priv, chainID, 0, big.NewInt(1_000_000_000), 21000, to, big.NewInt(5), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	if tx.Hash() != hash {
		t.Fatal("hash mismatch")
	}
	sender, err := types.Sender(types.NewEIP155Signer(chainID), &tx)
	if err != nil || sender != crypto.PubkeyToAddress(priv.PublicKey) {
		t.Fatalf("sender %s (%v)", sender.Hex(), err)
	}
}

func TestStructConversionMatchesABI(t *testing.T) {
	in := Session{
		User: common.HexToAddress("0x01"), BidId: [32]byte{2}, Stake: big.NewInt(3), CloseoutReceipt: []byte{4},
		CloseoutType: big.NewInt(5), ProviderWithdrawnAmount: big.NewInt(6), OpenedAt: big.NewInt(7),
		EndsAt: big.NewInt(8), ClosedAt: big.NewInt(9), IsActive: true, IsDirectPaymentFromUser: true,
	}
	packed, err := DiamondABI.Methods["getSession"].Outputs.Pack(in)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := DiamondABI.Unpack("getSession", packed)
	if err != nil {
		t.Fatal(err)
	}
	out := convert[Session](vals[0])
	if out.User != in.User || out.Stake.Int64() != 3 || out.EndsAt.Int64() != 8 || !out.IsDirectPaymentFromUser || out.CloseoutReceipt[0] != 4 {
		t.Fatalf("round trip mismatch: %+v", out)
	}

	stats := ProviderModelStats{TpsScaled1000: SD{Mean: 10, SqSum: 11}, TtftMs: SD{Mean: -1, SqSum: 2}, TotalDuration: 3, SuccessCount: 4, TotalCount: 5}
	packed, err = DiamondABI.Methods["getProviderModelStats"].Outputs.Pack(stats)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ = DiamondABI.Unpack("getProviderModelStats", packed)
	got := convert[ProviderModelStats](vals[0])
	if *got != stats {
		t.Fatalf("stats mismatch %+v", got)
	}
}

func rpcServer(t *testing.T, handle func(method string, params []json.RawMessage) (any, *RPCError, int)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, rerr, status := handle(req.Method, req.Params)
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("nope"))
			return
		}
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			out["error"] = rerr
		} else {
			out["result"] = res
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestClientFailoverAndRevertDecoding(t *testing.T) {
	var hits atomic.Int32
	limited := rpcServer(t, func(string, []json.RawMessage) (any, *RPCError, int) {
		hits.Add(1)
		return nil, nil, http.StatusTooManyRequests
	})
	defer limited.Close()
	tooShort := DiamondABI.Errors["SessionTooShort"].ID
	good := rpcServer(t, func(method string, _ []json.RawMessage) (any, *RPCError, int) {
		switch method {
		case "eth_chainId":
			return "0x2105", nil, 0
		case "eth_call":
			return nil, &RPCError{Code: 3, Message: "execution reverted", Data: hexutil.Encode(tooShort[:4])}, 0
		}
		return nil, &RPCError{Code: -32601, Message: "method not found"}, 0
	})
	defer good.Close()

	c, err := NewClient([]string{limited.URL, good.URL}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.ChainID(context.Background())
	if err != nil || id.Int64() != 8453 {
		t.Fatalf("chain id %v (%v)", id, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one attempt on the rate-limited endpoint, got %d", hits.Load())
	}
	// The client now prefers the healthy endpoint.
	if _, err := c.ChainID(context.Background()); err != nil || hits.Load() != 1 {
		t.Fatalf("did not stick to the healthy endpoint (hits=%d, err=%v)", hits.Load(), err)
	}

	k := NewContracts(c, common.HexToAddress("0x01"), common.HexToAddress("0x02"), big.NewInt(8453))
	_, err = k.GetSession(context.Background(), common.Hash{})
	var re *RevertError
	if !errors.As(err, &re) || re.Name != "SessionTooShort" || re.Hint == "" {
		t.Fatalf("expected decoded SessionTooShort, got %v", err)
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("https://base-mainnet.g.alchemy.com/v2/SECRETKEY"); got != "https://base-mainnet.g.alchemy.com/…" {
		t.Fatalf("redact leaked: %s", got)
	}
	if got := Redact("https://mainnet.base.org"); got != "https://mainnet.base.org" {
		t.Fatal(got)
	}
}

func TestIDHelpers(t *testing.T) {
	// keccak256(abi.encodePacked(address, bytes32, uint256)) computed independently.
	p := common.HexToAddress("0x1111111111111111111111111111111111111111")
	m := common.HexToHash("0x22")
	want := crypto.Keccak256Hash(append(append(p.Bytes(), m.Bytes()...), common.LeftPadBytes([]byte{3}, 32)...))
	if BidID(p, m, big.NewInt(3)) != want {
		t.Fatal("BidID mismatch")
	}
}
