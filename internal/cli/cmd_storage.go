package cli

import (
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/gateway"
	"github.com/rcondron/Morpheus-Core-Node/internal/storage"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

const storageHelp = `Files are kept by storage providers on the Morpheus marketplace, pinned on
their IPFS (Kubo) nodes. The node buys storage leases (marketplace sessions on
a storage listing, paid directly to the provider), renews them while you keep
files, and checks every download against the CID it computed itself.

Files are encrypted to your wallet key before they leave this machine (the
provider sees only ciphertext and a size) unless you pass --public.

Agents use the same storage through the OpenAI-style Files API:
  POST /v1/files, GET /v1/files, GET /v1/files/{id}/content, DELETE /v1/files/{id}
with an API key that has the storage scope (morcore keys create <name> --scopes inference,storage).`

// storageClient returns the running node: storage needs it to renew leases.
func (a *App) storageClient() (*Client, error) {
	c, ok := a.running()
	if !ok {
		return nil, usagef("Start it with `morcore start`; the node renews storage leases while it runs.", "the node is not running")
	}
	return c, nil
}

type fileView struct {
	ID          string   `json:"id"`
	Bytes       int64    `json:"bytes"`
	CreatedAt   int64    `json:"created_at"`
	Filename    string   `json:"filename"`
	Purpose     string   `json:"purpose"`
	ContentType string   `json:"content_type"`
	CID         string   `json:"cid"`
	Encrypted   bool     `json:"encrypted"`
	StoredBytes uint64   `json:"stored_bytes"`
	Copies      int      `json:"copies"`
	Providers   []string `json:"providers"`
}

func storageList(a *App, args []string) error {
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	var out struct {
		Data []fileView `json:"data"`
	}
	if err := c.Do("GET", "/v1/files", nil, &out); err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(out.Data)
	}
	if len(out.Data) == 0 {
		a.info("No files stored yet. Store one with %s.", tui.Code.Render("morcore storage put <file>"))
		return nil
	}
	t := a.table()
	header(t, "ID", "NAME", "SIZE", "ENCRYPTED", "COPIES", "STORED", "CID")
	for _, f := range out.Data {
		enc := tui.Good.Render("yes")
		if !f.Encrypted {
			enc = tui.Warning.Render("no")
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", f.ID, tui.Strong.Render(f.Filename), config.FormatBytes(uint64(f.Bytes)), enc, f.Copies,
			time.Unix(f.CreatedAt, 0).Local().Format("Jan 2 15:04"), tui.Subtle.Render(shortHash(f.CID)))
	}
	return t.Flush()
}

func storagePut(a *App, args []string) error {
	fs := flag.NewFlagSet("storage put", flag.ContinueOnError)
	public := fs.Bool("public", false, "")
	name := fs.String("name", "", "")
	purpose := fs.String("purpose", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore storage put report.pdf   (--public stores it unencrypted)", "storage put needs a file")
	}
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return usagef("Archive the folder first (e.g. tar czf folder.tgz folder).", "%s is a directory", pos[0])
	}
	filename := *name
	if filename == "" {
		filename = filepath.Base(pos[0])
	}
	fields := map[string]string{}
	if *purpose != "" {
		fields["purpose"] = *purpose
	}
	if *public {
		fields["public"] = "true"
	}
	var out fileView
	what := "Encrypting and storing "
	if *public {
		what = "Storing "
	}
	err = tui.Spin(what+filename+" ("+config.FormatBytes(uint64(st.Size()))+") on the marketplace", func() error {
		return c.Upload("/v1/files", filename, fields, f, &out)
	})
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(out)
	}
	a.ok("stored %s as %s with %d provider(s)", tui.Strong.Render(out.Filename), tui.Code.Render(out.ID), out.Copies)
	a.info("%s %s", tui.Subtle.Render("cid"), out.CID)
	if !out.Encrypted {
		a.info("%s", tui.Subtle.Render("Stored unencrypted: anyone who has the CID can read it."))
	}
	return nil
}

func storageGet(a *App, args []string) error {
	fs := flag.NewFlagSet("storage get", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("Example: morcore storage get file-1a2b… ./report.pdf   (- writes to stdout)", "storage get needs a file id")
	}
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	var meta fileView
	if err := c.Do("GET", "/v1/files/"+url.PathEscape(pos[0]), nil, &meta); err != nil {
		return err
	}
	dest := filepath.Base(meta.Filename)
	if dest == "" || dest == "." || dest == string(filepath.Separator) {
		dest = meta.ID
	}
	if len(pos) == 2 {
		dest = pos[1]
	}
	var w io.Writer = a.Out
	var tmp *os.File
	if dest != "-" {
		if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
			dest = filepath.Join(dest, filepath.Base(meta.Filename))
		}
		if _, err := os.Stat(dest); err == nil && !*force {
			return usagef("Pass --force to overwrite, or give another path.", "%s exists", dest)
		}
		if tmp, err = os.CreateTemp(filepath.Dir(dest), ".morcore-get-*"); err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		w = tmp
	}
	err = tui.Spin("Retrieving "+meta.Filename+" and checking it against its CID", func() error {
		resp, err := c.Stream("GET", "/v1/files/"+url.PathEscape(meta.ID)+"/content", "", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		n, err := io.Copy(w, resp.Body)
		if err == nil && n != meta.Bytes {
			err = fmt.Errorf("download ended after %d of %d bytes", n, meta.Bytes)
		}
		return err
	})
	if tmp != nil {
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), dest)
		}
	}
	if err != nil {
		return err
	}
	if dest != "-" {
		a.ok("saved %s (%s, verified against %s)", dest, config.FormatBytes(uint64(meta.Bytes)), shortHash(meta.CID))
	}
	return nil
}

func storageRemove(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("Find ids with `morcore storage ls`.", "storage rm needs a file id")
	}
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	if err := c.Do("DELETE", "/v1/files/"+url.PathEscape(args[0]), nil, nil); err != nil {
		return err
	}
	a.ok("deleted %s; leases with no files left are closed and their unused stake returns", args[0])
	return nil
}

func storageStatus(a *App, args []string) error {
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	var v gateway.StorageView
	if err := c.Do("GET", "/admin/storage", nil, &v); err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(v)
	}
	st := v.Stats
	staked, _ := new(big.Int).SetString(st.Staked, 10)
	a.info("%s  %d file(s), %s (%s on providers incl. copies and encryption)", tui.Heading.Render("Storage"), st.Files,
		config.FormatBytes(uint64(st.Bytes)), config.FormatBytes(st.StoredBytes))
	a.info("%s  %d provider(s), %d live lease(s), %s staked", tui.Subtle.Render("       "), st.Providers, st.Leases, units.MOR(staked))
	if !st.CoveredUntil.IsZero() {
		a.info("%s  paid until %s; the node renews leases before they end", tui.Subtle.Render("       "), st.CoveredUntil.Local().Format("Jan 2 15:04"))
	}
	if st.AuditFailures > 0 {
		a.warn("%d copy(ies) failed their last audit; the node replaces copies that fail twice", st.AuditFailures)
	}
	if len(v.Leases) == 0 {
		return nil
	}
	fmt.Fprintln(a.Out)
	t := a.table()
	header(t, "PROVIDER", "QUOTA", "PRICE", "STAKE", "ENDS", "SESSION")
	for _, l := range v.Leases {
		price := "-"
		if l.Price != nil {
			price = perHour(l.Price.String())
		}
		stake := "-"
		if l.Stake != nil {
			stake = units.MOR(l.Stake)
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", tui.ShortAddr(l.Provider.Hex()), config.FormatBytes(l.Quota), price, stake,
			l.EndsAt.Local().Format("Jan 2 15:04"), tui.Subtle.Render(shortHash(l.Session.Hex())))
	}
	return t.Flush()
}

func storageProviders(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	var offers []storage.Offer
	if c, ok := a.running(); ok {
		var out struct{ Offers []storage.Offer }
		if err := c.Do("GET", "/admin/storage/offers", nil, &out); err != nil {
			return err
		}
		offers = out.Offers
	} else {
		ro, err := a.readOnly(cfg)
		if err != nil {
			return err
		}
		ctx, cancel := timeout(90 * time.Second)
		defer cancel()
		if err := tui.Spin("Reading storage offers on "+cfg.NetworkInfo().Label, func() error {
			var err error
			offers, err = storage.ListOffers(ctx, ro.market, commonAddr(cfg.Wallet.Address))
			return err
		}); err != nil {
			return err
		}
	}
	if a.JSON {
		return a.printJSON(offers)
	}
	if len(offers) == 0 {
		a.info("No storage offers on %s yet. Providers list one with %s.", cfg.NetworkInfo().Label, tui.Code.Render("morcore provider storage-listing"))
		return nil
	}
	lease := cfg.Storage.Lease.Duration
	t := a.table()
	header(t, "PROVIDER", "PER LEASE", "PER GiB-MONTH", "LEASE ("+shortDuration(lease)+")", "LISTING")
	for _, o := range offers {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", tui.ShortAddr(o.Provider.Hex()), config.FormatBytes(o.Quota), units.MOR(o.PerGiBMonth()),
			units.MOR(o.LeaseCost(lease)), tui.Subtle.Render(o.Listing.Alias))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Subtle.Render("Cheapest per byte first. A lease buys its full quota; the stake it locks comes back when it ends."))
	return nil
}

func shortDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
}

func storageAudit(a *App, args []string) error {
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	path := "/admin/storage/audit"
	if len(args) == 1 {
		path += "?file=" + url.QueryEscape(args[0])
	}
	var out struct {
		Results []storage.AuditResult `json:"results"`
	}
	if err := tui.Spin("Challenging providers to prove they hold random blocks", func() error {
		return c.Do("POST", path, nil, &out)
	}); err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(out.Results)
	}
	if len(out.Results) == 0 {
		a.info("Nothing to audit.")
		return nil
	}
	failed := 0
	for _, r := range out.Results {
		name := r.Name
		if name == "" {
			name = r.File
		}
		if r.OK {
			a.ok("%s at %s: holds block at byte %d", name, tui.ShortAddr(r.Provider.Hex()), r.Offset)
		} else {
			failed++
			fmt.Fprintf(a.Out, "%s %s at %s: %s\n", tui.CrossMk, name, tui.ShortAddr(r.Provider.Hex()), r.Error)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d copies failed (the node repairs copies that fail twice)", failed, len(out.Results))
	}
	return nil
}

func storageRecover(a *App, args []string) error {
	c, err := a.storageClient()
	if err != nil {
		return err
	}
	var out struct {
		Recovered int `json:"recovered"`
	}
	if err := tui.Spin("Finding your storage leases on-chain and listing what providers hold", func() error {
		return c.Do("POST", "/admin/storage/recover", nil, &out)
	}); err != nil {
		return err
	}
	if out.Recovered == 0 {
		a.info("Nothing new: every file providers hold for this wallet is already listed.")
		return nil
	}
	a.ok("recovered %d file(s); see %s", out.Recovered, tui.Code.Render("morcore storage ls"))
	return nil
}
