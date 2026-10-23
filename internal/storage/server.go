package storage

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// Transfer sizes. One chunk or range is one MOR-RPC message, which must
// cross the network within the transport's read and write deadlines, so they
// stay small (as the reference node's 1 MiB upload chunks).
const (
	ChunkBytes    = 1 << 20 // upload chunk the server asks for
	maxChunkBytes = 4 << 20 // largest chunk accepted
	maxReadBytes  = 1 << 20 // largest range served by storage.get
	uploadIdle    = 15 * time.Minute
	uploadPrefix  = "morcore-upload-"
	// overQuotaGrace bounds how long files may exceed the live quota: the
	// excess was never paid for.
	overQuotaGrace = 6 * time.Hour
	// paidWindow is how far back paid lease time counts toward the grace
	// period.
	paidWindow = 7 * 24 * time.Hour
	// approvalTTL is how long an approved lease counts against capacity
	// before its session must be attached.
	approvalTTL = 10 * time.Minute
)

// Kubo is the provider's IPFS node (its RPC API; *kubo.Client).
type Kubo interface {
	Version(ctx context.Context) (string, error)
	DagImport(ctx context.Context, car io.Reader) ([]string, error)
	Cat(ctx context.Context, cid string, offset, length int64) (io.ReadCloser, error)
	BlockGet(ctx context.Context, cid string) ([]byte, error)
	PinRm(ctx context.Context, cid string) error
	RepoStat(ctx context.Context) (kubo.RepoStat, error)
}

// ServerOptions configure the provider's storage service.
type ServerOptions struct {
	Listings      []common.Hash // storage listings served (quotas come from their tags)
	MaxFileBytes  uint64
	MaxTotal      uint64 // cap on quota sold across live leases (0: Kubo's StorageMax)
	Grace         time.Duration
	AcceptStipend bool
	Gateway       string
	StatePath     string
	TempDir       string // spool for uploads (default: the OS temp dir)
	Version       string
}

// Server sells IPFS storage: users attach marketplace sessions on storage
// listings as leases, and their files stay pinned while a lease (plus a
// grace period earned by the time paid for) covers them.
type Server struct {
	opt  ServerOptions
	w    *wallet.Wallet
	k    *chain.Contracts
	kubo Kubo
	log  *slog.Logger
	bus  *events.Bus
	// servesFor reports whether this node may act for an on-chain provider
	// (its own address or a contract it owns).
	servesFor func(ctx context.Context, provider common.Address) bool

	mu        sync.Mutex
	quotas    map[common.Hash]uint64
	accounts  map[common.Address]*account
	refs      map[string]int // cid → owners
	uploads   map[string]*pending
	approvals []approval
	seen      map[string]int64 // request hashes already served → expiry (unix)
	lookups   lookupLimiter
	healthy   bool
	dirty     bool
}

type approval struct {
	user    common.Address
	listing common.Hash
	quota   uint64
	at      time.Time
}

type account struct {
	Files  map[string]*fileRec       `json:"files"`
	Leases map[common.Hash]*leaseRec `json:"leases"`
	// OverSince is when the files first exceeded the live quota (unix
	// seconds; 0 while within it).
	OverSince int64 `json:"overSince,omitempty"`
}

type fileRec struct {
	Size     uint64 `json:"size"`
	Label    string `json:"label,omitempty"`
	StoredAt int64  `json:"storedAt"`
}

type leaseRec struct {
	Listing common.Hash `json:"listing"`
	Quota   uint64      `json:"quota"`
	Opened  int64       `json:"openedAt"`
	EndsAt  int64       `json:"endsAt"`
	Closed  int64       `json:"closedAt,omitempty"`
	checked time.Time
}

func (l *leaseRec) end() int64 {
	if l.Closed > 0 && l.Closed < l.EndsAt {
		return l.Closed
	}
	return l.EndsAt
}

type pending struct {
	mu       sync.Mutex // orders the chunks of one upload and its end
	user     common.Address
	cid      string
	size     uint64
	label    string
	received uint64
	f        *os.File
	last     time.Time // last activity (guarded by Server.mu)
	ending   bool      // put.end in progress (guarded by Server.mu)
}

// NewServer builds the storage service. servesFor is the provider's own
// check of which on-chain provider addresses this key serves.
func NewServer(opt ServerOptions, w *wallet.Wallet, k *chain.Contracts, kubo Kubo, servesFor func(context.Context, common.Address) bool, bus *events.Bus, log *slog.Logger) *Server {
	s := &Server{
		opt: opt, w: w, k: k, kubo: kubo, servesFor: servesFor, bus: bus, log: log,
		quotas: map[common.Hash]uint64{}, accounts: map[common.Address]*account{},
		refs: map[string]int{}, uploads: map[string]*pending{}, seen: map[string]int64{},
		lookups: lookupLimiter{rate: 2, burst: 20}, healthy: true,
	}
	if opt.TempDir != "" {
		_ = os.MkdirAll(opt.TempDir, 0o700)
		// Uploads do not survive a restart: clear what an earlier run left.
		if old, err := filepath.Glob(filepath.Join(opt.TempDir, uploadPrefix+"*")); err == nil {
			for _, f := range old {
				os.Remove(f)
			}
		}
	}
	s.load()
	return s
}

// coverage sums the quota of a user's live leases and finds when the last
// one ends.
func (a *account) coverage(now int64) (quota uint64, until int64) {
	for _, l := range a.Leases {
		end := l.end()
		if end > until {
			until = end
		}
		if now < end {
			quota += l.Quota
		}
	}
	return quota, until
}

// grace is how long files outlive the last lease: the configured grace
// period, but no longer than the lease time paid for in the last week, so
// a short (or early-closed) lease cannot buy days of free storage.
func (a *account) grace(limit time.Duration, now time.Time) time.Duration {
	var paid time.Duration
	cutoff := now.Add(-paidWindow).Unix()
	for _, l := range a.Leases {
		start, end := l.Opened, l.end()
		if end <= cutoff {
			continue
		}
		start = max64(start, cutoff)
		if end > start {
			paid += time.Duration(end-start) * time.Second
		}
	}
	return min(limit, paid)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// pruneLeases forgets leases that ended before cutoff, keeping the latest so
// the account's coverage stays known.
func (a *account) pruneLeases(cutoff int64) (changed bool) {
	var latest common.Hash
	var end int64
	for id, l := range a.Leases {
		if e := l.end(); e > end {
			latest, end = id, e
		}
	}
	for id, l := range a.Leases {
		if id != latest && l.end() < cutoff {
			delete(a.Leases, id)
			changed = true
		}
	}
	return changed
}

// newestBeyond lists the newest files that must go to bring usage within
// quota.
func (a *account) newestBeyond(quota uint64) []string {
	type f struct {
		cid string
		*fileRec
	}
	files := make([]f, 0, len(a.Files))
	for cid, r := range a.Files {
		files = append(files, f{cid, r})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].StoredAt > files[j].StoredAt })
	used := a.used()
	var out []string
	for _, x := range files {
		if used <= quota {
			break
		}
		out = append(out, x.cid)
		used -= x.Size
	}
	return out
}

func (a *account) used() uint64 {
	var n uint64
	for _, f := range a.Files {
		n += f.Size
	}
	return n
}

// Serves reports whether a listing is a storage product of this node.
func (s *Server) Serves(listing common.Hash) bool {
	for _, l := range s.opt.Listings {
		if l == listing {
			return true
		}
	}
	return false
}

// Admit decides whether to approve a new session on a storage listing. Quota
// already sold (live leases, and approvals not attached yet) plus this lease
// must fit the capacity.
func (s *Server) Admit(ctx context.Context, listing common.Hash, user common.Address) error {
	quota, err := s.quota(ctx, listing)
	if err != nil {
		return err
	}
	if _, err := s.kubo.Version(ctx); err != nil {
		return errors.New("storage backend is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneApprovalsLocked(now)
	if s.opt.MaxTotal > 0 {
		if quota > s.opt.MaxTotal {
			return fmt.Errorf("this listing's %s quota exceeds the provider's %s storage capacity", config.FormatBytes(quota), config.FormatBytes(s.opt.MaxTotal))
		}
		var committed uint64
		for _, a := range s.accounts {
			q, _ := a.coverage(now.Unix())
			committed += q
		}
		for _, ap := range s.approvals {
			committed += ap.quota
		}
		if committed+quota > s.opt.MaxTotal {
			return errors.New("provider storage is fully committed")
		}
	}
	s.approvals = append(s.approvals, approval{user: user, listing: listing, quota: quota, at: now})
	return nil
}

func (s *Server) pruneApprovalsLocked(now time.Time) {
	kept := s.approvals[:0]
	for _, ap := range s.approvals {
		if now.Sub(ap.at) < approvalTTL {
			kept = append(kept, ap)
		}
	}
	s.approvals = kept
}

// quota returns the per-session bytes of a served listing (from its tags).
func (s *Server) quota(ctx context.Context, listing common.Hash) (uint64, error) {
	if !s.Serves(listing) {
		return 0, errors.New("not a storage listing served here")
	}
	s.mu.Lock()
	q, ok := s.quotas[listing]
	s.mu.Unlock()
	if ok {
		return q, nil
	}
	m, err := s.k.GetModel(ctx, listing)
	if err != nil {
		return 0, err
	}
	q = marketplace.StorageQuota(m.Tags)
	if q == 0 {
		return 0, fmt.Errorf("listing %s has no storage quota tag", listing.Hex())
	}
	s.mu.Lock()
	s.quotas[listing] = q
	s.mu.Unlock()
	return q, nil
}

// Handle serves the storage.* methods. Requests must name this provider and
// are served once: a replayed signature is refused.
func (s *Server) Handle(ctx context.Context, msg *morrpc.RPCMessage, remote string, send func(*morrpc.RPCResponse) error) error {
	req, user, _, err := morrpc.VerifyStorageRequest(msg)
	if err != nil {
		return err
	}
	if !s.servesFor(ctx, common.HexToAddress(req.Provider)) {
		return errors.New("storage request is for another provider")
	}
	hash := morrpc.RequestHash(req.Signature)
	s.mu.Lock()
	_, replayed := s.seen[hash]
	if !replayed {
		s.seen[hash] = time.Now().Add(morrpc.StorageWindow + 3*time.Minute).Unix()
	}
	s.mu.Unlock()
	if replayed {
		return errors.New("storage request was already served")
	}
	var result any
	switch msg.Method {
	case morrpc.MethodStorageInfo:
		result, err = s.info(ctx, req)
	case morrpc.MethodStorageAttach:
		result, err = s.attach(ctx, user, req)
	case morrpc.MethodStoragePutStart:
		result, err = s.putStart(ctx, user, req)
	case morrpc.MethodStoragePutChunk:
		result, err = s.putChunk(user, req)
	case morrpc.MethodStoragePutEnd:
		result, err = s.putEnd(ctx, user, req)
	case morrpc.MethodStorageList:
		result, err = s.list(user)
	case morrpc.MethodStorageGet:
		result, err = s.get(ctx, user, req)
	case morrpc.MethodStorageProve:
		result, err = s.prove(ctx, user, req)
	case morrpc.MethodStorageDelete:
		result, err = s.delete(ctx, user, req)
	default:
		return fmt.Errorf("unsupported method %q", msg.Method)
	}
	if err != nil {
		return err
	}
	res, err := morrpc.StorageResponse(msg.ID, result, hash, s.w)
	if err != nil {
		return err
	}
	return send(res)
}

func (s *Server) info(ctx context.Context, req *morrpc.StorageReq) (any, error) {
	info := morrpc.StorageInfo{
		Version: s.opt.Version, MaxFileBytes: s.opt.MaxFileBytes, GraceSeconds: uint64(s.opt.Grace / time.Second),
		Payment: "direct", Gateway: s.opt.Gateway, ChunkBytes: ChunkBytes, Nonce: req.Nonce,
	}
	if s.opt.AcceptStipend {
		info.Payment = "any"
	}
	for _, l := range s.opt.Listings {
		if q, err := s.quota(ctx, l); err == nil {
			info.Listings = append(info.Listings, morrpc.StorageListing{ID: l.Hex(), QuotaBytes: q})
		}
	}
	return info, nil
}

// attach registers a marketplace session as a storage lease after checking
// it on-chain: the signer's, ours, on a storage listing, live, and paid the
// way this provider accepts.
func (s *Server) attach(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	if err := s.attachLease(ctx, user, req.Session); err != nil {
		return nil, err
	}
	return s.standing(user), nil
}

func (s *Server) attachLease(ctx context.Context, user common.Address, session string) error {
	if !strings.HasPrefix(session, "0x") || len(session) != 66 {
		return errors.New("invalid session id")
	}
	id := common.HexToHash(session)
	s.mu.Lock()
	known := s.accounts[user] != nil && s.accounts[user].Leases[id] != nil
	allowed := known || s.lookups.allow(time.Now())
	s.mu.Unlock()
	if known {
		return nil
	}
	if !allowed {
		return errors.New("provider is busy checking sessions; retry shortly")
	}
	cs, err := s.k.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("session cannot be loaded: %w", err)
	}
	if cs.User != user {
		return errors.New("session belongs to another user")
	}
	if cs.Closed() || time.Now().After(cs.EndsAtTime()) {
		return errors.New("session is not active")
	}
	if !cs.IsDirectPaymentFromUser && !s.opt.AcceptStipend {
		return errors.New("storage sessions must be opened with direct payment")
	}
	bid, err := s.k.GetBid(ctx, cs.BidId)
	if err != nil {
		return fmt.Errorf("session bid cannot be loaded: %w", err)
	}
	if !s.servesFor(ctx, bid.Provider) {
		return errors.New("session not bound to this provider")
	}
	listing := common.Hash(bid.ModelId)
	quota, err := s.quota(ctx, listing)
	if err != nil {
		return err
	}
	s.mu.Lock()
	a := s.accounts[user]
	if a == nil {
		a = &account{Files: map[string]*fileRec{}, Leases: map[common.Hash]*leaseRec{}}
		s.accounts[user] = a
	}
	a.Leases[id] = &leaseRec{Listing: listing, Quota: quota, Opened: cs.OpenedAt.Int64(), EndsAt: cs.EndsAtTime().Unix(), checked: time.Now()}
	for i, ap := range s.approvals {
		if ap.user == user && ap.listing == listing {
			s.approvals = append(s.approvals[:i], s.approvals[i+1:]...)
			break
		}
	}
	s.dirty = true
	s.mu.Unlock()
	s.log.Info("storage lease attached", "user", user.Hex(), "session", id.Hex(), "quota", quota)
	return nil
}

// standing reports a user's quota, usage and cover (zero for strangers).
func (s *Server) standing(user common.Address) morrpc.StorageAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[user]
	if a == nil {
		return morrpc.StorageAccount{}
	}
	quota, until := a.coverage(time.Now().Unix())
	return morrpc.StorageAccount{QuotaBytes: quota, UsedBytes: a.used() + s.reservedLocked(user), CoveredUntil: until}
}

func (s *Server) reservedLocked(user common.Address) uint64 {
	var n uint64
	for _, p := range s.uploads {
		if p.user == user {
			n += p.size
		}
	}
	return n
}

func (s *Server) putStart(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	if req.Session != "" {
		if err := s.attachLease(ctx, user, req.Session); err != nil {
			return nil, err
		}
	}
	c, err := ipfs.Parse(req.CID)
	if err != nil {
		return nil, fmt.Errorf("invalid cid: %w", err)
	}
	switch {
	case req.Size == 0:
		return nil, errors.New("empty files cannot be stored")
	case s.opt.MaxFileBytes > 0 && req.Size > s.opt.MaxFileBytes:
		return nil, fmt.Errorf("file is larger than this provider's limit (%s)", config.FormatBytes(s.opt.MaxFileBytes))
	case len(req.Label) > 512:
		return nil, errors.New("label is too long")
	}
	f, err := os.CreateTemp(s.opt.TempDir, uploadPrefix+"*")
	if err != nil {
		return nil, err
	}
	// The quota check and the reservation happen under one lock, so
	// concurrent uploads cannot both claim the same room.
	s.mu.Lock()
	a := s.accounts[user]
	var quota, used uint64
	if a != nil {
		if rec := a.Files[c.String()]; rec != nil {
			s.mu.Unlock()
			drop(f)
			return morrpc.StorageUpload{Received: rec.Size, ChunkBytes: ChunkBytes}, nil // already stored
		}
		quota, _ = a.coverage(time.Now().Unix())
		used = a.used() + s.reservedLocked(user)
	}
	if req.Size > quota || used > quota-req.Size {
		s.mu.Unlock()
		drop(f)
		if quota == 0 {
			return nil, errors.New("no storage lease covers this wallet here; attach a storage session first")
		}
		return nil, fmt.Errorf("over quota: %s of %s used; attach another storage session", config.FormatBytes(used), config.FormatBytes(quota))
	}
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	id := hex.EncodeToString(idb)
	s.uploads[id] = &pending{user: user, cid: c.String(), size: req.Size, label: req.Label, f: f, last: time.Now()}
	s.mu.Unlock()
	return morrpc.StorageUpload{Upload: id, ChunkBytes: ChunkBytes}, nil
}

// upload finds a user's upload and marks it active.
func (s *Server) upload(user common.Address, id string) (*pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[id]
	if up == nil || up.user != user {
		return nil, errors.New("unknown upload (uploads expire after 15 idle minutes)")
	}
	up.last = time.Now()
	return up, nil
}

// dropUpload forgets an upload and removes its spool file once no chunk or
// end is using it.
func (s *Server) dropUpload(id string) {
	s.mu.Lock()
	up := s.uploads[id]
	delete(s.uploads, id)
	s.mu.Unlock()
	if up != nil {
		up.mu.Lock()
		drop(up.f)
		up.mu.Unlock()
	}
}

func (s *Server) putChunk(user common.Address, req *morrpc.StorageReq) (any, error) {
	up, err := s.upload(user, req.Upload)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		return nil, errors.New("chunk data is not base64")
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	switch {
	case req.Offset != up.received:
		return nil, fmt.Errorf("expected offset %d, got %d", up.received, req.Offset)
	case len(data) == 0 || len(data) > maxChunkBytes:
		return nil, errors.New("invalid chunk size")
	case uint64(len(data)) > up.size-up.received:
		return nil, errors.New("chunk exceeds the declared size")
	}
	if _, err := up.f.Write(data); err != nil {
		return nil, err
	}
	up.received += uint64(len(data))
	return morrpc.StorageUpload{Upload: req.Upload, Received: up.received, ChunkBytes: ChunkBytes}, nil
}

// putEnd re-imports the upload, requires the CID the user computed, and
// imports the DAG into Kubo as a CAR (pinned), so the stored CID never
// depends on the Kubo node's own import settings.
func (s *Server) putEnd(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	up, err := s.upload(user, req.Upload)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if up.ending {
		s.mu.Unlock()
		return nil, errors.New("this upload is already being stored")
	}
	up.ending = true
	s.mu.Unlock()
	defer s.dropUpload(req.Upload)
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.received != up.size {
		return nil, fmt.Errorf("incomplete upload: %d of %d bytes", up.received, up.size)
	}
	want, _ := ipfs.Parse(up.cid)
	if _, err := up.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// Stream the DAG as a CAR into Kubo; a mismatch aborts the import
	// before the root (written last) arrives, so nothing wrong is pinned.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := ipfs.WriteCAR(pw, bufio.NewReaderSize(up.f, 1<<20), want, ipfs.Options{})
		pw.CloseWithError(err)
		done <- err
	}()
	_, ierr := s.kubo.DagImport(ctx, pr)
	pr.CloseWithError(errImportEnded)
	werr := <-done
	switch {
	case errors.Is(werr, ipfs.ErrRootMismatch):
		return nil, fmt.Errorf("uploaded content does not match cid %s", want)
	case ierr != nil:
		return nil, fmt.Errorf("pin on IPFS: %w", ierr)
	case werr != nil && !errors.Is(werr, errImportEnded):
		return nil, werr
	}
	now := time.Now().Unix()
	s.mu.Lock()
	a := s.accounts[user]
	if a == nil { // the account expired during the upload
		a = &account{Files: map[string]*fileRec{}, Leases: map[common.Hash]*leaseRec{}}
		s.accounts[user] = a
	}
	if a.Files[up.cid] == nil {
		a.Files[up.cid] = &fileRec{Size: up.size, Label: up.label, StoredAt: now}
		s.refs[up.cid]++
	}
	s.dirty = true
	s.mu.Unlock()
	s.log.Info("stored file", "user", user.Hex(), "cid", up.cid, "bytes", up.size)
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("stored %s for %s", shortCID(up.cid), short(user.Hex())))
	return morrpc.StorageReceipt{CID: up.cid, Size: up.size, Owner: user.Hex(), StoredAt: now}, nil
}

var errImportEnded = errors.New("IPFS import ended")

func (s *Server) owned(user common.Address, cid string) (*fileRec, error) {
	c, err := ipfs.Parse(cid)
	if err != nil {
		return nil, fmt.Errorf("invalid cid: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.accounts[user]; a != nil {
		if f := a.Files[c.String()]; f != nil {
			return f, nil
		}
	}
	return nil, errors.New("no such file for this user")
}

func (s *Server) list(user common.Address) (any, error) {
	out := morrpc.StorageList{StorageAccount: s.standing(user), Files: []morrpc.StorageFile{}}
	s.mu.Lock()
	if a := s.accounts[user]; a != nil {
		for cid, f := range a.Files {
			out.Files = append(out.Files, morrpc.StorageFile{CID: cid, Size: f.Size, Label: f.Label, StoredAt: f.StoredAt})
		}
	}
	s.mu.Unlock()
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].StoredAt < out.Files[j].StoredAt })
	return out, nil
}

func (s *Server) get(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	f, err := s.owned(user, req.CID)
	if err != nil {
		return nil, err
	}
	length := req.Length
	if length == 0 || length > maxReadBytes {
		length = maxReadBytes
	}
	if req.Offset >= f.Size {
		return nil, errors.New("offset is past the end of the file")
	}
	length = min(length, f.Size-req.Offset)
	rc, err := s.kubo.Cat(ctx, req.CID, int64(req.Offset), int64(length))
	if err != nil {
		return nil, fmt.Errorf("read from IPFS: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, int64(length)))
	if err == nil && uint64(len(data)) != length {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, fmt.Errorf("read from IPFS: %w", err)
	}
	return morrpc.StorageData{CID: req.CID, Offset: req.Offset, Data: base64.StdEncoding.EncodeToString(data)}, nil
}

func (s *Server) prove(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	f, err := s.owned(user, req.CID)
	if err != nil {
		return nil, err
	}
	if req.Offset >= f.Size {
		return nil, errors.New("offset is past the end of the file")
	}
	root, _ := ipfs.Parse(req.CID)
	blocks, err := ipfs.ProvePath(ctx, func(ctx context.Context, c ipfs.CID) ([]byte, error) {
		return s.kubo.BlockGet(ctx, c.String())
	}, root, req.Offset)
	if err != nil {
		return nil, fmt.Errorf("build proof: %w", err)
	}
	out := morrpc.StorageProof{CID: req.CID, Offset: req.Offset, Nonce: req.Nonce}
	for _, b := range blocks {
		out.Blocks = append(out.Blocks, base64.StdEncoding.EncodeToString(b))
	}
	return out, nil
}

func (s *Server) delete(ctx context.Context, user common.Address, req *morrpc.StorageReq) (any, error) {
	f, err := s.owned(user, req.CID)
	if err != nil {
		return nil, err
	}
	c, _ := ipfs.Parse(req.CID)
	s.mu.Lock()
	unpin := s.dropFilesLocked(user, []string{c.String()})
	s.mu.Unlock()
	s.pinRm(ctx, unpin)
	return morrpc.StorageDeleted{CID: req.CID, FreedBytes: f.Size}, nil
}

// dropFilesLocked removes files from a user's account and returns the CIDs
// no user holds any more, to unpin.
func (s *Server) dropFilesLocked(user common.Address, cids []string) (unpin []string) {
	a := s.accounts[user]
	if a == nil {
		return nil
	}
	for _, cid := range cids {
		if a.Files[cid] == nil {
			continue
		}
		delete(a.Files, cid)
		if s.refs[cid]--; s.refs[cid] <= 0 {
			delete(s.refs, cid)
			unpin = append(unpin, cid)
		}
		s.dirty = true
	}
	return unpin
}

func (s *Server) pinRm(ctx context.Context, cids []string) {
	for _, cid := range cids {
		if err := s.kubo.PinRm(ctx, cid); err != nil {
			s.log.Warn("could not unpin", "cid", cid, "err", err)
		}
	}
}

// Run expires uploads and leases, unpins files whose coverage and grace
// period ended, and persists state.
func (s *Server) Run(ctx context.Context) {
	if s.opt.MaxTotal == 0 {
		// Default the capacity sold to the repo's StorageMax.
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		if st, err := s.kubo.RepoStat(sctx); err == nil && st.StorageMax > 0 {
			s.mu.Lock()
			s.opt.MaxTotal = st.StorageMax
			s.mu.Unlock()
			s.log.Info("storage capacity from the IPFS repo", "storageMax", st.StorageMax)
		} else if err != nil {
			s.log.Warn("could not read the IPFS repo size", "err", err)
		}
		cancel()
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.housekeep(ctx)
		s.save()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			ids := make([]string, 0, len(s.uploads))
			for id := range s.uploads {
				ids = append(ids, id)
			}
			s.mu.Unlock()
			for _, id := range ids {
				s.dropUpload(id)
			}
			s.save()
			return
		case <-tick.C:
		}
	}
}

func (s *Server) housekeep(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var stale []string
	for id, up := range s.uploads {
		if !up.ending && now.Sub(up.last) > uploadIdle {
			stale = append(stale, id)
		}
	}
	for h, exp := range s.seen {
		if now.Unix() > exp {
			delete(s.seen, h)
		}
	}
	s.pruneApprovalsLocked(now)
	type check struct {
		user common.Address
		id   common.Hash
	}
	var checks []check
	for user, a := range s.accounts {
		for id, l := range a.Leases {
			if l.Closed == 0 && now.Unix() < l.EndsAt && now.Sub(l.checked) > 30*time.Minute {
				checks = append(checks, check{user, id})
			}
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.dropUpload(id)
	}
	// Sessions closed early end the lease early.
	for _, c := range checks {
		cs, err := s.k.GetSession(ctx, c.id)
		if err != nil {
			continue
		}
		s.mu.Lock()
		if a := s.accounts[c.user]; a != nil {
			if l := a.Leases[c.id]; l != nil {
				l.checked = now
				if cs.Closed() && cs.ClosedAt != nil {
					l.Closed = cs.ClosedAt.Int64()
				}
				s.dirty = true
			}
		}
		s.mu.Unlock()
	}
	// Coverage plus grace over: unpin and forget. Files over the live quota
	// for longer than overQuotaGrace go too, newest first, so a small
	// renewal cannot keep a large upload. Accounts are changed and removed
	// in one critical section, so uploads or attaches racing with expiry
	// are either kept or never counted.
	type note struct {
		user    common.Address
		files   int
		expired bool
	}
	var notes []note
	var unpin []string
	s.mu.Lock()
	for user, a := range s.accounts {
		quota, until := a.coverage(now.Unix())
		grace := a.grace(s.opt.Grace, now)
		lapsed := len(a.Leases) == 0 || !now.Before(time.Unix(until, 0).Add(grace))
		if lapsed {
			cids := make([]string, 0, len(a.Files))
			for cid := range a.Files {
				cids = append(cids, cid)
			}
			unpin = append(unpin, s.dropFilesLocked(user, cids)...)
			if !s.uploadingLocked(user) {
				delete(s.accounts, user)
			}
			s.dirty = true
			if len(cids) > 0 {
				notes = append(notes, note{user, len(cids), true})
			}
			continue
		}
		if a.pruneLeases(now.Add(-paidWindow).Unix()) {
			s.dirty = true
		}
		used := a.used()
		switch {
		case used <= quota:
			if a.OverSince != 0 {
				a.OverSince, s.dirty = 0, true
			}
		case a.OverSince == 0:
			a.OverSince, s.dirty = now.Unix(), true
		case now.After(time.Unix(a.OverSince, 0).Add(min(grace, overQuotaGrace))):
			cids := a.newestBeyond(quota)
			unpin = append(unpin, s.dropFilesLocked(user, cids)...)
			notes = append(notes, note{user, len(cids), false})
		}
	}
	s.mu.Unlock()
	s.pinRm(ctx, unpin)
	for _, n := range notes {
		if n.expired {
			s.log.Info("storage lease lapsed; files unpinned", "user", n.user.Hex(), "files", n.files)
			s.bus.Emit(events.Warn, "storage", fmt.Sprintf("lease of %s lapsed: %d file(s) unpinned", short(n.user.Hex()), n.files))
		} else {
			s.log.Info("storage over quota past the grace period; files unpinned", "user", n.user.Hex(), "files", n.files)
			s.bus.Emit(events.Warn, "storage", fmt.Sprintf("%s stayed over quota: %d file(s) unpinned", short(n.user.Hex()), n.files))
		}
	}
	_, err := s.kubo.Version(ctx)
	s.mu.Lock()
	s.healthy = err == nil
	s.mu.Unlock()
}

func (s *Server) uploadingLocked(user common.Address) bool {
	for _, up := range s.uploads {
		if up.user == user {
			return true
		}
	}
	return false
}

// lookupLimiter bounds the chain reads strangers can trigger by attaching
// unknown sessions (a token bucket; guarded by Server.mu).
type lookupLimiter struct {
	rate, burst float64
	tokens      float64
	at          time.Time
}

func (l *lookupLimiter) allow(now time.Time) bool {
	if l.at.IsZero() {
		l.tokens = l.burst
	} else {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.at).Seconds()*l.rate)
	}
	l.at = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// ServerStats summarize the provider's storage service for status displays.
type ServerStats struct {
	Users       int    `json:"users"`
	Files       int    `json:"files"`
	StoredBytes uint64 `json:"storedBytes"`
	Healthy     bool   `json:"healthy"`
}

func (s *Server) Stats() ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServerStats{Users: len(s.accounts), Healthy: s.healthy}
	for _, a := range s.accounts {
		st.Files += len(a.Files)
		st.StoredBytes += a.used()
	}
	return st
}

// HealthReports describe the storage listings in the provider's pong.
func (s *Server) HealthReports(bids map[common.Hash]common.Hash) []morrpc.ModelHealthReport {
	s.mu.Lock()
	healthy := s.healthy
	s.mu.Unlock()
	var out []morrpc.ModelHealthReport
	for _, l := range s.opt.Listings {
		r := morrpc.ModelHealthReport{ModelID: l.Hex(), ModelType: "STORAGE", Status: morrpc.HealthHealthy, LastChecked: time.Now().Unix()}
		if !healthy {
			r.Status, r.ErrorKind = morrpc.HealthUnhealthy, "connection"
		}
		if bid, ok := bids[l]; ok {
			r.HasActiveBid, r.BidID = true, bid.Hex()
		} else {
			r.Status = morrpc.HealthNoBid
		}
		out = append(out, r)
	}
	return out
}

type serverState struct {
	Version  int                         `json:"version"`
	Accounts map[common.Address]*account `json:"accounts"`
}

func (s *Server) load() {
	if s.opt.StatePath == "" {
		return
	}
	var st serverState
	if _, err := store.ReadJSON(s.opt.StatePath, &st); err != nil {
		s.log.Warn("could not read storage state", "err", err)
		return
	}
	for user, a := range st.Accounts {
		if a.Files == nil {
			a.Files = map[string]*fileRec{}
		}
		if a.Leases == nil {
			a.Leases = map[common.Hash]*leaseRec{}
		}
		s.accounts[user] = a
		for cid := range a.Files {
			s.refs[cid]++
		}
	}
}

// save writes the state when it changed; the file is written outside the
// lock.
func (s *Server) save() {
	if s.opt.StatePath == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	data, err := json.MarshalIndent(serverState{Version: 1, Accounts: s.accounts}, "", "  ")
	s.dirty = err != nil
	s.mu.Unlock()
	if err == nil {
		err = store.WriteFile(s.opt.StatePath, data, 0o600)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		s.log.Warn("could not save storage state", "err", err)
	}
}

func shortCID(c string) string {
	if len(c) > 16 {
		return c[:10] + "…" + c[len(c)-4:]
	}
	return c
}

func short(s string) string {
	if len(s) > 10 {
		return s[:6] + "…" + s[len(s)-4:]
	}
	return s
}
