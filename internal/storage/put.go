package storage

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

// PutOptions describe a new file.
type PutOptions struct {
	Name        string
	ContentType string
	Purpose     string
	Owner       string // API key id
	// Public stores the bytes as they are (anyone with the CID can read
	// them) instead of encrypted to the wallet.
	Public bool
}

// Put stores r's content with up to Replicas providers and returns the
// file's record once at least one holds it; the keeper adds missing copies.
func (s *Store) Put(ctx context.Context, r io.Reader, opt PutOptions) (*File, error) {
	plain, err := s.tempFile()
	if err != nil {
		return nil, err
	}
	defer drop(plain)
	size, err := io.Copy(plain, r)
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if size == 0 {
		return nil, errors.New("empty files cannot be stored")
	}
	encrypt := s.opt.Encrypt && !opt.Public
	data := plain
	if encrypt {
		sealed, err := s.tempFile()
		if err != nil {
			return nil, err
		}
		defer drop(sealed)
		if _, err := plain.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		bw := bufio.NewWriterSize(sealed, 1<<20)
		meta := Meta{Name: opt.Name, ContentType: opt.ContentType, Size: size, Created: time.Now().Unix()}
		if err := Seal(bw, bufio.NewReaderSize(plain, 1<<20), meta, s.w.PublicKey()); err != nil {
			return nil, fmt.Errorf("encrypt: %w", err)
		}
		if err := bw.Flush(); err != nil {
			return nil, err
		}
		data = sealed
	}
	stored, err := data.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := data.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	root, _, err := ipfs.Import(bufio.NewReaderSize(data, 1<<20), nil)
	if err != nil {
		return nil, err
	}
	f := &File{
		ID: newFileID(), Name: opt.Name, ContentType: opt.ContentType, Purpose: opt.Purpose, Owner: opt.Owner,
		Size: size, StoredBytes: uint64(stored), CID: root.String(), Encrypted: encrypt, Created: time.Now().UTC(),
	}
	copies, releases, errs := s.addCopies(ctx, f, data, s.opt.Replicas, map[common.Address]bool{})
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	if len(copies) == 0 {
		return nil, fmt.Errorf("the file was not stored: %s", strings.Join(errs, "; "))
	}
	f.Copies = copies
	s.mu.Lock()
	s.files[f.ID] = f
	s.dirty = true
	out := f.clone()
	s.mu.Unlock()
	s.save()
	if len(copies) < s.opt.Replicas {
		s.log.Warn("stored fewer copies than configured", "file", f.ID, "copies", len(copies), "want", s.opt.Replicas, "errors", strings.Join(errs, "; "))
	}
	s.log.Info("file stored", "file", f.ID, "cid", f.CID, "bytes", size, "copies", len(copies), "encrypted", encrypt)
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("stored %s (%s) with %d provider(s)", displayName(f), config.FormatBytes(uint64(size)), len(copies)))
	return out, nil
}

// label is what a provider may see of a file's name: nothing when encrypted.
func (f *File) label() string {
	if f.Encrypted {
		return ""
	}
	return f.Name
}

func displayName(f *File) string {
	if f.Name != "" {
		return f.Name
	}
	return f.ID
}

// addCopies uploads data to n providers not in exclude (which it extends).
// The returned release functions free the room reserved for the new copies;
// call them once the copies are recorded in the manifest.
func (s *Store) addCopies(ctx context.Context, f *File, data *os.File, n int, exclude map[common.Address]bool) (copies []*Copy, releases []func(), errs []string) {
	for len(copies) < n {
		l, release, err := s.place(ctx, f.StoredBytes, exclude)
		if err != nil {
			errs = append(errs, err.Error())
			break
		}
		exclude[l.Provider] = true
		rc, err := s.upload(ctx, l, data, f)
		if err != nil {
			release()
			errs = append(errs, fmt.Sprintf("%s: %v", short(l.Provider.Hex()), err))
			s.log.Warn("upload failed", "file", f.ID, "provider", l.Provider.Hex(), "err", err)
			continue
		}
		releases = append(releases, release)
		copies = append(copies, &Copy{Provider: l.Provider, Endpoint: l.Endpoint, StoredAt: rc.StoredAt})
	}
	return copies, releases, errs
}

// place picks the provider for size more bytes, leasing room when needed,
// and reserves the bytes there until release is called. Providers already
// leased from come first (their room is paid for), then the cheapest offer.
func (s *Store) place(ctx context.Context, size uint64, exclude map[common.Address]bool) (*Lease, func(), error) {
	s.placeMu.Lock()
	defer s.placeMu.Unlock()
	horizon := time.Now().Add(s.opt.RenewBefore)
	reserve := func(l *Lease) (*Lease, func(), error) {
		s.mu.Lock()
		s.pending[l.Provider] += size
		s.mu.Unlock()
		var once sync.Once
		return l, func() {
			once.Do(func() {
				s.mu.Lock()
				if s.pending[l.Provider] -= size; s.pending[l.Provider] == 0 {
					delete(s.pending, l.Provider)
				}
				s.mu.Unlock()
			})
		}, nil
	}

	s.mu.Lock()
	used := s.usedLocked()
	var roomy []*Lease
	seen := map[common.Address]bool{}
	for _, l := range s.leases {
		if seen[l.Provider] || exclude[l.Provider] || !s.allowed(l.Provider) {
			continue
		}
		seen[l.Provider] = true
		if quota, latest := s.quotaLocked(l.Provider, horizon); latest != nil && quota >= used[l.Provider]+size {
			roomy = append(roomy, latest)
		}
	}
	s.mu.Unlock()
	if len(roomy) > 0 {
		sort.Slice(roomy, func(i, j int) bool {
			return cheaper(Offer{Price: roomy[i].Price, Quota: roomy[i].Quota}, Offer{Price: roomy[j].Price, Quota: roomy[j].Quota})
		})
		return reserve(roomy[0])
	}

	offers, err := s.Offers(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read storage offers: %w", err)
	}
	var reasons []string
	tried := 0
	for _, o := range offers {
		if exclude[o.Provider] {
			continue
		}
		s.mu.Lock()
		quota, _ := s.quotaLocked(o.Provider, horizon)
		s.mu.Unlock()
		if quota+o.Quota < used[o.Provider]+size {
			reasons = append(reasons, fmt.Sprintf("%s sells %s per lease", short(o.Provider.Hex()), config.FormatBytes(o.Quota)))
			continue
		}
		if tried++; tried > 4 {
			break
		}
		l, err := s.lease(ctx, o)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", short(o.Provider.Hex()), err))
			continue
		}
		return reserve(l)
	}
	if len(reasons) == 0 {
		return nil, nil, errors.New("no provider sells storage on the marketplace right now (see morcore storage providers)")
	}
	return nil, nil, fmt.Errorf("no storage provider could take %s: %s", config.FormatBytes(size), strings.Join(reasons, "; "))
}

// lease buys a storage session on an offer and registers it with the
// provider.
func (s *Store) lease(ctx context.Context, o Offer) (*Lease, error) {
	if s.opt.MaxStake != nil && s.opt.MaxStake.Sign() > 0 {
		need, err := s.market.EstimateStake(ctx, o.Price, s.opt.Lease)
		if err != nil {
			return nil, err
		}
		if staked := s.Staked(); staked.Add(staked, need).Cmp(s.opt.MaxStake) > 0 {
			return nil, fmt.Errorf("storage stake cap reached (storage.max_stake %s; this lease needs %s more)", units.MOR(s.opt.MaxStake), units.MOR(need))
		}
	}
	cs, err := s.leaser.OpenLease(ctx, &o.Listing, o.Bid, s.opt.Lease)
	if err != nil {
		return nil, err
	}
	l := &Lease{
		Session: cs.ID, Listing: o.Listing.ID, ListingName: o.Listing.Name, Bid: o.Bid, Provider: o.Provider,
		Endpoint: cs.Endpoint, ProviderKey: cs.ProviderKey, Quota: o.Quota, Price: o.Price, Stake: cs.Stake,
		OpenedAt: cs.OpenedAt, EndsAt: cs.EndsAt,
	}
	s.mu.Lock()
	s.leases[l.Session] = l
	s.dirty = true
	s.mu.Unlock()
	s.save()
	if err := s.attach(ctx, l); err != nil {
		s.log.Warn("provider has not registered the lease yet", "session", l.Session.Hex(), "err", err)
	}
	s.log.Info("storage lease opened", "provider", l.Provider.Hex(), "session", l.Session.Hex(), "quota", l.Quota, "ends", l.EndsAt)
	s.bus.Emit(events.Info, "storage", fmt.Sprintf("leased %s from %s until %s", config.FormatBytes(l.Quota), short(l.Provider.Hex()), l.EndsAt.Local().Format("Jan 2 15:04")))
	return l, nil
}

// attach tells the provider about a lease (uploads also attach theirs).
func (s *Store) attach(ctx context.Context, l *Lease) error {
	var acct morrpc.StorageAccount
	return s.call(ctx, l.peer(), morrpc.MethodStorageAttach, morrpc.StorageReq{Session: l.Session.Hex()}, &acct)
}

// upload sends a file's stored bytes to a provider under a lease and checks
// the provider's signed receipt.
func (s *Store) upload(ctx context.Context, l *Lease, data *os.File, f *File) (*morrpc.StorageReceipt, error) {
	p := l.peer()
	var up morrpc.StorageUpload
	err := s.call(ctx, p, morrpc.MethodStoragePutStart, morrpc.StorageReq{Session: l.Session.Hex(), CID: f.CID, Size: f.StoredBytes, Label: f.label()}, &up)
	if err != nil {
		return nil, err
	}
	if up.Upload == "" {
		if up.Received != f.StoredBytes {
			return nil, errors.New("provider did not open an upload")
		}
		// The provider already holds this content for us.
		return &morrpc.StorageReceipt{CID: f.CID, Size: f.StoredBytes, Owner: s.w.Address().Hex(), StoredAt: time.Now().Unix()}, nil
	}
	chunk := up.ChunkBytes
	if chunk == 0 || chunk > maxChunkBytes {
		chunk = ChunkBytes
	}
	buf := make([]byte, chunk)
	for off := uint64(0); off < f.StoredBytes; {
		n, err := data.ReadAt(buf[:min(chunk, f.StoredBytes-off)], int64(off))
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		var ack morrpc.StorageUpload
		req := morrpc.StorageReq{Upload: up.Upload, Offset: off, Data: base64.StdEncoding.EncodeToString(buf[:n])}
		if err := s.call(ctx, p, morrpc.MethodStoragePutChunk, req, &ack); err != nil {
			return nil, err
		}
		off += uint64(n)
		if ack.Received != off {
			return nil, fmt.Errorf("provider acknowledged %d bytes, %d were sent", ack.Received, off)
		}
	}
	// The provider rebuilds the DAG and imports it into IPFS: allow time.
	wait := 5*time.Minute + time.Duration(f.StoredBytes>>30)*time.Minute
	var rc morrpc.StorageReceipt
	if err := s.callWait(ctx, p, morrpc.MethodStoragePutEnd, morrpc.StorageReq{Upload: up.Upload}, wait, &rc); err != nil {
		return nil, err
	}
	if rc.CID != f.CID || rc.Size != f.StoredBytes || !strings.EqualFold(rc.Owner, s.w.Address().Hex()) {
		return nil, errors.New("provider's receipt does not match the upload")
	}
	return &rc, nil
}
