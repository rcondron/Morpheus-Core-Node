package cli

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/gateway"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
)

// keyBackend is either the running node's admin API or the key file.
type keyBackend interface {
	list() ([]gateway.KeyView, error)
	create(gateway.KeyRequest) (gateway.KeyView, error)
	update(ref string, req gateway.KeyRequest) (gateway.KeyView, error)
	revoke(ref string) (gateway.KeyView, error)
	rotate(ref string) (gateway.KeyView, error)
}

func (a *App) keys() (keyBackend, *config.Config, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, nil, err
	}
	if c, ok := a.running(); ok {
		return remoteKeys{c}, cfg, nil
	}
	s, err := apikeys.Open(cfg.Path(config.APIKeysFile), logx.Discard())
	if err != nil {
		return nil, nil, err
	}
	return localKeys{s}, cfg, nil
}

type remoteKeys struct{ c *Client }

func (r remoteKeys) list() ([]gateway.KeyView, error) {
	var out struct{ Keys []gateway.KeyView }
	err := r.c.Do("GET", "/admin/keys", nil, &out)
	return out.Keys, err
}

func (r remoteKeys) create(req gateway.KeyRequest) (gateway.KeyView, error) {
	var v gateway.KeyView
	return v, r.c.Do("POST", "/admin/keys", req, &v)
}

func (r remoteKeys) update(ref string, req gateway.KeyRequest) (gateway.KeyView, error) {
	var v gateway.KeyView
	return v, r.c.Do("PATCH", "/admin/keys/"+ref, req, &v)
}

func (r remoteKeys) revoke(ref string) (gateway.KeyView, error) {
	var v gateway.KeyView
	return v, r.c.Do("DELETE", "/admin/keys/"+ref, nil, &v)
}

func (r remoteKeys) rotate(ref string) (gateway.KeyView, error) {
	var v gateway.KeyView
	return v, r.c.Do("POST", "/admin/keys/"+ref+"/rotate", nil, &v)
}

type localKeys struct{ s *apikeys.Store }

func (l localKeys) list() ([]gateway.KeyView, error) {
	var out []gateway.KeyView
	for _, k := range l.s.List() {
		out = append(out, gateway.ViewKey(k))
	}
	return out, nil
}

func (l localKeys) create(req gateway.KeyRequest) (gateway.KeyView, error) {
	o := apikeys.CreateOptions{Scopes: req.Scopes}
	if req.Name != nil {
		o.Name = *req.Name
	}
	if req.Models != nil {
		o.Models = *req.Models
	}
	if req.RPM != nil {
		o.RPM = *req.RPM
	}
	if req.Concurrency != nil {
		o.Concurrency = *req.Concurrency
	}
	if req.DailyTokens != nil {
		o.DailyTokens = *req.DailyTokens
	}
	if req.ExpiresIn != nil && *req.ExpiresIn != "" {
		d, err := gateway.ParseTTL(*req.ExpiresIn)
		if err != nil {
			return gateway.KeyView{}, err
		}
		o.TTL = d
	}
	secret, k, err := l.s.Create(o)
	if err != nil {
		return gateway.KeyView{}, err
	}
	v := gateway.ViewKey(k)
	v.Secret = secret
	return v, nil
}

func (l localKeys) update(ref string, req gateway.KeyRequest) (gateway.KeyView, error) {
	u := apikeys.Update{Name: req.Name, Scopes: req.Scopes, Models: req.Models, RPM: req.RPM, Concurrency: req.Concurrency, DailyTokens: req.DailyTokens}
	if req.ExpiresIn != nil {
		var d time.Duration
		if *req.ExpiresIn != "" && *req.ExpiresIn != "never" {
			var err error
			if d, err = gateway.ParseTTL(*req.ExpiresIn); err != nil {
				return gateway.KeyView{}, err
			}
		}
		u.TTL = &d
	}
	k, err := l.s.Update(ref, u)
	return gateway.ViewKey(k), err
}

func (l localKeys) revoke(ref string) (gateway.KeyView, error) {
	k, err := l.s.Revoke(ref)
	return gateway.ViewKey(k), err
}

func (l localKeys) rotate(ref string) (gateway.KeyView, error) {
	secret, k, err := l.s.Rotate(ref)
	v := gateway.ViewKey(k)
	v.Secret = secret
	return v, err
}

// keyFlags registers the shared limit flags.
type keyFlags struct {
	models, scopes, expires        string
	rpm, concurrency               int
	dailyTokens                    int64
	setModels, setScopes, setLimit map[string]bool
}

func (f *keyFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.models, "models", "", "")
	fs.StringVar(&f.scopes, "scopes", "", "")
	fs.StringVar(&f.expires, "expires", "", "")
	fs.IntVar(&f.rpm, "rpm", 0, "")
	fs.IntVar(&f.concurrency, "concurrency", 0, "")
	fs.Int64Var(&f.dailyTokens, "daily-tokens", 0, "")
}

func (f *keyFlags) request(fs *flag.FlagSet) gateway.KeyRequest {
	var req gateway.KeyRequest
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "models":
			m := splitCSV(f.models)
			req.Models = &m
		case "scopes":
			req.Scopes = splitCSV(f.scopes)
		case "expires":
			e := f.expires
			req.ExpiresIn = &e
		case "rpm":
			req.RPM = &f.rpm
		case "concurrency":
			req.Concurrency = &f.concurrency
		case "daily-tokens":
			req.DailyTokens = &f.dailyTokens
		}
	})
	return req
}

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func keysList(a *App, args []string) error {
	kb, _, err := a.keys()
	if err != nil {
		return err
	}
	keys, err := kb.list()
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(keys)
	}
	if len(keys) == 0 {
		a.info("No API keys yet. Create one with: %s", tui.Code.Render("morcore keys create <name>"))
		return nil
	}
	t := a.table()
	header(t, "NAME", "ID", "PREFIX", "STATUS", "SCOPES", "LIMITS", "REQUESTS", "TOKENS", "LAST USED")
	for _, k := range keys {
		status := tui.Good.Render(k.Status)
		if k.Status != "active" {
			status = tui.Danger.Render(k.Status)
		}
		fmt.Fprintf(t, "%s\t%s\t%s…\t%s\t%s\t%s\t%d\t%s\t%s\n", tui.Strong.Render(k.Name), k.ID, k.Prefix, status,
			strings.Join(k.Scopes, ","), limits(k), k.Usage.Requests, humanTokens(k.Usage.InputTokens+k.Usage.OutputTokens), ago(k.LastUsedAt))
	}
	return t.Flush()
}

func limits(k gateway.KeyView) string {
	var parts []string
	if len(k.Models) > 0 {
		parts = append(parts, "models:"+strings.Join(k.Models, "+"))
	}
	if k.RPM > 0 {
		parts = append(parts, fmt.Sprintf("%d/min", k.RPM))
	}
	if k.Concurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d parallel", k.Concurrency))
	}
	if k.DailyTokens > 0 {
		parts = append(parts, humanTokens(k.DailyTokens)+" tok/day")
	}
	if k.ExpiresAt != nil {
		parts = append(parts, "expires "+k.ExpiresAt.Local().Format("2006-01-02"))
	}
	if len(parts) == 0 {
		return tui.Subtle.Render("none")
	}
	return strings.Join(parts, " ")
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func ago(t *time.Time) string {
	if t == nil {
		return tui.Subtle.Render("never")
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func keysCreate(a *App, args []string) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	var kf keyFlags
	kf.register(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore keys create research-agent --rpm 60 --models llama-3.3-70b", "keys create needs exactly one name")
	}
	kb, cfg, err := a.keys()
	if err != nil {
		return err
	}
	req := kf.request(fs)
	req.Name = &pos[0]
	v, err := kb.create(req)
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(v)
	}
	a.showSecret(cfg, v, "API key created")
	return nil
}

func (a *App) showSecret(cfg *config.Config, v gateway.KeyView, title string) {
	lines := []string{
		tui.Good.Render("✓ "+title) + "  " + tui.Subtle.Render(v.Name+" ("+v.ID+")"),
		"",
		tui.Strong.Render(v.Secret),
		"",
		tui.Warning.Render("Copy it now — only a hash is stored, it cannot be shown again."),
	}
	fmt.Fprintln(a.Out, tui.Box.Render(strings.Join(lines, "\n")))
	if cfg.ConsumerEnabled() {
		fmt.Fprintln(a.Out, tui.Subtle.Render("  Use with any OpenAI SDK:"))
		fmt.Fprintln(a.Out, tui.Code.Render("  OPENAI_BASE_URL=http://"+cfg.Gateway.Listen+"/v1 OPENAI_API_KEY="+v.Secret))
		fmt.Fprintln(a.Out, tui.Subtle.Render("  curl:"))
		fmt.Fprintln(a.Out, tui.Code.Render(fmt.Sprintf(`  curl http://%s/v1/chat/completions -H "Authorization: Bearer %s" \`, cfg.Gateway.Listen, v.Secret)))
		fmt.Fprintln(a.Out, tui.Code.Render(`    -H "Content-Type: application/json" -d '{"model":"<model>","messages":[{"role":"user","content":"hello"}]}'`))
	}
}

func keysShow(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("", "keys show needs a key name or id")
	}
	kb, _, err := a.keys()
	if err != nil {
		return err
	}
	keys, err := kb.list()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.ID == args[0] || strings.EqualFold(k.Name, args[0]) {
			if a.JSON {
				return a.printJSON(k)
			}
			t := a.table()
			row := func(label, val string) { fmt.Fprintf(t, "%s\t%s\n", tui.Subtle.Render(label), val) }
			row("Name", tui.Strong.Render(k.Name))
			row("ID", k.ID)
			row("Prefix", k.Prefix+"…")
			row("Status", k.Status)
			row("Scopes", strings.Join(k.Scopes, ", "))
			row("Limits", limits(k))
			row("Created", k.CreatedAt.Local().Format(time.RFC1123))
			row("Last used", ago(k.LastUsedAt))
			row("Requests", fmt.Sprintf("%d (%d errors)", k.Usage.Requests, k.Usage.Errors))
			row("Tokens", fmt.Sprintf("%s in / %s out", humanTokens(k.Usage.InputTokens), humanTokens(k.Usage.OutputTokens)))
			if k.Usage.Day != "" {
				row("Today", humanTokens(k.Usage.DayTokens)+" tokens ("+k.Usage.Day+" UTC)")
			}
			return t.Flush()
		}
	}
	return fmt.Errorf("API key %q not found", args[0])
}

func keysUpdate(a *App, args []string) error {
	fs := flag.NewFlagSet("keys update", flag.ContinueOnError)
	var kf keyFlags
	kf.register(fs)
	name := fs.String("name", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore keys update research-agent --rpm 120 --models all", "keys update needs a key name or id")
	}
	kb, _, err := a.keys()
	if err != nil {
		return err
	}
	req := kf.request(fs)
	if req.Models != nil && len(*req.Models) == 1 && strings.EqualFold((*req.Models)[0], "all") {
		empty := []string{}
		req.Models = &empty
	}
	if *name != "" {
		req.Name = name
	}
	v, err := kb.update(pos[0], req)
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(v)
	}
	a.ok("updated %s: %s", tui.Strong.Render(v.Name), limits(v))
	return nil
}

func keysRotate(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("", "keys rotate needs a key name or id")
	}
	kb, cfg, err := a.keys()
	if err != nil {
		return err
	}
	v, err := kb.rotate(args[0])
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(v)
	}
	a.showSecret(cfg, v, "Key rotated — the old secret stopped working")
	return nil
}

func keysRevoke(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("", "keys revoke needs a key name or id")
	}
	kb, _, err := a.keys()
	if err != nil {
		return err
	}
	v, err := kb.revoke(args[0])
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(v)
	}
	a.ok("revoked %s (%s); requests with it are now rejected", tui.Strong.Render(v.Name), v.ID)
	return nil
}
