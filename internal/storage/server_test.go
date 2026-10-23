package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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
	s := NewServer(ServerOptions{MaxFileBytes: 8 << 20, Grace: grace, StatePath: state, Version: "test"}, w, nil, kc,
		func(context.Context, common.Address) bool { return true }, events.New(), log)
	return s, node
}

func (s *Server) testLease(user common.Address, quota uint64, ends time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := common.BytesToHash([]byte(randomHex(16)))
	s.acct(user).Leases[id] = &leaseRec{Quota: quota, EndsAt: ends.Unix(), checked: time.Now()}
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

func TestServerKeepsFilesForTheGracePeriod(t *testing.T) {
	ctx := context.Background()
	s, node := testServer(t, 2*time.Hour, "")
	user := common.HexToAddress("0x1")
	s.testLease(user, 8<<20, time.Now().Add(time.Hour))
	data, c := content(300 << 10)
	if err := s.testPut(user, data, c); err != nil {
		t.Fatal(err)
	}
	// The lease ended an hour ago: still within the grace period.
	for _, l := range s.accounts[user].Leases {
		l.EndsAt = time.Now().Add(-time.Hour).Unix()
	}
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
	for _, l := range s.accounts[user].Leases {
		l.EndsAt = time.Now().Add(-3 * time.Hour).Unix()
	}
	s.housekeep(ctx)
	if node.Pinned(c) || s.accounts[user] != nil {
		t.Fatal("lapsed files were kept")
	}
}

func TestServerDropsNewestFilesOverQuota(t *testing.T) {
	ctx := context.Background()
	s, node := testServer(t, time.Hour, "")
	user := common.HexToAddress("0x1")
	s.testLease(user, 2<<20, time.Now().Add(time.Hour))
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
	for _, l := range s.accounts[user].Leases {
		l.EndsAt = time.Now().Add(-time.Minute).Unix()
	}
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
