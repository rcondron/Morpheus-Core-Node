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
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// failedAudits is how many consecutive failed audits make a copy bad.
const failedAudits = 2

// Run keeps the store's promises while the node runs: it renews leases
// before they end, closes leases that no longer hold files, retries
// deletions, audits copies and repairs or replaces failing ones.
func (s *Store) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.Maintain(ctx)
		select {
		case <-ctx.Done():
			s.save()
			return
		case <-tick.C:
		}
	}
}

// Maintain runs one keeper pass (Run does every minute).
func (s *Store) Maintain(ctx context.Context) {
	s.renew(ctx)
	s.prune(ctx)
	s.retryDeletes(ctx)
	s.auditDue(ctx)
	s.replenish(ctx)
	s.save()
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

// renew leases more room wherever the leases that outlast the renewal window
// no longer cover the bytes a provider holds for us.
func (s *Store) renew(ctx context.Context) {
	horizon := time.Now().Add(s.opt.RenewBefore)
	type gap struct {
		provider common.Address
		deficit  uint64
		listing  common.Hash
	}
	var gaps []gap
	s.mu.Lock()
	for p, used := range s.usedLocked() {
		if used == 0 || time.Now().Before(s.retry["p:"+p.Hex()]) {
			continue
		}
		if quota, _ := s.quotaLocked(p, horizon); quota < used {
			g := gap{provider: p, deficit: used - quota}
			if l := s.lastLeaseLocked(p); l != nil {
				g.listing = l.Listing
			}
			gaps = append(gaps, g)
		}
	}
	s.mu.Unlock()
	if len(gaps) == 0 {
		return
	}
	offers, err := s.Offers(ctx)
	if err != nil {
		s.log.Warn("cannot renew storage leases: marketplace unavailable", "err", err)
		return
	}
	for _, g := range gaps {
		key := "p:" + g.provider.Hex()
		o, ok := offerFrom(offers, g.provider, g.listing)
		if !ok {
			s.log.Warn("storage provider no longer sells storage; moving its files", "provider", g.provider.Hex())
			s.bus.Emit(events.Warn, "storage", fmt.Sprintf("%s stopped selling storage; moving its files", short(g.provider.Hex())))
			s.evacuate(g.provider, "the provider stopped selling storage")
			s.backoff(key, 15*time.Minute)
			continue
		}
		for n := (g.deficit + o.Quota - 1) / o.Quota; n > 0; n-- {
			if _, err := s.lease(ctx, o); err != nil {
				s.log.Warn("could not renew a storage lease", "provider", g.provider.Hex(), "err", err)
				s.bus.Emit(events.Warn, "storage", fmt.Sprintf("renewing storage with %s failed: %v", short(g.provider.Hex()), err))
				s.backoff(key, 5*time.Minute)
				break
			}
		}
	}
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
		switch {
		case !l.EndsAt.After(now):
			list = append(list, closing{l, "lease ended"})
		case used[l.Provider] == 0 && now.Sub(l.OpenedAt) > 10*time.Minute:
			list = append(list, closing{l, "no files stored"})
		}
	}
	s.mu.Unlock()
	for _, c := range list {
		l := c.l
		if err := s.leaser.CloseLease(ctx, l.session(), c.reason); err != nil {
			s.log.Warn("could not close a storage lease", "session", l.Session.Hex(), "reason", c.reason, "err", err)
			s.backoff("l:"+l.Session.Hex(), 15*time.Minute)
			continue
		}
		s.mu.Lock()
		delete(s.leases, l.Session)
		s.dirty = true
		s.mu.Unlock()
	}
}

// retryDeletes asks providers again to drop deleted files' copies. Copies at
// providers we no longer lease from are left to lapse.
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
		s.mu.Lock()
		_, leased := s.quotaLocked(o.Provider, time.Now())
		s.mu.Unlock()
		if leased == nil {
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
		s.challenge(ctx, j.f, j.c)
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
// than Replicas, a few files per pass.
func (s *Store) replenish(ctx context.Context) {
	var work []string
	s.mu.Lock()
	for _, f := range s.sortedFilesLocked() {
		healthy, bad := split(f)
		if (len(bad) > 0 || len(healthy) < s.opt.Replicas) && !time.Now().Before(s.retry["f:"+f.ID]) && len(work) < 4 {
			work = append(work, f.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range work {
		if err := s.repair(ctx, id); err != nil {
			s.log.Warn("could not repair a stored file", "file", id, "err", err)
			s.backoff("f:"+id, 30*time.Minute)
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

// repair rebuilds a file's copies from one that is intact: new copies go to
// providers that do not hold it, then bad copies are dropped.
func (s *Store) repair(ctx context.Context, id string) error {
	f, err := s.File(id)
	if err != nil {
		return nil // deleted meanwhile
	}
	healthy, bad := split(f)
	need := s.opt.Replicas - len(healthy)
	if need <= 0 && len(bad) == 0 {
		return nil
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
	exclude := map[common.Address]bool{}
	for _, c := range f.Copies {
		exclude[c.Provider] = true
	}
	var added []*Copy
	if need > 0 {
		var releases []func()
		var errs []string
		added, releases, errs = s.addCopies(ctx, f, data, need, exclude)
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		if len(added) == 0 {
			return fmt.Errorf("no provider took a new copy: %s", strings.Join(errs, "; "))
		}
	}
	s.mu.Lock()
	live := s.files[id]
	if live == nil {
		s.mu.Unlock()
		return nil
	}
	live.Copies = append(live.Copies, added...)
	var dropped []*Copy
	if len(healthy)+len(added) > 0 { // never drop the last copy
		kept := live.Copies[:0]
		for _, c := range live.Copies {
			if c.Failures >= failedAudits {
				dropped = append(dropped, c)
			} else {
				kept = append(kept, c)
			}
		}
		live.Copies = kept
	}
	s.dirty = true
	s.mu.Unlock()
	s.save()
	for _, c := range dropped {
		if err := s.deleteCopy(ctx, c.peer(), f.CID); err != nil {
			s.mu.Lock()
			s.orphans = append(s.orphans, Orphan{CID: f.CID, Provider: c.Provider, Endpoint: c.Endpoint})
			s.dirty = true
			s.mu.Unlock()
		}
	}
	s.log.Info("stored file repaired", "file", id, "added", len(added), "dropped", len(dropped))
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("repaired %s: %d new copy(ies), %d bad dropped", displayName(f), len(added), len(dropped)))
	return nil
}

// Recover rebuilds the manifest after it was lost (on a new machine, say):
// it finds the wallet's storage leases among its recent sessions, lists what
// each provider holds and adds the files missing locally, reading names back
// from encrypted headers. It returns how many files it added.
func (s *Store) Recover(ctx context.Context) (int, error) {
	ids, err := s.k.RecentUserSessionIDs(ctx, s.w.Address(), 500)
	if err != nil {
		return 0, err
	}
	sessions, err := s.k.GetSessions(ctx, ids)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	providers := map[common.Address]string{}
	for _, cs := range sessions {
		// Providers keep files for a grace period after the last lease.
		if cs.EndsAt == nil || cs.EndsAtTime().Before(now.Add(-7*24*time.Hour)) {
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
		if cs.Closed() || !now.Before(cs.EndsAtTime()) {
			continue
		}
		s.mu.Lock()
		if s.leases[cs.ID] == nil {
			s.leases[cs.ID] = &Lease{
				Session: cs.ID, Listing: common.Hash(bid.ModelId), ListingName: model.Name, Bid: common.Hash(cs.BidId),
				Provider: bid.Provider, Endpoint: ep, Quota: marketplace.StorageQuota(model.Tags), Price: bid.PricePerSecond,
				Stake: cs.Stake, OpenedAt: cs.OpenedAtTime(), EndsAt: cs.EndsAtTime(),
			}
			s.dirty = true
		}
		s.mu.Unlock()
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
// copy of a known one. It reports whether the file was new.
func (s *Store) addRecovered(ctx context.Context, p peer, lf morrpc.StorageFile) bool {
	cp := &Copy{Provider: p.provider, Endpoint: p.endpoint, StoredAt: lf.StoredAt}
	s.mu.Lock()
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
