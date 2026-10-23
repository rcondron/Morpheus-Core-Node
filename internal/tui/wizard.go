package tui

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// Setup is everything the wizard collected.
type Setup struct {
	Config     *config.Config
	Wallet     *wallet.Wallet
	KeepWallet bool // reuse the existing keystore untouched
	Password   string
	AutoUnlock bool
	KeyName    string // first agent API key ("" = skip)
	StartNow   bool
}

// WizardOptions describe the current state of the home directory.
type WizardOptions struct {
	Home            string
	Existing        *config.Config
	ExistingAddress string
}

// ErrAborted is returned when the user quits the wizard.
var ErrAborted = errors.New("setup cancelled")

func run(fields ...huh.Field) error {
	return runGroups(huh.NewGroup(fields...))
}

func runGroups(groups ...*huh.Group) error {
	err := huh.NewForm(groups...).WithTheme(Theme()).WithWidth(76).WithShowHelp(true).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrAborted
	}
	return err
}

// RunWizard walks a new user from nothing to a configured node.
func RunWizard(ctx context.Context, opt WizardOptions) (*Setup, error) {
	fmt.Println()
	fmt.Println(Banner())
	fmt.Println()

	cfg := config.Default("mainnet")
	if opt.Existing != nil {
		cp := *opt.Existing
		cfg = &cp
	}
	cfg.Home = opt.Home
	s := &Setup{Config: cfg}

	// 1. What are we running?
	role, network := cfg.Role, cfg.Network
	networkOpts := []huh.Option[string]{
		huh.NewOption("BASE Mainnet — real MOR", "mainnet"),
		huh.NewOption("BASE Sepolia — testnet, free test tokens", "testnet"),
	}
	if os.Getenv("MORCORE_DEV_NETWORKS") != "" {
		networkOpts = append(networkOpts, huh.NewOption("Local devnet (morcore-devnet)", "local"))
	}
	err := runGroups(huh.NewGroup(
		huh.NewNote().
			Title("Welcome to Morpheus Core Node").
			Description("A lightweight routing node for the Morpheus decentralized compute marketplace.\n\n"+
				"• Consumers give AI agents one OpenAI-compatible endpoint, served by independent providers and paid in MOR on BASE.\n"+
				"• Providers sell model inference to the network and earn MOR.\n\n"+
				"Setup takes about two minutes."),
		huh.NewSelect[string]().Title("What do you want to run?").
			Options(
				huh.NewOption("Agent gateway — route my agents' AI requests to the marketplace", config.RoleConsumer),
				huh.NewOption("Provider — sell my model's compute to the marketplace", config.RoleProvider),
				huh.NewOption("Both", config.RoleBoth),
			).Value(&role),
		huh.NewSelect[string]().Title("Which network?").Options(networkOpts...).Value(&network),
	))
	if err != nil {
		return nil, err
	}
	if network != cfg.Network {
		n, _ := config.LookupNetwork(network)
		cfg.Chain.RPCURLs = append([]string(nil), n.PublicRPCs...)
	}
	cfg.Role, cfg.Network = role, network
	nw := cfg.NetworkInfo()

	// 2. RPC endpoint (needed for balances in the wallet step).
	if err := askRPC(ctx, cfg, nw); err != nil {
		return nil, err
	}
	nw = cfg.NetworkInfo()

	// 3. Wallet.
	if err := askWallet(ctx, s, opt, nw); err != nil {
		return nil, err
	}

	// 4. Role-specific settings.
	if cfg.ConsumerEnabled() {
		if err := askConsumer(cfg); err != nil {
			return nil, err
		}
	}
	if cfg.ProviderEnabled() {
		if err := askProvider(ctx, cfg, nw); err != nil {
			return nil, err
		}
	}

	// 5. First API key + start.
	createKey := cfg.ConsumerEnabled()
	keyName := "my-agent"
	start := true
	var fields []huh.Field
	if cfg.ConsumerEnabled() {
		fields = append(fields,
			huh.NewConfirm().Title("Create an API key for your agents now?").
				Description("Agents authenticate to this node with keys like sk-mor-…; you can add, limit and revoke more later.").
				Value(&createKey),
			huh.NewInput().Title("Key name").Description("A label such as the agent or app that will use it.").
				Value(&keyName).Validate(func(v string) error {
				if strings.TrimSpace(v) == "" && createKey {
					return errors.New("name required")
				}
				return nil
			}),
		)
	}
	fields = append(fields, huh.NewConfirm().Title("Start the node now?").Affirmative("Start").Negative("Later").Value(&start))
	if err := run(fields...); err != nil {
		return nil, err
	}
	if createKey {
		s.KeyName = strings.TrimSpace(keyName)
	}
	s.StartNow = start
	return s, nil
}

func validURL(v string) error {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("enter an http(s) URL")
	}
	return nil
}

func askRPC(ctx context.Context, cfg *config.Config, nw config.Network) error {
	choice := "public"
	custom := ""
	if len(cfg.Chain.RPCURLs) == 1 && !contains(nw.PublicRPCs, cfg.Chain.RPCURLs[0]) {
		choice, custom = "custom", cfg.Chain.RPCURLs[0]
	}
	for {
		err := runGroups(
			huh.NewGroup(huh.NewSelect[string]().Title("BASE RPC endpoint").
				Description("The node reads the marketplace and sends transactions through an RPC provider.").
				Options(
					huh.NewOption("Public endpoints — quick start, rate-limited", "public"),
					huh.NewOption("My own endpoint (Alchemy, Infura, QuickNode, …) — recommended", "custom"),
				).Value(&choice)),
			huh.NewGroup(huh.NewInput().Title("RPC URL").Placeholder("https://base-mainnet.g.alchemy.com/v2/…").
				Value(&custom).Validate(validURL)).
				WithHideFunc(func() bool { return choice != "custom" }),
		)
		if err != nil {
			return err
		}
		urls := nw.PublicRPCs
		if choice == "custom" {
			urls = []string{strings.TrimSpace(custom)}
		}
		var gotChain int64
		err = Spin("Connecting to "+nw.Label, func() error {
			c, err := chain.NewClient(urls, logx.Discard())
			if err != nil {
				return err
			}
			defer c.Close()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			id, err := c.ChainID(cctx)
			if err != nil {
				return err
			}
			gotChain = id.Int64()
			return nil
		})
		if err == nil && gotChain != nw.ChainID {
			err = fmt.Errorf("that endpoint serves chain %d, not %s (%d)", gotChain, nw.Label, nw.ChainID)
		}
		if err == nil {
			cfg.Chain.RPCURLs = append([]string(nil), urls...)
			return nil
		}
		retry := true
		if rerr := run(huh.NewConfirm().Title("Could not use that endpoint").
			Description(err.Error() + "\n\nTry a different endpoint?").Value(&retry)); rerr != nil {
			return rerr
		}
		if !retry {
			cfg.Chain.RPCURLs = append([]string(nil), urls...)
			return nil
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func askWallet(ctx context.Context, s *Setup, opt WizardOptions, nw config.Network) error {
	method := "create"
	opts := []huh.Option[string]{
		huh.NewOption("Create a new wallet", "create"),
		huh.NewOption("Import a recovery phrase (MetaMask, Rabby, …)", "mnemonic"),
		huh.NewOption("Import a private key", "key"),
		huh.NewOption("Import a keystore JSON file", "keystore"),
	}
	if opt.ExistingAddress != "" {
		method = "keep"
		opts = append([]huh.Option[string]{huh.NewOption("Keep current wallet "+opt.ExistingAddress, "keep")}, opts...)
	}
	if err := run(huh.NewSelect[string]().Title("Wallet").
		Description("The node pays for sessions (consumer) or receives earnings (provider) from this wallet. " +
			"It is stored encrypted on this machine.").
		Options(opts...).Value(&method)); err != nil {
		return err
	}

	switch method {
	case "keep":
		s.KeepWallet = true
		s.Config.Wallet.Address = opt.ExistingAddress
		return nil
	case "create":
		phrase, err := wallet.NewMnemonic(false)
		if err != nil {
			return err
		}
		w, err := wallet.FromMnemonic(phrase, "", 0)
		if err != nil {
			return err
		}
		saved := false
		if err := run(
			huh.NewNote().Title("Your recovery phrase").Description(formatPhrase(phrase)+"\n\n"+
				Warning.Render("Write these 12 words down and keep them offline.")+"\n"+
				"They are the only way to recover this wallet. They will not be shown again.\n\n"+
				"Address: "+Code.Render(w.Address().Hex())),
			huh.NewConfirm().Title("I have saved my recovery phrase").Affirmative("Yes, saved").Negative("No").
				Value(&saved).Validate(func(v bool) error {
				if !v {
					return errors.New("save the phrase before continuing")
				}
				return nil
			}),
		); err != nil {
			return err
		}
		s.Wallet = w
	case "mnemonic":
		var phrase string
		if err := run(huh.NewText().Title("Recovery phrase").Description("12 or 24 words, separated by spaces.").
			Lines(3).Value(&phrase).Validate(func(v string) error {
			if !wallet.ValidMnemonic(v) {
				return errors.New("not a valid recovery phrase — check spelling and word count")
			}
			return nil
		})); err != nil {
			return err
		}
		w, err := pickAccount(ctx, phrase, nw)
		if err != nil {
			return err
		}
		s.Wallet = w
	case "key":
		var hexKey string
		if err := run(huh.NewInput().Title("Private key").Description("64 hex characters, with or without 0x.").
			EchoMode(huh.EchoModePassword).Value(&hexKey).Validate(func(v string) error {
			_, err := wallet.FromHex(v)
			return err
		})); err != nil {
			return err
		}
		s.Wallet, _ = wallet.FromHex(hexKey)
	case "keystore":
		var path, pass string
		if err := run(
			huh.NewInput().Title("Keystore file path").Value(&path).Validate(func(v string) error {
				_, err := os.Stat(strings.TrimSpace(v))
				return err
			}),
			huh.NewInput().Title("Keystore password").EchoMode(huh.EchoModePassword).Value(&pass),
		); err != nil {
			return err
		}
		var w *wallet.Wallet
		err := Spin("Decrypting keystore", func() error {
			var err error
			w, err = wallet.LoadKeystore(strings.TrimSpace(path), pass)
			return err
		})
		if err != nil {
			return err
		}
		s.Wallet = w
	}
	s.Config.Wallet.Address = s.Wallet.Address().Hex()

	var pass, confirm string
	auto := false
	if err := run(
		huh.NewInput().Title("Choose a wallet password").
			Description("Encrypts the key on disk. You'll need it to start the node.").
			EchoMode(huh.EchoModePassword).Value(&pass).Validate(func(v string) error {
			if len(v) < 8 {
				return errors.New("use at least 8 characters")
			}
			return nil
		}),
		huh.NewInput().Title("Confirm password").EchoMode(huh.EchoModePassword).Value(&confirm).
			Validate(func(v string) error {
				if v != pass {
					return errors.New("passwords do not match")
				}
				return nil
			}),
		huh.NewConfirm().Title("Unlock automatically when the node starts?").
			Description("Saves the password in a private file next to the wallet (0600). Handy for servers "+
				"and auto-start; skip it on shared machines. MORCORE_PASSWORD also works.").
			Value(&auto),
	); err != nil {
		return err
	}
	s.Password, s.AutoUnlock = pass, auto
	return nil
}

func formatPhrase(p string) string {
	words := strings.Fields(p)
	var b strings.Builder
	for i, w := range words {
		b.WriteString(fmt.Sprintf("%s %-10s", Subtle.Render(fmt.Sprintf("%2d.", i+1)), Strong.Render(w)))
		if (i+1)%4 == 0 {
			b.WriteByte('\n')
		} else {
			b.WriteString("  ")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// pickAccount shows the first addresses of a phrase with balances, so users
// whose funds sit on "Account 2" in MetaMask pick the right one.
func pickAccount(ctx context.Context, phrase string, nw config.Network) (*wallet.Wallet, error) {
	const n = 5
	wallets := make([]*wallet.Wallet, n)
	for i := range wallets {
		w, err := wallet.FromMnemonic(phrase, "", i)
		if err != nil {
			return nil, err
		}
		wallets[i] = w
	}
	labels := make([]string, n)
	for i, w := range wallets {
		labels[i] = fmt.Sprintf("Account %d  %s", i+1, w.Address().Hex())
	}
	_ = Spin("Looking up balances", func() error {
		c, err := chain.NewClient(nw.PublicRPCs, logx.Discard())
		if err != nil {
			return err
		}
		defer c.Close()
		k := chain.NewContracts(c, common.HexToAddress(nw.DiamondAddress), common.HexToAddress(nw.MORTokenAddress), big.NewInt(nw.ChainID))
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for i, w := range wallets {
			wg.Add(1)
			go func(i int, w *wallet.Wallet) {
				defer wg.Done()
				eth, err1 := c.BalanceAt(cctx, w.Address())
				mor, err2 := k.MORBalance(cctx, w.Address())
				if err1 == nil && err2 == nil {
					labels[i] += Subtle.Render(fmt.Sprintf("  %s · %s", units.MOR(mor), units.ETH(eth)))
				}
			}(i, w)
		}
		wg.Wait()
		return nil
	})
	choice := 0
	opts := make([]huh.Option[int], n)
	for i := range wallets {
		opts[i] = huh.NewOption(labels[i], i)
	}
	if err := run(huh.NewSelect[int]().Title("Which account?").
		Description("MetaMask \"Account 2\" is index 2 here. Pick the one holding your MOR and ETH.").
		Options(opts...).Value(&choice)); err != nil {
		return nil, err
	}
	return wallets[choice], nil
}

func askConsumer(cfg *config.Config) error {
	listen := cfg.Gateway.Listen
	dur := cfg.Sessions.Duration.String()
	parallel := strconv.Itoa(cfg.Sessions.MaxPerModel)
	maxStake := cfg.Sessions.MaxStake
	if maxStake == "" || maxStake == "0" {
		maxStake = ""
	}
	err := run(
		huh.NewNote().Title("Agent gateway").Description(
			"Agents call this node like the OpenAI API. On first use of a model the node buys a session "+
				"from the best-rated provider by escrowing MOR; unused stake is returned when the session "+
				"closes. Requests run in parallel across several sessions."),
		huh.NewInput().Title("API listen address").Description("Keep 127.0.0.1 unless remote agents must connect.").
			Value(&listen).Validate(func(v string) error {
			_, _, err := net.SplitHostPort(v)
			return err
		}),
		huh.NewSelect[string]().Title("Session length").Description("Longer sessions escrow more MOR up front but re-open less often.").
			Options(huh.NewOption("30 minutes", "30m0s"), huh.NewOption("1 hour", "1h0m0s"), huh.NewOption("4 hours", "4h0m0s"), huh.NewOption("24 hours", "24h0m0s")).
			Value(&dur),
		huh.NewSelect[string]().Title("Parallel sessions per model").
			Description("Each provider session serves one request at a time; more sessions = more concurrency.").
			Options(huh.NewOption("1", "1"), huh.NewOption("2 (recommended)", "2"), huh.NewOption("4", "4"), huh.NewOption("8", "8")).
			Value(&parallel),
		huh.NewInput().Title("Spending cap (MOR, optional)").
			Description("Max MOR escrowed across open sessions at once. Leave empty for no cap.").
			Placeholder("e.g. 50").Value(&maxStake).Validate(func(v string) error {
			_, err := units.ParseToken(v)
			return err
		}),
	)
	if err != nil {
		return err
	}
	cfg.Gateway.Enabled = true
	cfg.Gateway.Listen = strings.TrimSpace(listen)
	if d, err := time.ParseDuration(dur); err == nil {
		cfg.Sessions.Duration = config.D(d)
	}
	cfg.Sessions.MaxPerModel, _ = strconv.Atoi(parallel)
	cfg.Sessions.MaxTotal = max(cfg.Sessions.MaxTotal, cfg.Sessions.MaxPerModel*3)
	if strings.TrimSpace(maxStake) == "" {
		maxStake = "0"
	}
	cfg.Sessions.MaxStake = strings.TrimSpace(maxStake)
	return nil
}

func askProvider(ctx context.Context, cfg *config.Config, nw config.Network) error {
	endpoint := cfg.Provider.PublicEndpoint
	listen := cfg.Provider.Listen
	if err := run(
		huh.NewNote().Title("Provider").Description(
			"Consumers connect to your node over TCP (port 3333 by default) and your node forwards their "+
				"requests to the model backend: a hosted API you resell (Venice, OpenAI, Anthropic, OpenRouter, "+
				"Groq, Prodia…) or your own server (vLLM, llama.cpp, Ollama, LM Studio…)."),
		huh.NewInput().Title("Public address consumers reach you at (host:port)").
			Description("Registered on-chain. Open this TCP port in your firewall.").Placeholder("provider.example.com:3333").
			Value(&endpoint).Validate(func(v string) error {
			_, _, err := net.SplitHostPort(strings.TrimSpace(v))
			return err
		}),
		huh.NewInput().Title("Listen address").Value(&listen).Validate(func(v string) error {
			_, _, err := net.SplitHostPort(v)
			return err
		}),
	); err != nil {
		return err
	}
	cfg.Provider.Enabled = true
	cfg.Provider.PublicEndpoint = strings.TrimSpace(endpoint)
	cfg.Provider.Listen = strings.TrimSpace(listen)

	for {
		add := true
		title := "Add a model to sell now?"
		if n := len(cfg.Provider.Models); n > 0 {
			title = fmt.Sprintf("You sell %d model(s). Add another?", n)
			add = false
		}
		if err := run(huh.NewConfirm().Title(title).
			Description("You can also add models later with `morcore provider add`.").Value(&add)); err != nil {
			return err
		}
		if !add {
			return nil
		}
		pm, err := AskOffering(ctx, cfg, nw)
		if errors.Is(err, ErrAborted) {
			return err
		}
		if err != nil {
			fmt.Println(Warning.Render("  " + err.Error()))
			continue
		}
		cfg.Provider.Models, _, _ = config.MergeModels(cfg.Provider.Models, []config.ProviderModel{*pm})
		fmt.Println(Good.Render("  ✓ ") + pm.Name + Subtle.Render(" via "+pm.BackendModel))
	}
}

func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("required")
	}
	return nil
}
