package ipfstest_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/ipfstest"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
)

// The storage flow end to end: the consumer computes the CID, the provider
// streams a CAR into Kubo, and audits pass until the provider loses data.
func TestAuditFlow(t *testing.T) {
	ctx := context.Background()
	node := ipfstest.New()
	defer node.Close()
	kc, err := kubo.New(node.URL)
	if err != nil {
		t.Fatal(err)
	}

	file := bytes.Repeat([]byte("morpheus storage "), 40000) // 680 KB: three leaves
	root, size, err := ipfs.Import(bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, err := ipfs.WriteCAR(pw, bytes.NewReader(file), root, ipfs.Options{})
		pw.CloseWithError(err)
	}()
	if _, err := kc.DagImport(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if !node.Pinned(root) || node.Calls("dag/import") != 1 {
		t.Fatal("not pinned")
	}

	get := func(ctx context.Context, c ipfs.CID) ([]byte, error) { return kc.BlockGet(ctx, c.String()) }
	audit := func(off uint64) error {
		proof, err := ipfs.ProvePath(ctx, get, root, off)
		if err != nil {
			return err
		}
		return ipfs.VerifyPath(root, off, size, proof)
	}
	for _, off := range []uint64{0, ipfs.DefaultChunkSize, size - 1} {
		if err := audit(off); err != nil {
			t.Fatalf("offset %d: %v", off, err)
		}
	}

	rc, err := kc.Cat(ctx, root.String(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.Verify(rc, root); err != nil {
		t.Fatal(err)
	}
	rc.Close()

	// The text's period does not divide the chunk size, so all three leaves
	// differ and each can be damaged on its own.
	rootBlock, err := kc.BlockGet(ctx, root.String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := ipfs.DecodePBNode(rootBlock)
	if err != nil || len(n.Links) != 3 {
		t.Fatalf("root: %d links, %v", len(n.Links), err)
	}
	node.CorruptBlock(n.Links[1].Hash)
	if audit(ipfs.DefaultChunkSize+5) == nil {
		t.Fatal("audit passed over a corrupt block")
	}
	node.DeleteBlock(n.Links[2].Hash)
	if audit(size-1) == nil {
		t.Fatal("audit passed over a lost block")
	}
	if node.Has(n.Links[2].Hash) || !node.Has(n.Links[0].Hash) {
		t.Fatal("Has")
	}
	if err := audit(0); err != nil {
		t.Fatalf("untouched leaf: %v", err)
	}

	if c := node.AddFile([]byte("tiny")); !node.Pinned(c) || c != ipfs.RawCID([]byte("tiny")) {
		t.Fatal("AddFile")
	}
}
