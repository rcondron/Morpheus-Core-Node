package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"math"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/ipfstest"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// testServer is a storage server on a fake Kubo node, driven directly (no
// chain: leases are recorded as attach would record them).
func testServer(t *testing.T, grace time.Duration, state string) (*Server, *ipfstest.Node) {
	t.Helper()
	node := ipfstest.New()
	t.Cleanup(node.Close)
	kc, err := kubo.New(node.URL)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := wallet.Generate()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(ServerOptions{MaxFileBytes: 8 << 20, Grace: grace, StatePath: state, TempDir: t.TempDir(), Version: "test"}, w, nil, kc,
		func(context.Context, common.Address) bool { return true }, events.New(), log)
	return s, node
}

// testLease records a day-long lease ending at ends.
func (s *Server) testLease(user common.Address, quota uint64, ends time.Time) *leaseRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[user]
	if a == nil {
		a = &account{Files: map[string]*fileRec{}, Leases: map[common.Hash]*leaseRec{}}
		s.accounts[user] = a
	}
	l := &leaseRec{Quota: quota, Opened: ends.Add(-24 * time.Hour).Unix(), EndsAt: ends.Unix(), checked: time.Now()}
	a.Leases[common.BytesToHash([]byte(randomHex(16)))] = l
	return l
}

func (s *Server) testPut(user common.Address, data []byte, announce ipfs.CID) error {
	ctx := context.Background()
	res, err := s.putStart(ctx, user, &morrpc.StorageReq{CID: announce.String(), Size: uint64(len(data))})
	if err != nil {
		return err
	}
	up := res.(morrpc.StorageUpload)
	if up.Upload == "" {
		return nil // already stored for this user
	}
	for off := 0; off < len(data); off += ChunkBytes {
		end := min(off+ChunkBytes, len(data))
		req := &morrpc.StorageReq{Upload: up.Upload, Offset: uint64(off), Data: base64.StdEncoding.EncodeToString(data[off:end])}
		if _, err := s.putChunk(user, req); err != nil {
			return err
		}
	}
	_, err = s.putEnd(ctx, user, &morrpc.StorageReq{Upload: up.Upload})
	return err
}

func content(n int) ([]byte, ipfs.CID) {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	c, _, _ := ipfs.Import(bytes.NewReader(b), nil)
	return b, c
}

func TestServerRejectsContentNotMatchingTheCID(t *testing.T) {
	s, node := testServer(t, time.Hour, "")
	user := common.HexToAddress("0x1")
	s.testLease(user, 8<<20, time.Now().Add(time.Hour))
	_, announced := content(600 << 10)
	other, _ := content(600 << 10)
	err := s.testPut(user, other, announced)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched upload: %v", err)
	}
	if node.Pinned(announced) || s.standing(user).UsedBytes != 0 {
		t.Fatal("a mismatched upload was kept")
	}
	if len(s.uploads) != 0 {
		t.Fatal("the failed upload was not cleaned up")
	}
}

func TestServerQuotaAndSharedFiles(t *testing.T) {
	s, node := testServer(t, time.Hour, "")
	alice, bob := common.HexToAddress("0xa"), common.HexToAddress("0xb")
	s.testLease(alice, 1<<20, time.Now().Add(time.Hour))
	s.testLease(bob, 1<<20, time.Now().Add(time.Hour))
	a, ac := content(600 << 10)
	b, bc := content(600 << 10)
	if err := s.testPut(alice, a, ac); err != nil {
		t.Fatal(err)
	}
	if err := s.testPut(alice, b, bc); err == nil || !strings.Contains(err.Error(), "over quota") {
		t.Fatalf("upload past the quota: %v", err)
	}
	if _, err := s.putStart(context.Background(), common.HexToAddress("0xc"), &morrpc.StorageReq{CID: bc.String(), Size: 1}); err == nil {
		t.Fatal("a user without a lease started an upload")
	}
	// Bob stores the same bytes: one pin, two owners.
	if err := s.testPut(bob, a, ac); err != nil {
		t.Fatal(err)
	}
	if _, err := s.delete(context.Background(), alice, &morrpc.StorageReq{CID: ac.String()}); err != nil {
		t.Fatal(err)
	}
	if !node.Pinned(ac) {
		t.Fatal("deleting one owner's copy unpinned another's")
	}
	if _, err := s.get(context.Background(), alice, &morrpc.StorageReq{CID: ac.String()}); err == nil {
		t.Fatal("a deleted file is still readable by its former owner")
	}
	if _, err := s.delete(context.Background(), bob, &morrpc.StorageReq{CID: ac.String()}); err != nil || node.Pinned(ac) {
		t.Fatalf("last owner's delete: %v pinned=%v", err, node.Pinned(ac))
	}
}

func TestServerQuotaHoldsUnderConcurrencyAndOverflow(t *testing.T) {
	s, _ := testServer(t, time.Hour, "")
	s.opt.MaxFileBytes = 0 // no per-file limit: only the quota bounds uploads
	user := common.HexToAddress("0x1")
	s.testLease(user, 1<<20, time.Now().Add(time.Hour))
	_, c := content(10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.putStart(context.Background(), user, &morrpc.StorageReq{CID: c.String(), Size: 1 << 20}); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("%d concurrent uploads of the whole quota were accepted", accepted)
	}
	// A declared size that would wrap the quota arithmetic is refused.
	if _, err := s.putStart(context.Background(), user, &morrpc.StorageReq{CID: c.String(), Size: math.MaxUint64 - (1 << 20) + 1}); err == nil {
		t.Fatal("an overflowing size was accepted")
	}
}

func TestServerKeepsFilesForTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	s, node := testServer(t, 2*time.Hour, "")
	user := common.HexToAddress("0x1")
	l := s.testLease(user, 8<<20, time.Now().Add(time.Hour))
	data, c := content(300 << 10)
	if err := s.testPut(user, data, c); err != nil {
		t.Fatal(err)
	}
	// The day-long lease ended an hour ago: still within the grace period.
	l.EndsAt = time.Now().Add(-time.Hour).Unix()
	s.housekeep(ctx)
	if !node.Pinned(c) {
		t.Fatal("file dropped during the grace period")
	}
	got, err := s.get(ctx, user, &morrpc.StorageReq{CID: c.String()})
	if err != nil {
		t.Fatalf("read during the grace period: %v", err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(got.(morrpc.StorageData).Data); !bytes.Equal(raw, data) {
		t.Fatal("read returned other bytes")
	}
	// Three hours after the end: unpinned and forgotten.
	l.EndsAt = time.Now().Add(-3 * time.Hour).Unix()
	s.housekeep(ctx)
	if node.Pinned(c) || s.accounts[user] != nil {
		t.Fatal("lapsed files were kept")
	}
}

func TestServerGraceIsBoundedByPaidTime(t *testing.T) {
	ctx := context.Background()
	s, node := testServer(t, 72*time.Hour, "")
	user := common.HexToAddress("0x1")
	l := s.testLease(user, 8<<20, time.Now().Add(time.Hour))
	data, c := content(100 << 10)
	if err := s.testPut(user, data, c); err != nil {
		t.Fatal(err)
	}
	// A five-minute lease (or one closed after five minutes) that ended ten
	// minutes ago earned five minutes of grace, not 72 hours.
	l.Opened, l.EndsAt = time.Now().Add(-15*time.Minute).Unix(), time.Now().Add(-10*time.Minute).Unix()
	s.housekeep(ctx)
	if node.Pinned(c) {
		t.Fatal("a short lease bought the full grace period")
	}
}

func TestServerDropsNewestFilesOverQuota(t *testing.T) {
	ctx := context.Background()
	s, node := testServer(t, time.Hour, "")
	user := common.HexToAddress("0x1")
	large := s.testLease(user, 2<<20, time.Now().Add(time.Hour))
	old, oc := content(700 << 10)
	newer, nc := content(700 << 10)
	if err := s.testPut(user, old, oc); err != nil {
		t.Fatal(err)
	}
	if err := s.testPut(user, newer, nc); err != nil {
		t.Fatal(err)
	}
	s.accounts[user].Files[oc.String()].StoredAt -= 60
	// The big lease ends; a small one (1 MiB for 1.4 MiB stored) follows.
	large.EndsAt = time.Now().Add(-time.Minute).Unix()
	s.testLease(user, 1<<20, time.Now().Add(time.Hour))
	s.housekeep(ctx)
	if s.accounts[user].OverSince == 0 || !node.Pinned(nc) {
		t.Fatal("over quota was not noticed, or files went before the grace period")
	}
	s.accounts[user].OverSince = time.Now().Add(-2 * time.Hour).Unix()
	s.housekeep(ctx)
	if node.Pinned(nc) || !node.Pinned(oc) {
		t.Fatalf("after the grace period: newest pinned=%v oldest pinned=%v", node.Pinned(nc), node.Pinned(oc))
	}
	s.housekeep(ctx)
	if s.accounts[user].OverSince != 0 {
		t.Fatal("still marked over quota after dropping files")
	}
}

func TestServerStatePersists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "storage_provider.json")
	s, _ := testServer(t, time.Hour, state)
	user := common.HexToAddress("0x1")
	s.testLease(user, 8<<20, time.Now().Add(time.Hour))
	data, c := content(100 << 10)
	if err := s.testPut(user, data, c); err != nil {
		t.Fatal(err)
	}
	s.save()
	again, _ := testServer(t, time.Hour, state)
	st := again.standing(user)
	if st.UsedBytes != uint64(len(data)) || st.QuotaBytes != 8<<20 || again.refs[c.String()] != 1 {
		t.Fatalf("reloaded state: %+v refs=%d", st, again.refs[c.String()])
	}
}

// TestServerSignedRequests checks requests are bound to their method and
// provider and served once.
func TestServerSignedRequests(t *testing.T) {
	s, _ := testServer(t, time.Hour, "")
	me := common.HexToAddress("0xbeef")
	s.servesFor = func(_ context.Context, p common.Address) bool { return p == me }
	user, _ := wallet.Generate()
	s.testLease(user.Address(), 8<<20, time.Now().Add(time.Hour))
	data, c := content(100 << 10)
	if err := s.testPut(user.Address(), data, c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handle := func(msg *morrpc.RPCMessage) (*morrpc.RPCResponse, error) {
		var res *morrpc.RPCResponse
		err := s.Handle(ctx, msg, "test", func(r *morrpc.RPCResponse) error { res = r; return nil })
		return res, err
	}

	prove, hash, _ := morrpc.StorageRequest("1", morrpc.MethodStorageProve, me, morrpc.StorageReq{CID: c.String(), Nonce: "n"}, user)
	res, err := handle(prove)
	if err != nil {
		t.Fatal(err)
	}
	var proof morrpc.StorageProof
	if _, err := morrpc.VerifyStorageResponse(res, hash, func(common.Address) error { return nil }, &proof); err != nil || proof.Nonce != "n" {
		t.Fatalf("proof response: %v", err)
	}
	if _, err := morrpc.VerifyStorageResponse(res, "0x00", func(common.Address) error { return nil }, &proof); err == nil {
		t.Fatal("a response was accepted for another request")
	}
	// The same signed request again is refused.
	if _, err := handle(prove); err == nil || !strings.Contains(err.Error(), "already served") {
		t.Fatalf("replay: %v", err)
	}
	// Signed params resent as another method (a prove turned into a delete).
	other, _, _ := morrpc.StorageRequest("2", morrpc.MethodStorageProve, me, morrpc.StorageReq{CID: c.String()}, user)
	other.Method = morrpc.MethodStorageDelete
	if _, err := handle(other); err == nil || !strings.Contains(err.Error(), "another method") {
		t.Fatalf("method swap: %v", err)
	}
	// A request meant for another provider.
	elsewhere, _, _ := morrpc.StorageRequest("3", morrpc.MethodStorageDelete, common.HexToAddress("0xdead"), morrpc.StorageReq{CID: c.String()}, user)
	if _, err := handle(elsewhere); err == nil || !strings.Contains(err.Error(), "another provider") {
		t.Fatalf("other provider: %v", err)
	}
	if _, err := s.owned(user.Address(), c.String()); err != nil {
		t.Fatal("the file was deleted by a forged request")
	}
}

func TestServerReadsCreateNoAccounts(t *testing.T) {
	s, _ := testServer(t, time.Hour, "")
	for i := range 50 {
		user := common.BigToAddress(big.NewInt(int64(i + 100)))
		if _, err := s.list(user); err != nil {
			t.Fatal(err)
		}
		_ = s.standing(user)
		_, _ = s.putStart(context.Background(), user, &morrpc.StorageReq{CID: "bafkreih6ujchdcg4jinhbx2k3fqfkb7yjwlfw5tpmsfiyl3oaizzj2dvu4", Size: 1})
	}
	if len(s.accounts) != 0 {
		t.Fatalf("%d accounts created by reads", len(s.accounts))
	}
}

func TestServerApprovalsCountAgainstCapacity(t *testing.T) {
	s, _ := testServer(t, time.Hour, "")
	listing := common.HexToHash("0x51")
	s.opt.Listings, s.opt.MaxTotal = []common.Hash{listing}, 2<<20
	s.quotas[listing] = 1 << 20
	ctx := context.Background()
	for i := range 2 {
		if err := s.Admit(ctx, listing, common.HexToAddress("0x1")); err != nil {
			t.Fatalf("approval %d: %v", i, err)
		}
	}
	if err := s.Admit(ctx, listing, common.HexToAddress("0x2")); err == nil || !strings.Contains(err.Error(), "fully committed") {
		t.Fatalf("approval past capacity: %v", err)
	}
}
