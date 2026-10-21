// Package node assembles the routing node from its parts and runs it.
package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/buildinfo"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/gateway"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// Node is a running Morpheus Core Node.
type Node struct {
	Cfg      *config.Config
	Wallet   *wallet.Wallet
	Client   *chain.Client
	K        *chain.Contracts
	Tx       *chain.TxSender
	Market   *marketplace.Market
	Sessions *consumer.Manager
	Provider *provider.Provider
	Keys     *apikeys.Store
	Bus      *events.Bus
	Log      *slog.Logger

	AdminToken string
	Started    time.Time

	GatewayAddr  string
	ProviderAddr string

	balMu   sync.Mutex
	eth     *big.Int
	mor     *big.Int
	balAt   time.Time
	blockAt uint64
}

// New wires the node's components (no network activity yet).
func New(cfg *config.Config, w *wallet.Wallet, log *slog.Logger) (*Node, error) {
	nw := cfg.NetworkInfo()
	client, err := chain.NewClient(cfg.Chain.RPCURLs, log)
	if err != nil {
		return nil, err
	}
	k := chain.NewContracts(client, common.HexToAddress(nw.DiamondAddress), common.HexToAddress(nw.MORTokenAddress), big.NewInt(nw.ChainID))
	tx := chain.NewTxSender(k, w, cfg.Chain.LegacyTx, log)
	bus := events.New()
	n := &Node{
		Cfg: cfg, Wallet: w, Client: client, K: k, Tx: tx, Bus: bus, Log: log,
		Market: marketplace.New(k, cfg.Sessions.ProviderAllowlist, cfg.Sessions.ProviderDenylist, log),
	}
	keys, err := apikeys.Open(cfg.Path(config.APIKeysFile), log)
	if err != nil {
		return nil, fmt.Errorf("open API keys: %w", err)
	}
	n.Keys = keys
	if n.AdminToken, err = EnsureAdminToken(cfg.Home); err != nil {
		return nil, err
	}

	if cfg.ConsumerEnabled() {
		maxStake, _ := units.ParseToken(cfg.Sessions.MaxStake)
		s := cfg.Sessions
		n.Sessions = consumer.New(consumer.Options{
			Duration: s.Duration.Duration, MaxPerModel: s.MaxPerModel, MaxTotal: s.MaxTotal, MaxStake: maxStake,
			IdleTimeout: s.IdleTimeout.Duration, RenewBefore: s.RenewBefore.Duration, Direct: s.DirectPayment,
			ProviderTimeout: s.ProviderTimeout.Duration, QueueTimeout: s.QueueTimeout.Duration, HealthPolicy: s.HealthPolicy,
			CloseOnShutdown: s.CloseOnShutdown, AutoWithdraw: s.AutoWithdraw, StorePath: cfg.Path(config.SessionsFile),
		}, w, k, tx, n.Market, morrpc.DefaultClient(), bus, log.With("component", "sessions"))
	}
	if cfg.ProviderEnabled() {
		var models []provider.Model
		for _, m := range cfg.Provider.Models {
			models = append(models, provider.Model{
				ID: common.HexToHash(m.ModelID), Name: m.Name, APIURL: m.APIURL, APIKey: m.APIKey,
				Backend: m.BackendModel, MaxSessions: m.MaxSessions,
			})
		}
		n.Provider = provider.New(provider.Options{
			Models: models, StatePath: cfg.Path(config.ProviderFile), Version: buildinfo.ProtocolVersion(), AutoClaim: true,
		}, w, k, tx, bus, log.With("component", "provider"))
	}
	return n, nil
}

// EnsureAdminToken loads or creates the local admin token used by the CLI.
func EnsureAdminToken(home string) (string, error) {
	path := home + string(os.PathSeparator) + config.AdminTokenFile
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := "mca_" + hex.EncodeToString(buf)
	if err := store.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// LockInfo describes a running node; the CLI uses it to reach the admin API.
type LockInfo struct {
	PID     int       `json:"pid"`
	Gateway string    `json:"gateway"`
	Started time.Time `json:"started"`
}

// ReadLock returns the running node's lock info, if any.
func ReadLock(home string) (*LockInfo, bool) {
	var li LockInfo
	ok, err := store.ReadJSON(home+string(os.PathSeparator)+config.LockFile, &li)
	if err != nil || !ok {
		return nil, false
	}
	return &li, true
}

// Alive reports whether a node answers on addr.
func Alive(addr string) bool {
	if addr == "" {
		return false
	}
	c := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ErrAlreadyRunning is returned when another node uses the same home.
var ErrAlreadyRunning = errors.New("a node is already running from this home directory")

// Run starts every enabled service and blocks until ctx is cancelled, then
// shuts down gracefully (closing sessions if configured). ready is called
// once all listeners are up.
func (n *Node) Run(ctx context.Context, ready func()) error {
	if li, ok := ReadLock(n.Cfg.Home); ok && Alive(li.Gateway) {
		return fmt.Errorf("%w (pid %d, %s)", ErrAlreadyRunning, li.PID, li.Gateway)
	}
	n.Started = time.Now()
	nw := n.Cfg.NetworkInfo()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	id, err := n.Client.ChainID(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot reach the %s RPC endpoint (%s): %w", nw.Label, n.Client.Endpoint(), err)
	}
	if id.Int64() != nw.ChainID {
		return fmt.Errorf("RPC endpoint is on chain %d but %s is chain %d; check chain.rpc_urls", id.Int64(), nw.Label, nw.ChainID)
	}

	var gwLn, provLn net.Listener
	if n.Cfg.Gateway.Enabled {
		if gwLn, err = listen(n.Cfg.Gateway.Listen); err != nil {
			return fmt.Errorf("agent API: %w", err)
		}
		n.GatewayAddr = gwLn.Addr().String()
	}
	if n.Provider != nil {
		if provLn, err = listen(n.Cfg.Provider.Listen); err != nil {
			if gwLn != nil {
				gwLn.Close()
			}
			return fmt.Errorf("provider port: %w", err)
		}
		n.ProviderAddr = provLn.Addr().String()
	}
	lockPath := n.Cfg.Path(config.LockFile)
	_ = store.WriteJSON(lockPath, LockInfo{PID: os.Getpid(), Gateway: n.GatewayAddr, Started: n.Started}, 0o600)
	defer os.Remove(lockPath)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	errc := make(chan error, 4)

	if gwLn != nil {
		gw := gateway.New(gateway.Deps{
			Market: n.Market, Sessions: n.Sessions, Keys: n.Keys, Bus: n.Bus, AdminToken: n.AdminToken,
			Status: func(ctx context.Context) any { return n.Status(ctx) }, CORS: n.Cfg.Gateway.CORSOrigins,
			Log: n.Log.With("component", "api"),
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gw.Serve(runCtx, gwLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("agent API: %w", err)
			}
		}()
		n.Log.Info("agent API listening", "url", "http://"+n.GatewayAddr+"/v1")
	}
	if provLn != nil {
		srv := &morrpc.Server{Handler: n.Provider.Handle, Log: n.Log.With("component", "mor-rpc")}
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := srv.Serve(runCtx, provLn); err != nil {
				errc <- fmt.Errorf("provider server: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			n.Provider.Run(runCtx)
		}()
		n.Log.Info("provider listening", "addr", n.ProviderAddr, "public", n.Cfg.Provider.PublicEndpoint)
	}
	if n.Sessions != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(runCtx, 2*time.Minute)
			if err := n.Sessions.Reconcile(rctx); err != nil {
				n.Log.Warn("could not reconcile sessions", "err", err)
			}
			cancel()
			n.Sessions.Run(runCtx)
		}()
		go func() {
			if _, err := n.Market.Models(runCtx); err != nil {
				n.Log.Warn("could not load the model catalog yet", "err", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		n.background(runCtx)
	}()

	n.Bus.Emit(events.Info, "node", fmt.Sprintf("node started on %s", nw.Label))
	if ready != nil {
		ready()
	}

	select {
	case <-ctx.Done():
	case err = <-errc:
		n.Log.Error("service failed", "err", err)
	}
	n.Log.Info("shutting down")
	if n.Sessions != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		n.Sessions.Shutdown(sctx)
		cancel()
	}
	stop()
	wg.Wait()
	if ferr := n.Keys.Flush(); ferr != nil {
		n.Log.Warn("could not save key usage", "err", ferr)
	}
	n.Client.Close()
	return err
}

func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil && strings.Contains(err.Error(), "address already in use") {
		return nil, fmt.Errorf("%s is already in use (another node or app?) — change the port in config.toml", addr)
	}
	return ln, err
}

func (n *Node) background(ctx context.Context) {
	n.refreshBalances(ctx)
	flush := time.NewTicker(15 * time.Second)
	bal := time.NewTicker(time.Minute)
	defer flush.Stop()
	defer bal.Stop()
	evs, unsubscribe := n.Bus.Subscribe()
	defer unsubscribe()
	// Session opens/closes move MOR; refresh shortly after so displays track them.
	var soon <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if err := n.Keys.Flush(); err != nil {
				n.Log.Warn("could not save key usage", "err", err)
			}
		case <-bal.C:
			n.refreshBalances(ctx)
		case v := <-evs:
			if e, ok := v.(events.Event); ok && (e.Kind == "session" || e.Kind == "provider") && soon == nil {
				soon = time.After(3 * time.Second)
			}
		case <-soon:
			soon = nil
			n.refreshBalances(ctx)
		}
	}
}

func (n *Node) refreshBalances(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	eth, err1 := n.Client.BalanceAt(cctx, n.Wallet.Address())
	mor, err2 := n.K.MORBalance(cctx, n.Wallet.Address())
	block, _ := n.Client.BlockNumber(cctx)
	if err1 != nil || err2 != nil {
		return
	}
	n.balMu.Lock()
	n.eth, n.mor, n.balAt, n.blockAt = eth, mor, time.Now(), block
	n.balMu.Unlock()
}

// Status is a snapshot of the node for the CLI, API and dashboard.
type Status struct {
	Version        string              `json:"version"`
	Network        string              `json:"network"`
	ChainID        int64               `json:"chainId"`
	Role           string              `json:"role"`
	Address        string              `json:"address"`
	ETH            string              `json:"ethWei"`
	MOR            string              `json:"morWei"`
	LockedStake    string              `json:"lockedStakeWei"`
	Block          uint64              `json:"block"`
	RPC            string              `json:"rpc"`
	Gateway        string              `json:"gateway,omitempty"`
	ProviderListen string              `json:"providerListen,omitempty"`
	PublicEndpoint string              `json:"publicEndpoint,omitempty"`
	StartedAt      time.Time           `json:"startedAt"`
	Sessions       []consumer.Snapshot `json:"sessions"`
	Pools          []consumer.PoolInfo `json:"pools"`
	Keys           int                 `json:"activeKeys"`
	Totals         events.Totals       `json:"totals"`
	Provider       *provider.Stats     `json:"provider,omitempty"`
	Requests       []events.Request    `json:"recentRequests"`
	Events         []events.Event      `json:"recentEvents"`
	Explorer       string              `json:"explorer"`
	Models         []marketplace.Model `json:"-"`
}

// Status returns the current node snapshot.
func (n *Node) Status(_ context.Context) Status {
	nw := n.Cfg.NetworkInfo()
	n.balMu.Lock()
	eth, mor, block := n.eth, n.mor, n.blockAt
	n.balMu.Unlock()
	st := Status{
		Version: buildinfo.Version, Network: nw.Label, ChainID: nw.ChainID, Role: n.Cfg.Role,
		Address: n.Wallet.Address().Hex(), ETH: bigStr(eth), MOR: bigStr(mor), Block: block,
		RPC: n.Client.Endpoint(), Gateway: n.GatewayAddr, ProviderListen: n.ProviderAddr,
		PublicEndpoint: n.Cfg.Provider.PublicEndpoint, StartedAt: n.Started, Keys: n.Keys.Count(),
		Totals: n.Bus.Totals(), Requests: n.Bus.RecentRequests(50), Events: n.Bus.RecentEvents(50),
		Explorer: nw.ExplorerURL, LockedStake: "0",
	}
	if n.Sessions != nil {
		st.Sessions, st.Pools, st.LockedStake = n.Sessions.Sessions(), n.Sessions.Pools(), n.Sessions.LockedStake().String()
	}
	if n.Provider != nil {
		ps := n.Provider.Stats()
		st.Provider = &ps
	}
	return st
}

func bigStr(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}
