package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// ErrNotFound is returned for unknown file ids.
var ErrNotFound = errors.New("no such file")

// Leaser opens and closes storage sessions (the consumer's session manager).
type Leaser interface {
	OpenLease(ctx context.Context, model *marketplace.Model, bidID common.Hash, duration time.Duration) (*consumer.Session, error)
	CloseLease(ctx context.Context, s *consumer.Session, reason string) error
	Authorized(ctx context.Context, provider, signer common.Address) error
}

// StoreOptions configure the user's side of marketplace storage.
type StoreOptions struct {
	Path          string // manifest (storage.json)
	TempDir       string // spool directory for uploads and downloads
	Lease         time.Duration
	RenewBefore   time.Duration
	Replicas      int
	Encrypt       bool
	AuditInterval time.Duration
	MaxStake      *big.Int         // cap on MOR staked in live leases (nil or 0: none)
	MaxPrice      *big.Int         // cap on MOR per GiB-month (nil or 0: none)
	Providers     []common.Address // only use these providers (empty: any)
}

// File is a stored file as the manifest records it.
type File struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"contentType,omitempty"`
	Purpose     string    `json:"purpose,omitempty"`
	Owner       string    `json:"owner,omitempty"` // API key id that stored it (empty: the node operator)
	Size        int64     `json:"size"`            // the user's bytes
	StoredBytes uint64    `json:"storedBytes"`     // bytes on IPFS (with encryption overhead)
	CID         string    `json:"cid"`
	Encrypted   bool      `json:"encrypted"`
	Created     time.Time `json:"created"`
	Copies      []*Copy   `json:"copies"`
}

// Copy is one provider's replica of a file.
type Copy struct {
	Provider common.Address `json:"provider"`
	Endpoint string         `json:"endpoint"`
	StoredAt int64          `json:"storedAt"`
	Audited  time.Time      `json:"audited,omitzero"`
	AuditErr string         `json:"auditError,omitempty"`
	Failures int            `json:"failures,omitempty"` // consecutive failed audits
}

// Lease is a storage session this store opened.
type Lease struct {
	Session     common.Hash      `json:"session"`
	Listing     common.Hash      `json:"listing"`
	ListingName string           `json:"listingName,omitempty"`
	Bid         common.Hash      `json:"bid"`
	Provider    common.Address   `json:"provider"`
	Endpoint    string           `json:"endpoint"`
	ProviderKey morrpc.HexString `json:"providerKey,omitempty"`
	Quota       uint64           `json:"quotaBytes"`
	Price       *big.Int         `json:"pricePerSecond"`
	Stake       *big.Int         `json:"stake"`
	OpenedAt    time.Time        `json:"openedAt"`
	EndsAt      time.Time        `json:"endsAt"`
	// Attached is set once the provider registered the lease; the keeper
	// retries until it has.
	Attached bool `json:"attached"`
}

// session rebuilds the consumer session for closing the lease.
func (l *Lease) session() *consumer.Session {
	return &consumer.Session{
		ID: l.Session, ModelID: l.Listing, ModelName: l.ListingName, BidID: l.Bid, Provider: l.Provider,
		Endpoint: l.Endpoint, ProviderKey: l.ProviderKey, Price: l.Price, Stake: l.Stake,
		OpenedAt: l.OpenedAt, EndsAt: l.EndsAt, Direct: true,
	}
}

// Store keeps the wallet's files with marketplace storage providers. Files
// are encrypted to the wallet key by default and identified by the CID the
// store computes itself; each provider holds them under leases (storage
// sessions paid directly to it) that the store renews while files remain,
// and is audited with Merkle proofs of randomly chosen blocks.
type Store struct {
	opt    StoreOptions
	w      *wallet.Wallet
	k      *chain.Contracts
	market *marketplace.Market
	leaser Leaser
	rpc    *morrpc.Client
	bus    *events.Bus
	log    *slog.Logger

	// placeMu serializes choosing a provider (and leasing room there), so
	// concurrent uploads do not both claim the same free space.
	placeMu sync.Mutex

	mu      sync.Mutex
	files   map[string]*File
	leases  map[common.Hash]*Lease
	orphans []Orphan                  // provider copies still to delete
	pending map[common.Address]uint64 // bytes being uploaded, per provider
	retry   map[string]time.Time      // keeper backoff per provider or file
	fails   map[string]int            // consecutive failures behind a backoff
	missing map[common.Address]int    // renewal passes that found no offer from a provider
	infos   map[common.Address]*providerInfo
	dirty   bool
}

// NewStore loads the manifest and returns the store; call Run to keep leases
// renewed and copies audited.
func NewStore(opt StoreOptions, w *wallet.Wallet, k *chain.Contracts, market *marketplace.Market, leaser Leaser, rpc *morrpc.Client, bus *events.Bus, log *slog.Logger) *Store {
	if opt.Replicas < 1 {
		opt.Replicas = 1
	}
	limited := *rpc
	limited.MaxReplyBytes = maxReplyBytes
	s := &Store{
		opt: opt, w: w, k: k, market: market, leaser: leaser, rpc: &limited, bus: bus, log: log,
		files: map[string]*File{}, leases: map[common.Hash]*Lease{}, pending: map[common.Address]uint64{},
		retry: map[string]time.Time{}, fails: map[string]int{}, missing: map[common.Address]int{}, infos: map[common.Address]*providerInfo{},
	}
	s.load()
	return s
}

type manifest struct {
	Version int      `json:"version"`
	Files   []*File  `json:"files"`
	Leases  []*Lease `json:"leases"`
	Orphans []Orphan `json:"orphans,omitempty"`
}

// Orphan is a deleted file's copy a provider has not confirmed removing.
type Orphan struct {
	CID      string         `json:"cid"`
	Provider common.Address `json:"provider"`
	Endpoint string         `json:"endpoint"`
	Since    time.Time      `json:"since"`
}

func (s *Store) load() {
	if s.opt.Path == "" {
		return
	}
	var m manifest
	if _, err := store.ReadJSON(s.opt.Path, &m); err != nil {
		s.log.Warn("could not read the storage manifest", "err", err)
		return
	}
	for _, f := range m.Files {
		s.files[f.ID] = f
	}
	for _, l := range m.Leases {
		s.leases[l.Session] = l
	}
	s.orphans = m.Orphans
}

func (s *Store) save() {
	if s.opt.Path == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	m := manifest{Version: 1, Files: s.sortedFilesLocked(), Leases: make([]*Lease, 0, len(s.leases)), Orphans: s.orphans}
	for _, l := range s.leases {
		m.Leases = append(m.Leases, l)
	}
	sort.Slice(m.Leases, func(i, j int) bool { return m.Leases[i].OpenedAt.Before(m.Leases[j].OpenedAt) })
	err := store.WriteJSON(s.opt.Path, m, 0o600)
	if err == nil {
		s.dirty = false
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("could not save the storage manifest", "err", err)
	}
}

func (s *Store) sortedFilesLocked() []*File {
	out := make([]*File, 0, len(s.files))
	for _, f := range s.files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (f *File) clone() *File {
	c := *f
	c.Copies = make([]*Copy, len(f.Copies))
	for i, cp := range f.Copies {
		x := *cp
		c.Copies[i] = &x
	}
	return &c
}

// Files returns the stored files, oldest first.
func (s *Store) Files() []*File {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sortedFilesLocked()
	for i, f := range list {
		list[i] = f.clone()
	}
	return list
}

// File returns one file's record.
func (s *Store) File(id string) (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.files[id]
	if f == nil {
		return nil, ErrNotFound
	}
	return f.clone(), nil
}

// SetPurpose records a file's purpose (OpenAI Files API metadata).
func (s *Store) SetPurpose(id, purpose string) {
	s.mu.Lock()
	if f := s.files[id]; f != nil && f.Purpose != purpose {
		f.Purpose, s.dirty = purpose, true
	}
	s.mu.Unlock()
	s.save()
}

// Leases returns the live leases, soonest ending first.
func (s *Store) Leases() []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []Lease
	for _, l := range s.leases {
		if l.EndsAt.After(now) {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndsAt.Before(out[j].EndsAt) })
	return out
}

// Staked is the MOR held in live leases.
func (s *Store) Staked() *big.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stakedLocked()
}

func (s *Store) stakedLocked() *big.Int {
	sum := new(big.Int)
	now := time.Now()
	for _, l := range s.leases {
		if l.EndsAt.After(now) && l.Stake != nil {
			sum.Add(sum, l.Stake)
		}
	}
	return sum
}

// StoreStats summarize the store for status displays.
type StoreStats struct {
	Files         int       `json:"files"`
	Bytes         int64     `json:"bytes"`
	StoredBytes   uint64    `json:"storedBytes"`
	Providers     int       `json:"providers"`
	Leases        int       `json:"leases"`
	Staked        string    `json:"staked"` // MOR wei
	CoveredUntil  time.Time `json:"coveredUntil,omitzero"`
	AuditFailures int       `json:"auditFailures"`
}

func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := StoreStats{Files: len(s.files), Staked: s.stakedLocked().String()}
	providers := map[common.Address]bool{}
	for _, f := range s.files {
		st.Bytes += f.Size
		st.StoredBytes += f.StoredBytes * uint64(len(f.Copies))
		for _, c := range f.Copies {
			providers[c.Provider] = true
			if c.Failures > 0 {
				st.AuditFailures++
			}
		}
	}
	st.Providers = len(providers)
	now := time.Now()
	for _, l := range s.leases {
		if l.EndsAt.After(now) {
			st.Leases++
			if l.EndsAt.After(st.CoveredUntil) {
				st.CoveredUntil = l.EndsAt
			}
		}
	}
	return st
}

// usedLocked returns the bytes each provider counts against our quota: one
// count per distinct CID, plus uploads in progress.
func (s *Store) usedLocked() map[common.Address]uint64 {
	seen := map[common.Address]map[string]bool{}
	used := map[common.Address]uint64{}
	for _, f := range s.files {
		for _, c := range f.Copies {
			if seen[c.Provider] == nil {
				seen[c.Provider] = map[string]bool{}
			}
			if !seen[c.Provider][f.CID] {
				seen[c.Provider][f.CID] = true
				used[c.Provider] += f.StoredBytes
			}
		}
	}
	for p, n := range s.pending {
		used[p] += n
	}
	return used
}

// quotaLocked sums the quota of p's leases that last past t and returns the
// one that lasts longest.
func (s *Store) quotaLocked(p common.Address, t time.Time) (quota uint64, latest *Lease) {
	for _, l := range s.leases {
		if l.Provider != p || !l.EndsAt.After(t) {
			continue
		}
		quota += l.Quota
		if latest == nil || l.EndsAt.After(latest.EndsAt) {
			latest = l
		}
	}
	return quota, latest
}

func (s *Store) allowed(p common.Address) bool {
	if len(s.opt.Providers) == 0 {
		return true
	}
	for _, a := range s.opt.Providers {
		if a == p {
			return true
		}
	}
	return false
}

func (s *Store) tempFile() (*os.File, error) {
	if s.opt.TempDir != "" {
		if err := os.MkdirAll(s.opt.TempDir, 0o700); err != nil {
			return nil, err
		}
	}
	return os.CreateTemp(s.opt.TempDir, "morcore-file-*")
}

func drop(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

func newFileID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "file-" + hex.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
