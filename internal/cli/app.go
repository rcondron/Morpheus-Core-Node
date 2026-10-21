// Package cli implements the morcore command line.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
)

// App carries global options for a CLI invocation.
type App struct {
	Home string
	JSON bool
	Out  io.Writer
	Err  io.Writer
}

type command struct {
	name    string
	args    string
	summary string
	long    string
	run     func(a *App, args []string) error
	subs    []*command
}

var commands []*command

func init() {
	commands = []*command{
		{name: "init", summary: "Set up the node (interactive wizard)", run: cmdInit, long: initHelp},
		{name: "start", summary: "Start the node (live dashboard when interactive)", run: cmdStart, long: startHelp},
		{name: "status", summary: "Show node status", run: cmdStatus},
		{name: "chat", args: "[model]", summary: "Chat with a marketplace model from the terminal", run: cmdChat},
		{name: "models", summary: "List models on the marketplace", run: cmdModels},
		{name: "bids", args: "<model>", summary: "Show ranked provider offers for a model", run: cmdBids},
		{name: "keys", summary: "Create and manage agent API keys", run: keysList, subs: []*command{
			{name: "list", summary: "List API keys", run: keysList},
			{name: "create", args: "<name>", summary: "Create a key (shown once)", run: keysCreate, long: keysCreateHelp},
			{name: "show", args: "<name|id>", summary: "Show a key's settings and usage", run: keysShow},
			{name: "update", args: "<name|id>", summary: "Change a key's limits, models or scopes", run: keysUpdate},
			{name: "rotate", args: "<name|id>", summary: "Replace a key's secret", run: keysRotate},
			{name: "revoke", args: "<name|id>", summary: "Disable a key permanently", run: keysRevoke},
		}},
		{name: "sessions", summary: "Inspect, open and close marketplace sessions", run: sessionsList, subs: []*command{
			{name: "list", summary: "List sessions", run: sessionsList},
			{name: "open", args: "<model>", summary: "Open a session now (normally automatic)", run: sessionsOpen},
			{name: "close", args: "<session-id|all>", summary: "Close sessions and release their stake", run: sessionsClose},
		}},
		{name: "wallet", summary: "Wallet address, balances and released stake", run: walletShow, subs: []*command{
			{name: "show", summary: "Show address and balances", run: walletShow},
			{name: "withdraw", summary: "Withdraw stake released from closed sessions", run: walletWithdraw},
			{name: "export", summary: "Print the wallet private key", run: walletExport},
		}},
		{name: "provider", summary: "Sell compute: registration, bids and earnings", run: providerStatus, subs: []*command{
			{name: "status", summary: "Show on-chain provider registration and bids", run: providerStatus},
			{name: "register", summary: "Register (or update) this wallet as a provider", run: providerRegister},
			{name: "bid", args: "<model>", summary: "Offer a model at a price", run: providerBid},
			{name: "unbid", args: "<bid-id>", summary: "Withdraw a bid", run: providerUnbid},
			{name: "claim", summary: "Claim earnings from finished sessions", run: providerClaim},
			{name: "model-register", args: "<name>", summary: "Create a new model listing", run: providerModelRegister},
			{name: "deregister", summary: "Stop being a provider and recover stake", run: providerDeregister},
		}},
		{name: "doctor", summary: "Check configuration, wallet and connectivity", run: cmdDoctor},
		{name: "config", summary: "Show where configuration lives (config show: print it)", run: cmdConfig},
		{name: "version", summary: "Print version information", run: cmdVersion},
	}
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	tui.Init()
	a := &App{Out: os.Stdout, Err: os.Stderr}
	fs := flag.NewFlagSet("morcore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Home, "home", config.DefaultHome(), "node home directory")
	fs.BoolVar(&a.JSON, "json", false, "machine-readable output")
	noColor := fs.Bool("no-color", false, "disable colors")
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	version := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(a.Err, tui.Danger.Render("error: ")+err.Error())
		return 2
	}
	rest, lateNoColor := a.globalFlags(fs.Args())
	if *noColor || lateNoColor {
		os.Setenv("NO_COLOR", "1")
	}
	if *version {
		rest = []string{"version"}
	}
	if *help {
		a.usage(nil)
		return 0
	}
	if len(rest) == 0 {
		return a.exit(cmdDefault(a))
	}
	if rest[0] == "help" {
		a.usage(rest[1:])
		return 0
	}
	cmd, cmdArgs := find(commands, rest)
	if cmd == nil {
		fmt.Fprintf(a.Err, "%s unknown command %q\n\n", tui.Danger.Render("error:"), rest[0])
		a.usage(nil)
		return 2
	}
	for _, x := range cmdArgs {
		if x == "-h" || x == "--help" {
			a.commandHelp(cmd, rest)
			return 0
		}
	}
	return a.exit(cmd.run(a, cmdArgs))
}

// globalFlags extracts global flags given after the command name, so that
// `morcore keys list --json` works like `morcore --json keys list`.
func (a *App) globalFlags(args []string) (rest []string, noColor bool) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		x := args[i]
		switch {
		case x == "--":
			return append(rest, args[i:]...), noColor
		case x == "--json" || x == "-json":
			a.JSON = true
		case x == "--no-color" || x == "-no-color":
			noColor = true
		case (x == "--home" || x == "-home") && i+1 < len(args):
			a.Home = args[i+1]
			i++
		case strings.HasPrefix(x, "--home=") || strings.HasPrefix(x, "-home="):
			a.Home = x[strings.IndexByte(x, '=')+1:]
		default:
			rest = append(rest, x)
		}
	}
	return rest, noColor
}

func find(cmds []*command, args []string) (*command, []string) {
	for _, c := range cmds {
		if c.name != args[0] {
			continue
		}
		if len(c.subs) > 0 && len(args) > 1 {
			if sub, rest := find(c.subs, args[1:]); sub != nil {
				return sub, rest
			}
		}
		return c, args[1:]
	}
	return nil, nil
}

func (a *App) exit(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, tui.ErrAborted) {
		fmt.Fprintln(a.Err, tui.Subtle.Render("cancelled"))
		return 130
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.Err, "%s %s\n", tui.Danger.Render("error:"), ue.msg)
		if ue.hint != "" {
			fmt.Fprintf(a.Err, "%s\n", tui.Subtle.Render(ue.hint))
		}
		return 2
	}
	fmt.Fprintf(a.Err, "%s %s\n", tui.Danger.Render("error:"), err)
	return 1
}

type usageError struct{ msg, hint string }

func (e *usageError) Error() string { return e.msg }

func usagef(hint, format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...), hint: hint}
}

func (a *App) usage(path []string) {
	if len(path) > 0 {
		if cmd, _ := find(commands, path); cmd != nil {
			a.commandHelp(cmd, path)
			return
		}
	}
	w := a.Out
	fmt.Fprintln(w, tui.Banner())
	fmt.Fprintln(w)
	fmt.Fprintln(w, tui.Heading.Render("Usage"))
	fmt.Fprintln(w, "  morcore                 first run: setup wizard · afterwards: start the node")
	fmt.Fprintln(w, "  morcore <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, tui.Heading.Render("Commands"))
	for _, c := range commands {
		name := c.name
		if c.args != "" {
			name += " " + c.args
		}
		fmt.Fprintf(w, "  %-22s %s\n", tui.Good.Render(pad(name, 22)), c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, tui.Heading.Render("Global flags"))
	fmt.Fprintf(w, "  %-22s %s\n", "--home <dir>", "node home (default "+config.DefaultHome()+", or $MORCORE_HOME)")
	fmt.Fprintf(w, "  %-22s %s\n", "--json", "machine-readable output")
	fmt.Fprintf(w, "  %-22s %s\n", "--no-color", "disable colors")
	fmt.Fprintln(w)
	fmt.Fprintln(w, tui.Subtle.Render("Run `morcore help <command>` for details."))
}

func (a *App) commandHelp(c *command, path []string) {
	w := a.Out
	name := strings.Join(path[:depth(path)], " ")
	fmt.Fprintf(w, "%s %s\n\n", tui.Heading.Render("morcore "+name), c.args)
	fmt.Fprintln(w, c.summary)
	if c.long != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimSpace(c.long))
	}
	if len(c.subs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, tui.Heading.Render("Subcommands"))
		subs := append([]*command(nil), c.subs...)
		sort.SliceStable(subs, func(i, j int) bool { return false })
		for _, s := range subs {
			n := s.name
			if s.args != "" {
				n += " " + s.args
			}
			fmt.Fprintf(w, "  %s %s\n", tui.Good.Render(pad(n, 26)), s.summary)
		}
	}
}

func depth(path []string) int {
	n := 0
	cmds := commands
	for _, p := range path {
		found := false
		for _, c := range cmds {
			if c.name == p {
				n++
				cmds = c.subs
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return max(n, 1)
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// parseFlags parses flags that may appear before, between or after positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("", "%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

const initHelp = `
Runs the guided setup: role (agent gateway / provider), network, RPC endpoint,
wallet (create or import), password and your first agent API key.

Non-interactive (scripts, containers):
  morcore init --yes --network testnet --role consumer \
      --rpc https://sepolia.base.org --key-env MORCORE_PRIVATE_KEY \
      --password-env MORCORE_PASSWORD --api-key my-agent

Flags:
  --yes               do not prompt; use flags and environment variables
  --network NAME      mainnet | testnet
  --role ROLE         consumer | provider | both
  --rpc URL           BASE RPC endpoint (repeat or comma-separate for failover)
  --key-env VAR       import the private key from env VAR (default: generate)
  --mnemonic-env VAR  import a recovery phrase from env VAR
  --index N           account index for --mnemonic-env (default 0)
  --password-env VAR  read the wallet password from env VAR (required with --yes)
  --auto-unlock       store the password so the node starts unattended
  --api-key NAME      create an agent API key with this name
  --listen ADDR       agent API listen address (default 127.0.0.1:8082)
  --force             overwrite an existing configuration`

const startHelp = `
Starts the node: the agent API (OpenAI-compatible, /v1) and/or the provider
listener. In a terminal it shows a live dashboard; press q to stop. On stop,
sessions this node opened are closed so unused stake returns to the wallet.

Flags:
  --headless          plain logs instead of the dashboard (servers, services)
  --log-level LEVEL   debug | info | warn | error
  --log-format FMT    auto | pretty | json

The wallet password is taken from $MORCORE_PASSWORD, the configured password
file, or prompted for. $MORCORE_PRIVATE_KEY bypasses the keystore entirely.`

const keysCreateHelp = `
Creates an API key for an agent. The secret is printed once; only a hash is stored.

Flags:
  --models a,b        restrict to these models (names or ids); default all
  --rpm N             requests per minute (0 = unlimited)
  --concurrency N     parallel requests (0 = unlimited)
  --daily-tokens N    daily token quota (0 = unlimited)
  --expires DUR       lifetime, e.g. 30d, 12h (default never)
  --scopes LIST       inference (default), sessions, admin`
