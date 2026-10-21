// Command morcore-devnet runs a local, simulated Morpheus marketplace for
// development and demos: an in-memory BASE-like chain with the Diamond, MOR and
// Multicall3, one or more providers, and an echo model (or your own
// OpenAI-compatible backend). No real funds are involved.
//
//	go run ./cmd/morcore-devnet
//	morcore init --yes --network local --key-env DEVNET_KEY --password-env MORCORE_PASSWORD --api-key demo
//
// The devnet always funds Hardhat's well-known test account (devKey), so a
// node can connect without any setup; --fund adds more addresses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain/chaintest"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// devKey is Hardhat/Anvil's account #0. It is published everywhere, so it must
// never hold real funds; here it only exists on the simulated chain.
const devKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func main() {
	rpcAddr := flag.String("rpc", "127.0.0.1:8545", "JSON-RPC listen address")
	fund := flag.String("fund", "", "comma-separated extra addresses to fund with 1000 MOR and 1 ETH")
	backend := flag.String("backend", "", "OpenAI-compatible chat completions URL (default: built-in echo model)")
	backendModel := flag.String("backend-model", "echo", "model name sent to the backend")
	models := flag.String("models", "llama-3.3-70b,qwen3-235b", "comma-separated marketplace model names to list")
	verbose := flag.Bool("v", false, "verbose provider logs")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *verbose {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fc := chaintest.New(31337)
	fc.Server.Close() // re-serve on a fixed address
	ln, err := net.Listen("tcp", *rpcAddr)
	if err != nil {
		fail(err)
	}
	srv := &http.Server{Handler: fc.Server.Config.Handler}
	go func() { _ = srv.Serve(ln) }()

	backendURL := *backend
	if backendURL == "" {
		echo := httptest.NewServer(http.HandlerFunc(echoModel))
		defer echo.Close()
		backendURL = echo.URL + "/v1/chat/completions"
	}

	client, _ := chain.NewClient([]string{"http://" + *rpcAddr}, log)
	k := chain.NewContracts(client, chaintest.Diamond, chaintest.MOR, big.NewInt(31337))

	var names []string
	for _, n := range strings.Split(*models, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	prices := []int64{2_000_000_000_000_000, 1_000_000_000_000_000, 500_000_000_000_000}
	fmt.Println("Morpheus devnet")
	fmt.Printf("  RPC          http://%s  (chain 31337)\n", *rpcAddr)
	fmt.Printf("  Backend      %s\n", backendURL)
	ids := make([]common.Hash, len(names))
	for i, name := range names {
		ids[i] = fc.AddModel(name, "LLM")
		fmt.Printf("  Model        %-20s %s\n", name, ids[i].Hex())
	}
	for pi := 0; pi < 2; pi++ {
		pw, _ := wallet.Generate()
		pln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fail(err)
		}
		fc.AddProvider(pw.Address(), pln.Addr().String())
		var pmodels []provider.Model
		for i, name := range names {
			pmodels = append(pmodels, provider.Model{ID: ids[i], Name: name, APIURL: backendURL, Backend: *backendModel})
			fc.AddBid(pw.Address(), ids[i], big.NewInt(prices[(i+pi)%len(prices)]))
		}
		p := provider.New(provider.Options{Models: pmodels, Version: "devnet"}, pw, k, chain.NewTxSender(k, pw, false, log), events.New(), log)
		go func() { _ = (&morrpc.Server{Handler: p.Handle, Log: log}).Serve(ctx, pln) }()
		fmt.Printf("  Provider %d   %s at %s\n", pi+1, pw.Address().Hex(), pln.Addr())
	}
	dev, _ := wallet.FromHex(devKey)
	for _, a := range append([]string{dev.Address().Hex()}, strings.Split(*fund, ",")...) {
		if a = strings.TrimSpace(a); common.IsHexAddress(a) {
			fc.Fund(common.HexToAddress(a), chaintest.MORs(1), chaintest.MORs(1000))
			fmt.Printf("  Funded       %s with 1000 MOR + 1 ETH\n", a)
		}
	}
	fmt.Println("\nConnect a node (devnet only: this is Hardhat's public test key):")
	fmt.Println("  export MORCORE_HOME=$(mktemp -d) MORCORE_PASSWORD=devnet-password")
	fmt.Println("  export DEVNET_KEY=" + devKey)
	fmt.Println("  morcore init --yes --network local --key-env DEVNET_KEY --password-env MORCORE_PASSWORD --api-key demo")
	fmt.Println("  morcore start")
	fmt.Println("\nCtrl+C to stop.")
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "devnet:", err)
	os.Exit(1)
}

// echoModel is a tiny OpenAI-compatible model that repeats the last message.
func echoModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"echo"}]}`)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Input any `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"echo","usage":{"prompt_tokens":1,"total_tokens":1}}`)
		return
	}
	last := ""
	if n := len(req.Messages); n > 0 {
		last = req.Messages[n-1].Content
	}
	answer := "You said: " + last
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "devnet", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": len(last) / 4, "completion_tokens": len(answer) / 4, "total_tokens": (len(last) + len(answer)) / 4},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for i, word := range strings.Fields(answer) {
		if i > 0 {
			word = " " + word
		}
		b, _ := json.Marshal(map[string]any{"id": "devnet", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": word}}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
