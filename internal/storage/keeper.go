package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

const (
	// failedAudits is how many consecutive failed audits make a copy bad.
	failedAudits = 2
	// missingPasses is how many renewal passes in a row must find no
	// acceptable offer from a provider before its files move elsewhere.
	missingPasses = 3
	// orphanTTL is how long a deletion is retried; by then the copy has
	// lapsed with its leases anyway.
	orphanTTL = 7 * 24 * time.Hour
	// syncWindow is how far back the keeper looks for storage sessions the
	// manifest does not know.
	syncWindow = 3 * 24 * time.Hour
)

// Run keeps the store's promises while the node runs. One loop keeps files
// paid for (renewing, attaching and closing leases, retrying deletions); a
// second audits copies and repairs failing ones, so a slow provider cannot
// hold renewals up.
func (s *Store) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.loop(ctx, s.renewPass)
	}()
	go func() {
		defer wg.Done()
		s.loop(ctx, s.carePass)
	}()
	wg.Wait()
	s.save()
}

func (s *Store) loop(ctx context.Context, pass func(context.Context)) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		pass(ctx)
		s.save()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Maintain runs one pass of both keeper loops.
func (s *Store) Maintain(ctx context.Context) {
	s.renewPass(ctx)
	s.carePass(ctx)
	s.save()
}

func (s *Store) renewPass(ctx context.Context) {
	s.renew(ctx)
	s.attachPending(ctx)
	s.prune(ctx)
	s.retryDeletes(ctx)
	if s.due("sync") {
		s.backoff("sync", time.Hour)
		if _, err := s.syncSessions(ctx, syncWindow); err != nil {
			s.log.Debug("could not check the wallet's storage sessions", "err", err)
		}
	}
}

func (s *Store) carePass(ctx context.Context) {
	s.auditDue(ctx)
	s.replenish(ctx)
}

func (s *Store) due(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !time.Now().Before(s.retry[key])
}

func (s *Store) backoff(key string, d time.Duration) {
	s.mu.Lock()
	s.retry[key] = time.Now().Add(d)
	s.mu.Unlock()
}

// failed counts a failure behind key and backs off exponentially from base,
// up to 12 hours.
func (s *Store) failed(key string, base time.Duration) {
	s.mu.Lock()
	n := min(s.fails[key], 5)
	s.fails[key]++
	s.retry[key] = time.Now().Add(min(base<<n, 12*time.Hour))
	s.mu.Unlock()
}

func (s *Store) succeeded(key string) {
	s.mu.Lock()
	delete(s.fails, key)
	delete(s.retry, key)
	s.mu.Unlock()
}

// renew leases more room wherever the leases that outlast the renewal window
// no longer cover the bytes a provider holds for us.
func (s *Store) renew(ctx context.Context) {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	now := time.Now()
	horizon := now.Add(s.opt.RenewBefore)
	type gap struct {
		provider common.Address
		deficit  uint64
		last     *Lease // the provider's lease that lasts longest
	}
	var gaps []gap
	s.mu.Lock()
	for p, used := range s.usedLocked() {
		if used == 0 || now.Before(s.retry["p:"+p.Hex()]) {
			continue
		}
		if quota, _ := s.quotaLocked(p, horizon); quota < used {
			g := gap{provider: p, deficit: used - quota}
			if l := s.lastLeaseLocked(p); l != nil {
				c := *l
				g.last = &c
			}
			gaps = append(gaps, g)
		}
	}
	s.mu.Unlock()
	if len(gaps) == 0 {
		return
	}
	offers, err := s.Offers(ctx)
	partial := errors.Is(err, ErrPartialOffers)
	if err != nil && !partial {
		s.log.Warn("cannot renew storage leases: marketplace unavailable", "err", err)
		return
	}
	for _, g := range gaps {
		key := "p:" + g.provider.Hex()
		var listing common.Hash
		if g.last != nil {
			listing = g.last.Listing
		}
		o, ok := offerFrom(offers, g.provider, listing)
		if !ok {
			if partial {
				continue // an incomplete read proves nothing; try again next pass
			}
			s.mu.Lock()
			s.missing[g.provider]++
			n := s.missing[g.provider]
			s.mu.Unlock()
			if n < missingPasses {
				continue
			}
			s.log.Warn("storage provider no longer offers storage at an acceptable price; moving its files", "provider", g.provider.Hex())
			s.bus.Emit(events.Warn, "storage", fmt.Sprintf("%s no longer offers storage (or only above storage.max_price); moving its files", short(g.provider.Hex())))
			s.evacuate(g.provider, "the provider no longer offers storage at an acceptable price")
			s.backoff(key, 15*time.Minute)
			continue
		}
		s.mu.Lock()
		delete(s.missing, g.provider)
		s.mu.Unlock()
		// A provider that raised its price sharply keeps the files only if
		// no one is cheaper. While its current lease runs, move them; once
		// it ended, renew anyway rather than risk losing them.
		if g.last != nil && jumped(o, g.last) {
			was, is := units.MOR(Offer{Price: g.last.Price, Quota: g.last.Quota}.PerGiBMonth()), units.MOR(o.PerGiBMonth())
			if _, cheaper := cheaperElsewhere(offers, g.provider, o); cheaper && g.last.EndsAt.After(now) {
				s.bus.Emit(events.Warn, "storage", fmt.Sprintf("%s raised its price from %s to %s per GiB-month; moving its files to a cheaper provider", short(g.provider.Hex()), was, is))
				s.evacuate(g.provider, "the provider raised its price")
				s.backoff(key, 15*time.Minute)
				continue
			}
			s.bus.Emit(events.Warn, "storage", fmt.Sprintf("%s raised its price from %s to %s per GiB-month; renewing (no cheaper provider)", short(g.provider.Hex()), was, is))
		}
		for n := (g.deficit + o.Quota - 1) / o.Quota; n > 0; n-- {
			l, err := s.lease(ctx, o)
			if err != nil {
				s.log.Warn("could not renew a storage lease", "provider", g.provider.Hex(), "err", err)
				s.bus.Emit(events.Warn, "storage", fmt.Sprintf("renewing storage with %s failed: %v", short(g.provider.Hex()), err))
				s.backoff(key, 5*time.Minute)
				break
			}
			if !l.EndsAt.After(horizon) {
				// The marketplace granted less time than the renewal window:
				// buying more now would only buy more short leases.
				s.log.Warn("storage lease ends inside the renewal window; lower storage.renew_before", "provider", g.provider.Hex(), "ends", l.EndsAt)
				s.bus.Emit(events.Warn, "storage", "a storage lease was shorter than storage.renew_before; lower it")
				s.backoff(key, max(time.Until(l.EndsAt)/2, time.Minute))
				break
			}
		}
	}
}

// jumped reports an offer more than 25% dearer per byte than a lease.
func jumped(o Offer, last *Lease) bool {
	if last.Price == nil || last.Quota == 0 {
		return false
	}
	limit := Offer{Price: last.Price, Quota: last.Quota}.PerByte()
	limit.Mul(limit, big.NewInt(5)).Div(limit, big.NewInt(4))
	return o.PerByte().Cmp(limit) > 0
}

// cheaperElsewhere finds an offer from another provider cheaper than o.
func cheaperElsewhere(offers []Offer, p common.Address, o Offer) (Offer, bool) {
	for _, x := range offers {
		if x.Provider != p && cheaper(x, o) {
			return x, true
		}
	}
	return Offer{}, false
}

func (s *Store) lastLeaseLocked(p common.Address) *Lease {
	var last *Lease
	for _, l := range s.leases {
		if l.Provider == p && (last == nil || l.EndsAt.After(last.EndsAt)) {
			last = l
		}
	}
	return last
}

// offerFrom picks a provider's offer: the listing leased before when still
// offered, else its cheapest (offers come sorted by price per byte).
func offerFrom(offers []Offer, p common.Address, listing common.Hash) (Offer, bool) {
	var best *Offer
	for i := range offers {
		o := &offers[i]
		if o.Provider != p {
			continue
		}
		if o.Listing.ID == listing {
			return *o, true
		}
		if best == nil {
			best = o
		}
	}
	if best == nil {
		return Offer{}, false
	}
	return *best, true
}

// attachPending retries registering leases the provider has not
// acknowledged, so a paid lease always extends the provider's cover.
func (s *Store) attachPending(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var list []*Lease
	for _, l := range s.leases {
		if !l.Attached && l.EndsAt.After(now) {
			c := *l
			list = append(list, &c)
		}
	}
	s.mu.Unlock()
	for _, l := range list {
		if err := s.attach(ctx, l); err != nil {
			s.log.Debug("lease not registered with the provider yet", "session", l.Session.Hex(), "err", err)
		}
	}
}

// prune closes leases that ended, which settles the provider's pay and
// returns the rest of the stake, and closes live leases at providers that
// no longer hold our files, which returns their stake early.
func (s *Store) prune(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	used := s.usedLocked()
	type closing struct {
		l      *Lease
		reason string
	}
	var list []closing
	for id, l := range s.leases {
		if now.Before(s.retry["l:"+id.Hex()]) {
			continue
		}
		c := *l
		switch {
		case !l.EndsAt.After(now):
			list = append(list, closing{&c, "lease ended"})
		case used[l.Provider] == 0 && now.Sub(l.OpenedAt) > 10*time.Minute:
			list = append(list, closing{&c, "no files stored"})
		}
	}
	s.mu.Unlock()
	for _, c := range list {
		l := c.l
		if len(l.ProviderKey) == 0 {
			// Adopted leases lack the provider's key; with it the close
			// carries the provider's own usage report.
			if pi, err := s.info(ctx, l.peer()); err == nil {
				l.ProviderKey = pi.key
			}
		}
		if err := s.leaser.CloseLease(ctx, l.session(), c.reason); err != nil {
			s.log.Warn("could not close a storage lease", "session", l.Session.Hex(), "reason", c.reason, "err", err)
			s.failed("l:"+l.Session.Hex(), 15*time.Minute)
			continue
		}
		s.succeeded("l:" + l.Session.Hex())
		s.mu.Lock()
		delete(s.leases, l.Session)
		s.dirty = true
		s.mu.Unlock()
	}
}

// retryDeletes asks providers again to drop deleted files' copies (they
// honour deletes without a lease), until the copies would have lapsed.
func (s *Store) retryDeletes(ctx context.Context) {
	if !s.due("deletes") {
		return
	}
	s.backoff("deletes", 15*time.Minute)
	s.mu.Lock()
	list := append([]Orphan(nil), s.orphans...)
	s.mu.Unlock()
	if len(list) == 0 {
		return
	}
	var keep []Orphan
	for _, o := range list {
		if !o.Since.IsZero() && time.Since(o.Since) > orphanTTL {
			continue
		}
		if err := s.deleteCopy(ctx, peer{o.Provider, o.Endpoint}, o.CID); err != nil {
			keep = append(keep, o)
		}
	}
	s.mu.Lock()
	s.orphans = append(keep, s.orphans[len(list):]...)
	s.dirty = true
	s.mu.Unlock()
}

func (s *Store) orphanedLocked(cid string, p common.Address) bool {
	for _, o := range s.orphans {
		if o.CID == cid && o.Provider == p {
			return true
		}
	}
	return false
}

// AuditResult is the outcome of one proof-of-storage challenge.
type AuditResult struct {
	File     string         `json:"file"`
	Name     string         `json:"name,omitempty"`
	CID      string         `json:"cid"`
	Provider common.Address `json:"provider"`
	Offset   uint64         `json:"offset"`
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
}

// Audit challenges every copy of the given files (all files when none are
// given) to prove it still holds a randomly chosen block.
func (s *Store) Audit(ctx context.Context, ids ...string) ([]AuditResult, error) {
	var files []*File
	if len(ids) == 0 {
		files = s.Files()
	}
	for _, id := range ids {
		f, err := s.File(id)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		files = append(files, f)
	}
	out := []AuditResult{}
	for _, f := range files {
		for _, c := range f.Copies {
			out = append(out, s.challenge(ctx, f, c))
		}
	}
	s.save()
	return out, nil
}

// auditDue challenges a few copies not audited within the audit interval.
func (s *Store) auditDue(ctx context.Context) {
	if s.opt.AuditInterval <= 0 {
		return
	}
	type job struct {
		f *File
		c *Copy
	}
	var jobs []job
	cutoff := time.Now().Add(-s.opt.AuditInterval)
	s.mu.Lock()
	for _, f := range s.sortedFilesLocked() {
		for i, c := range f.Copies {
			last := c.Audited
			if stored := time.Unix(c.StoredAt, 0); stored.After(last) {
				last = stored
			}
			if last.Before(cutoff) && len(jobs) < 8 {
				fc := f.clone()
				jobs = append(jobs, job{fc, fc.Copies[i]})
			}
		}
	}
	s.mu.Unlock()
	for _, j := range jobs {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		s.challenge(cctx, j.f, j.c)
		cancel()
	}
}

// challenge asks one copy to prove it holds a random block of the file.
func (s *Store) challenge(ctx context.Context, f *File, c *Copy) AuditResult {
	off := randBelow(f.StoredBytes)
	nonce := randomHex(16)
	r := AuditResult{File: f.ID, Name: f.Name, CID: f.CID, Provider: c.Provider, Offset: off}
	var proof morrpc.StorageProof
	err := s.call(ctx, c.peer(), morrpc.MethodStorageProve, morrpc.StorageReq{CID: f.CID, Offset: off, Nonce: nonce}, &proof)
	if err == nil {
		err = checkProof(f, off, nonce, &proof)
	}
	r.OK = err == nil
	if err != nil {
		r.Error = err.Error()
	}
	s.mu.Lock()
	if live := s.files[f.ID]; live != nil {
		for _, lc := range live.Copies {
			if lc.Provider != c.Provider {
				continue
			}
			lc.Audited = time.Now().UTC()
			if err == nil {
				lc.AuditErr, lc.Failures = "", 0
			} else {
				lc.AuditErr = r.Error
				lc.Failures++
			}
		}
		s.dirty = true
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("storage audit failed", "file", f.ID, "provider", c.Provider.Hex(), "err", err)
		s.bus.Emit(events.Warn, "storage", fmt.Sprintf("audit of %s at %s failed: %v", displayName(f), short(c.Provider.Hex()), err))
	}
	return r
}

// checkProof verifies a provider's answer to a challenge against the CID
// and size the store computed itself.
func checkProof(f *File, off uint64, nonce string, p *morrpc.StorageProof) error {
	if p.CID != f.CID || p.Offset != off || p.Nonce != nonce {
		return errors.New("the proof answers a different challenge")
	}
	root, err := ipfs.Parse(f.CID)
	if err != nil {
		return err
	}
	blocks := make([][]byte, len(p.Blocks))
	for i, b := range p.Blocks {
		if blocks[i], err = base64.StdEncoding.DecodeString(b); err != nil {
			return errors.New("the proof is not valid base64")
		}
	}
	if err := ipfs.VerifyPath(root, off, f.StoredBytes, blocks); err != nil {
		return fmt.Errorf("invalid proof: %w", err)
	}
	return nil
}

func randBelow(n uint64) uint64 {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, new(big.Int).SetUint64(n))
	if err != nil {
		return 0
	}
	return v.Uint64()
}

// evacuate marks a provider's copies bad so replenish moves them.
func (s *Store) evacuate(p common.Address, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		for _, c := range f.Copies {
			if c.Provider == p && c.Failures < failedAudits {
				c.Failures, c.AuditErr = failedAudits, reason
				s.dirty = true
			}
		}
	}
}

// replenish replaces bad copies and adds copies to files stored with fewer
// than Replicas, a few files per pass, backing off per file.
func (s *Store) replenish(ctx context.Context) {
	type job struct {
		id   string
		size uint64
	}
	var work []job
	s.mu.Lock()
	for _, f := range s.sortedFilesLocked() {
		healthy, bad := split(f)
		if (len(bad) > 0 || len(healthy) < s.opt.Replicas) && !time.Now().Before(s.retry["f:"+f.ID]) && len(work) < 4 {
			work = append(work, job{f.ID, f.StoredBytes})
		}
	}
	s.mu.Unlock()
	for _, j := range work {
		// A transfer that crawls must not hold the loop forever.
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute+time.Duration(j.size/(256<<10))*time.Second)
		err := s.repair(rctx, j.id)
		cancel()
		if err != nil {
			s.log.Warn("could not repair a stored file", "file", j.id, "err", err)
			s.failed("f:"+j.id, 30*time.Minute)
		} else {
			s.succeeded("f:" + j.id)
		}
	}
}

func split(f *File) (healthy, bad []*Copy) {
	for _, c := range f.Copies {
		if c.Failures >= failedAudits {
			bad = append(bad, c)
		} else {
			healthy = append(healthy, c)
		}
	}
	return healthy, bad
}

// repair rebuilds a file's copies: with enough healthy copies it only drops
// the bad ones; otherwise it finds a new provider first, then copies the
// file there from an intact copy and drops the bad ones.
func (s *Store) repair(ctx context.Context, id string) error {
	f, err := s.File(id)
	if err != nil {
		return nil // deleted meanwhile
	}
	healthy, bad := split(f)
	need := s.opt.Replicas - len(healthy)
	if need <= 0 {
		s.dropBad(ctx, f, 0)
		return nil
	}
	exclude := map[common.Address]bool{}
	for _, c := range f.Copies {
		exclude[c.Provider] = true
	}
	if err := s.canPlace(ctx, f.StoredBytes, exclude); err != nil {
		return err
	}
	skip := map[common.Address]bool{}
	for _, c := range bad {
		skip[c.Provider] = true
	}
	data, err := s.fetch(ctx, f, skip)
	if err != nil && len(bad) > 0 {
		data, err = s.fetch(ctx, f, nil) // a failing provider may still serve reads
	}
	if err != nil {
		return err
	}
	defer drop(data)
	added, releases, errs := s.addCopies(ctx, f, data, need, exclude)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	if len(added) == 0 {
		return fmt.Errorf("no provider took a new copy: %s", strings.Join(errs, "; "))
	}
	s.mu.Lock()
	if live := s.files[id]; live != nil {
		live.Copies = append(live.Copies, added...)
		s.dirty = true
	}
	s.mu.Unlock()
	dropped := s.dropBad(ctx, f, len(added))
	s.save()
	s.log.Info("stored file repaired", "file", id, "added", len(added), "dropped", dropped)
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("repaired %s: %d new copy(ies), %d bad dropped", displayName(f), len(added), dropped))
	return nil
}

// dropBad removes a file's bad copies, never its last copy, and asks their
// providers to delete them.
func (s *Store) dropBad(ctx context.Context, f *File, added int) int {
	s.mu.Lock()
	live := s.files[f.ID]
	if live == nil {
		s.mu.Unlock()
		return 0
	}
	healthy, bad := split(live)
	if len(healthy) == 0 || len(bad) == 0 {
		s.mu.Unlock()
		return 0
	}
	live.Copies = healthy
	s.dirty = true
	s.mu.Unlock()
	for _, c := range bad {
		if err := s.deleteCopy(ctx, c.peer(), f.CID); err != nil {
			s.mu.Lock()
			s.orphans = append(s.orphans, Orphan{CID: f.CID, Provider: c.Provider, Endpoint: c.Endpoint, Since: time.Now()})
			s.dirty = true
			s.mu.Unlock()
		}
	}
	return len(bad)
}

// canPlace reports whether a provider outside exclude could take size more
// bytes, before anything is downloaded for it.
func (s *Store) canPlace(ctx context.Context, size uint64, exclude map[common.Address]bool) error {
	horizon := time.Now().Add(s.opt.RenewBefore)
	s.mu.Lock()
	used := s.usedLocked()
	for _, l := range s.leases {
		if !exclude[l.Provider] && s.allowed(l.Provider) {
			if quota, _ := s.quotaLocked(l.Provider, horizon); quota >= used[l.Provider]+size {
				s.mu.Unlock()
				return nil
			}
		}
	}
	s.mu.Unlock()
	offers, err := s.Offers(ctx)
	if err != nil && !errors.Is(err, ErrPartialOffers) {
		return fmt.Errorf("read storage offers: %w", err)
	}
	for _, o := range offers {
		if !exclude[o.Provider] && o.Quota >= size {
			return nil
		}
	}
	return fmt.Errorf("no other provider offers room for %s", config.FormatBytes(size))
}

// syncSessions looks among the wallet's recent sessions for storage leases
// the manifest does not know (a crash between opening and recording one, or
// a lost manifest): live ones are adopted, and ended ones still open are
// closed so their stake returns. It returns the providers of all storage
// sessions in the window.
func (s *Store) syncSessions(ctx context.Context, window time.Duration) (map[common.Address]string, error) {
	ids, err := s.k.RecentUserSessionIDs(ctx, s.w.Address(), 500)
	if err != nil {
		return nil, err
	}
	sessions, err := s.k.GetSessions(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	providers := map[common.Address]string{}
	for _, cs := range sessions {
		if cs.EndsAt == nil || cs.EndsAtTime().Before(now.Add(-window)) {
			continue
		}
		bid, err := s.k.GetBid(ctx, common.Hash(cs.BidId))
		if err != nil {
			continue
		}
		model, err := s.k.GetModel(ctx, common.Hash(bid.ModelId))
		if err != nil || !marketplace.IsStorageTags(model.Tags) {
			continue
		}
		ep, ok := providers[bid.Provider]
		if !ok {
			pr, err := s.k.GetProvider(ctx, bid.Provider)
			if err != nil {
				continue
			}
			ep = morrpc.NormalizeEndpoint(pr.Endpoint)
			providers[bid.Provider] = ep
		}
		s.mu.Lock()
		known := s.leases[cs.ID] != nil
		s.mu.Unlock()
		if known || cs.Closed() {
			continue
		}
		l := &Lease{
			Session: cs.ID, Listing: common.Hash(bid.ModelId), ListingName: model.Name, Bid: common.Hash(cs.BidId),
			Provider: bid.Provider, Endpoint: ep, Quota: marketplace.StorageQuota(model.Tags), Price: bid.PricePerSecond,
			Stake: cs.Stake, OpenedAt: cs.OpenedAtTime(), EndsAt: cs.EndsAtTime(),
		}
		if pi, err := s.info(ctx, l.peer()); err == nil {
			l.ProviderKey = pi.key
		}
		if now.Before(l.EndsAt) {
			s.mu.Lock()
			if s.leases[l.Session] == nil {
				s.leases[l.Session] = l
				s.dirty = true
			}
			s.mu.Unlock()
			s.log.Info("adopted a storage lease", "session", l.Session.Hex(), "provider", l.Provider.Hex())
			continue
		}
		if err := s.leaser.CloseLease(ctx, l.session(), "storage lease ended"); err != nil {
			s.log.Warn("could not close an ended storage session", "session", l.Session.Hex(), "err", err)
		}
	}
	s.save()
	return providers, nil
}

// Recover rebuilds the manifest after it was lost (on a new machine, say):
// it finds the wallet's storage leases among its recent sessions, lists what
// each provider holds and adds the files missing locally, reading names back
// from encrypted headers. It returns how many files it added.
func (s *Store) Recover(ctx context.Context) (int, error) {
	providers, err := s.syncSessions(ctx, 7*24*time.Hour)
	if err != nil {
		return 0, err
	}
	added := 0
	var errs []string
	for p, ep := range providers {
		var list morrpc.StorageList
		if err := s.call(ctx, peer{p, ep}, morrpc.MethodStorageList, morrpc.StorageReq{}, &list); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", short(p.Hex()), err))
			continue
		}
		for _, lf := range list.Files {
			if s.addRecovered(ctx, peer{p, ep}, lf) {
				added++
			}
		}
	}
	s.save()
	if added == 0 && len(errs) > 0 {
		return 0, fmt.Errorf("providers unavailable: %s", strings.Join(errs, "; "))
	}
	return added, nil
}

// addRecovered records a file a provider lists, as a new file or as another
// copy of a known one; copies we deleted stay deleted. It reports whether
// the file was new.
func (s *Store) addRecovered(ctx context.Context, p peer, lf morrpc.StorageFile) bool {
	cp := &Copy{Provider: p.provider, Endpoint: p.endpoint, StoredAt: lf.StoredAt}
	s.mu.Lock()
	if s.orphanedLocked(lf.CID, p.provider) {
		s.mu.Unlock()
		return false
	}
	for _, f := range s.files {
		if f.CID != lf.CID {
			continue
		}
		for _, c := range f.Copies {
			if c.Provider == p.provider {
				s.mu.Unlock()
				return false
			}
		}
		f.Copies = append(f.Copies, cp)
		s.dirty = true
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	f := &File{
		ID: newFileID(), Name: lf.Label, Size: int64(lf.Size), StoredBytes: lf.Size, CID: lf.CID,
		Created: time.Unix(lf.StoredAt, 0).UTC(), Copies: []*Copy{cp},
	}
	if m := s.readMeta(ctx, p, lf.CID, lf.Size); m != nil {
		f.Name, f.ContentType, f.Size, f.Encrypted = m.Name, m.ContentType, m.Size, true
		if m.Created > 0 {
			f.Created = time.Unix(m.Created, 0).UTC()
		}
	}
	if f.Name == "" {
		f.Name = lf.CID
	}
	s.mu.Lock()
	s.files[f.ID] = f
	s.dirty = true
	s.mu.Unlock()
	return true
}

// readMeta reads an encrypted file's header from a provider; nil when the
// file is not encrypted to this wallet.
func (s *Store) readMeta(ctx context.Context, p peer, cid string, size uint64) *Meta {
	lead, err := s.readRange(ctx, p, cid, 0, min(8, size))
	if err != nil || !IsSealed(lead) {
		return nil
	}
	n, err := HeaderLength(lead)
	if err != nil || uint64(n) > size {
		return nil
	}
	hdr, err := s.readRange(ctx, p, cid, 0, uint64(n))
	if err != nil {
		return nil
	}
	m, err := ReadMeta(bytes.NewReader(hdr), s.w)
	if err != nil {
		return nil
	}
	return m
}
