package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain/chaintest"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/storage"
)

func storageHarness(t *testing.T) *harness {
	return newHarness(t, 2, 0, offering{name: "ipfs-256mib", quota: 256 << 20})
}

type fileJSON struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Bytes     int64    `json:"bytes"`
	Filename  string   `json:"filename"`
	Purpose   string   `json:"purpose"`
	CID       string   `json:"cid"`
	Encrypted bool     `json:"encrypted"`
	Copies    int      `json:"copies"`
	Providers []string `json:"providers"`
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// TestStorageFilesAPI stores a file through the OpenAI-style Files API: the
// node buys a lease (a marketplace session paid directly to the provider),
// encrypts the file to the wallet, uploads it over MOR-RPC, and the provider
// pins the CID the node computed on its Kubo node.
func TestStorageFilesAPI(t *testing.T) {
	h := storageHarness(t)
	ctx := context.Background()
	agent := h.newKey(apikeys.CreateOptions{Name: "files", Scopes: []string{apikeys.ScopeInference, apikeys.ScopeStorage}})
	other := h.newKey(apikeys.CreateOptions{Name: "other", Scopes: []string{apikeys.ScopeStorage}})
	chatOnly := h.newKey(apikeys.CreateOptions{Name: "chat"})
	data := randomBytes(1<<20 + 12345) // five 256 KiB leaves: a dag-pb root

	if resp, _ := h.multipart("/v1/files", chatOnly, map[string]string{"purpose": "assistants"}, data, "notes.bin"); resp.StatusCode != 403 {
		t.Fatalf("upload without the storage scope: %d", resp.StatusCode)
	}
	resp, body := h.multipart("/v1/files", agent, map[string]string{"purpose": "assistants"}, data, "notes.bin")
	var f fileJSON
	if resp.StatusCode != 200 || json.Unmarshal(body, &f) != nil {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	if f.Object != "file" || f.Bytes != int64(len(data)) || f.Filename != "notes.bin" || f.Purpose != "assistants" || !f.Encrypted ||
		f.Copies != 1 || f.Providers[0] != h.provW.Address().Hex() || !strings.HasPrefix(f.ID, "file-") {
		t.Fatalf("file object: %s", body)
	}

	// The node bought a storage lease, paid directly from its stake.
	leases := h.store.Leases()
	if len(leases) != 1 || leases[0].Listing != h.ids["ipfs-256mib"] || leases[0].Quota != 256<<20 {
		t.Fatalf("leases: %+v", leases)
	}
	if cs := h.fc.Session(leases[0].Session); cs == nil || !cs.IsDirectPaymentFromUser || cs.User != h.consumer.Address() {
		t.Fatalf("lease session on chain: %+v", cs)
	}

	// The provider pinned the client's CID and holds only ciphertext.
	root, err := ipfs.Parse(f.CID)
	if err != nil || !h.kubo.Pinned(root) {
		t.Fatalf("cid %s not pinned (%v)", f.CID, err)
	}
	kc, _ := kubo.New(h.kubo.URL)
	rc, err := kc.Cat(ctx, f.CID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if lead, _ := io.ReadAll(rc); string(lead) != "MCE1" {
		t.Fatalf("stored bytes start %q, want the encrypted container", lead)
	}
	rc.Close()

	// Each key sees only its own files.
	var list struct {
		Data []fileJSON `json:"data"`
	}
	_, body = h.do("GET", "/v1/files", agent, nil)
	if json.Unmarshal(body, &list) != nil || len(list.Data) != 1 || list.Data[0].ID != f.ID {
		t.Fatalf("list: %s", body)
	}
	_, body = h.do("GET", "/v1/files", other, nil)
	if json.Unmarshal(body, &list) != nil || len(list.Data) != 0 {
		t.Fatalf("another key's list: %s", body)
	}
	if resp, _ := h.do("GET", "/v1/files/"+f.ID+"/content", other, nil); resp.StatusCode != 404 {
		t.Fatalf("another key's download: %d", resp.StatusCode)
	}

	// Downloads are verified against the CID and decrypted.
	resp, body = h.do("GET", "/v1/files/"+f.ID+"/content", agent, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) || !strings.Contains(resp.Header.Get("Content-Disposition"), "notes.bin") {
		t.Fatalf("download: %d, %d bytes, %q", resp.StatusCode, len(body), resp.Header.Get("Content-Disposition"))
	}

	// The provider proves it still holds a random block.
	var audit struct {
		Results []storage.AuditResult `json:"results"`
	}
	resp, body = h.do("POST", "/admin/storage/audit", agent, nil)
	if resp.StatusCode != 200 || json.Unmarshal(body, &audit) != nil || len(audit.Results) != 1 || !audit.Results[0].OK {
		t.Fatalf("audit: %d %s", resp.StatusCode, body)
	}

	// A new machine with only the wallet recovers the file, name included.
	fresh := h.newStore("recovered.json")
	if n, err := fresh.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	rf := fresh.Files()[0]
	if rf.Name != "notes.bin" || !rf.Encrypted || rf.Size != int64(len(data)) || rf.CID != f.CID || len(fresh.Leases()) != 1 {
		t.Fatalf("recovered: %+v leases=%d", rf, len(fresh.Leases()))
	}
	_, rc, err = fresh.Open(ctx, rf.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("recovered download: %v", err)
	}

	// Bytes a provider corrupted never reach the client: the download is
	// checked against the CID before it is decrypted.
	rootBlock, err := kc.BlockGet(ctx, f.CID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := ipfs.DecodePBNode(rootBlock)
	if err != nil || len(node.Links) < 2 {
		t.Fatalf("root node: %v", err)
	}
	h.kubo.CorruptBlock(node.Links[1].Hash)
	if resp, body := h.do("GET", "/v1/files/"+f.ID+"/content", agent, nil); resp.StatusCode != 502 || !strings.Contains(string(body), "does not match") {
		t.Fatalf("download of a corrupted copy: %d %s", resp.StatusCode, body)
	}
	// Every audit path starts at the root: corrupting it fails any challenge.
	h.kubo.CorruptBlock(root)
	_, body = h.do("POST", "/admin/storage/audit", agent, nil)
	if json.Unmarshal(body, &audit) != nil || len(audit.Results) != 1 || audit.Results[0].OK {
		t.Fatalf("audit of a corrupted copy: %s", body)
	}

	// Deleting unpins it on the provider.
	resp, body = h.do("DELETE", "/v1/files/"+f.ID, agent, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"deleted":true`) || h.kubo.Pinned(root) {
		t.Fatalf("delete: %d %s pinned=%v", resp.StatusCode, body, h.kubo.Pinned(root))
	}
	_, body = h.do("GET", "/v1/files", agent, nil)
	if json.Unmarshal(body, &list) != nil || len(list.Data) != 0 {
		t.Fatalf("list after delete: %s", body)
	}

	// Storage listings are not chat models.
	_, body = h.do("GET", "/v1/models", agent, nil)
	if strings.Contains(string(body), "ipfs-256mib") {
		t.Fatalf("storage listing in /v1/models: %s", body)
	}
	if resp, _ := h.do("POST", "/v1/chat/completions", agent, map[string]any{"model": "ipfs-256mib", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}); resp.StatusCode != 400 {
		t.Fatalf("chat with a storage listing: %d", resp.StatusCode)
	}
}

// TestStorageLeaseRenewal checks the keeper buys the next lease before the
// current one runs out, once, and that public files are stored as they are.
func TestStorageLeaseRenewal(t *testing.T) {
	h := storageHarness(t)
	ctx := context.Background()
	st := storage.NewStore(storage.StoreOptions{
		Path: h.dir + "/renew.json", TempDir: h.dir + "/tmp", Lease: time.Hour, RenewBefore: 30 * time.Minute, Replicas: 1, Encrypt: true,
	}, h.consumer, h.ck, h.market, h.mgr, morrpc.DefaultClient(), h.bus, logger())
	// The marketplace grants 20 minutes at first, so the first lease ends
	// inside the renewal window.
	h.fc.SetMaxDuration(20 * 60)
	data := []byte(strings.Repeat("public dataset row\n", 20000))
	f, err := st.Put(ctx, bytes.NewReader(data), storage.PutOptions{Name: "rows.txt", ContentType: "text/plain", Public: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.Encrypted || f.StoredBytes != uint64(len(data)) {
		t.Fatalf("public file: %+v", f)
	}
	root, _ := ipfs.Parse(f.CID)
	if want, _, _ := ipfs.Import(bytes.NewReader(data), nil); want != root {
		t.Fatalf("public file cid %s, plain bytes hash to %s", root, want)
	}
	if l := st.Leases(); len(l) != 1 || !l[0].Attached || time.Until(l[0].EndsAt) > 21*time.Minute {
		t.Fatalf("first lease: %+v", l)
	}
	h.fc.SetMaxDuration(86400)
	st.Maintain(ctx)
	leases := st.Leases()
	if len(leases) != 2 || leases[0].Session == leases[1].Session || leases[0].Provider != leases[1].Provider || !leases[1].Attached {
		t.Fatalf("leases after renewal: %+v", leases)
	}
	for _, l := range leases {
		if cs := h.fc.Session(l.Session); cs == nil || cs.Closed() || common.Hash(cs.BidId) != l.Bid {
			t.Fatalf("lease %s on chain: %+v", l.Session.Hex(), cs)
		}
	}
	// The new lease covers the window: another pass buys nothing.
	st.Maintain(ctx)
	if n := len(st.Leases()); n != 2 {
		t.Fatalf("%d leases after a second pass", n)
	}
	if err := st.Delete(ctx, f.ID); err != nil || h.kubo.Pinned(root) {
		t.Fatalf("delete: %v pinned=%v", err, h.kubo.Pinned(root))
	}
}

// TestStoragePriceCap checks storage.max_price keeps the node off providers
// that charge more.
func TestStoragePriceCap(t *testing.T) {
	h := storageHarness(t)
	ctx := context.Background()
	// The test provider sells 256 MiB for 0.001 MOR/s: about 10,000 MOR per
	// GiB-month. Cap at 1 MOR.
	st := storage.NewStore(storage.StoreOptions{
		Path: h.dir + "/capped.json", TempDir: h.dir + "/tmp", Lease: time.Hour, RenewBefore: 10 * time.Minute, Replicas: 1,
		MaxPrice: chaintest.MORs(1),
	}, h.consumer, h.ck, h.market, h.mgr, morrpc.DefaultClient(), h.bus, logger())
	if _, err := st.Put(ctx, strings.NewReader("too dear"), storage.PutOptions{Name: "x"}); err == nil || !strings.Contains(err.Error(), "no provider sells storage") {
		t.Fatalf("store above the price cap: %v", err)
	}
	if len(st.Leases()) != 0 {
		t.Fatal("a lease was bought above the price cap")
	}
}
