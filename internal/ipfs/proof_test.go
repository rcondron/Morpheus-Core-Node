package ipfs

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestProveVerify(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		size  int
		opts  Options
		depth int
	}{
		{1, Options{}, 1},
		{DefaultChunkSize, Options{}, 1},
		{3*DefaultChunkSize + 5, Options{}, 2},
		{20*100 + 7, Options{ChunkSize: 100, MaxLinks: 3}, 4},
		{9 * 100, Options{ChunkSize: 100, MaxLinks: 3}, 3},
	} {
		data := testData(t, tc.size)
		root, size, blocks := importAll(t, data, tc.opts)
		chunk := uint64(tc.opts.ChunkSize)
		if chunk == 0 {
			chunk = DefaultChunkSize
		}
		for _, off := range []uint64{0, 1, chunk - 1, chunk, chunk + 1, size / 2, size - 1} {
			if off >= size {
				continue
			}
			proof, err := ProvePath(ctx, blocks.get, root, off)
			if err != nil {
				t.Fatalf("size %d offset %d: %v", tc.size, off, err)
			}
			if len(proof) != tc.depth {
				t.Fatalf("size %d: proof of %d blocks, want %d", tc.size, len(proof), tc.depth)
			}
			start := off - off%chunk
			if leaf := proof[len(proof)-1]; !bytes.Equal(leaf, data[start:min(start+chunk, size)]) {
				t.Fatalf("size %d offset %d: wrong leaf", tc.size, off)
			}
			if err := VerifyPath(root, off, size, proof); err != nil {
				t.Fatalf("size %d offset %d: %v", tc.size, off, err)
			}
		}
		if _, err := ProvePath(ctx, blocks.get, root, size); err == nil {
			t.Fatal("proved a byte past the end")
		}
	}
}

func TestVerifyPathRejects(t *testing.T) {
	data := testData(t, 20*100+7)
	opts := Options{ChunkSize: 100, MaxLinks: 3}
	root, size, blocks := importAll(t, data, opts)
	proof, err := ProvePath(context.Background(), blocks.get, root, 1234)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ProvePath(context.Background(), blocks.get, root, 5)
	clone := func() [][]byte {
		p := make([][]byte, len(proof))
		for i := range proof {
			p[i] = bytes.Clone(proof[i])
		}
		return p
	}
	corrupt := clone()
	corrupt[len(corrupt)-1][0] ^= 1

	for name, tc := range map[string]struct {
		root   CID
		off    uint64
		size   uint64
		blocks [][]byte
		want   string
	}{
		"other root":      {RawCID([]byte("x")), 1234, size, proof, "hash"},
		"offset too big":  {root, size, size, proof, "beyond"},
		"size too big":    {root, 1234, size + 1, proof, "covers"},
		"size too small":  {root, 1234, size - 1, proof, "covers"},
		"other offset":    {root, 1234, size, other, "hash"},
		"extra block":     {root, 1234, size, append(clone(), proof[0]), "after the leaf"},
		"missing leaf":    {root, 1234, size, proof[:len(proof)-1], "before reaching a leaf"},
		"no blocks":       {root, 1234, size, nil, "before reaching a leaf"},
		"corrupt leaf":    {root, 1234, size, corrupt, "hash"},
		"blocks reversed": {root, 1234, size, [][]byte{proof[3], proof[2], proof[1], proof[0]}, "hash"},
	} {
		err := VerifyPath(tc.root, tc.off, tc.size, tc.blocks)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want error containing %q", name, err, tc.want)
		}
	}

	// A single-leaf file is its own proof, and its size is checked too.
	leaf := []byte("tiny file")
	if err := VerifyPath(RawCID(leaf), 3, 9, [][]byte{leaf}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPath(RawCID(leaf), 3, 10, [][]byte{leaf}); err == nil {
		t.Fatal("wrong size accepted")
	}
	if err := VerifyPath(RawCID(nil), 0, 0, [][]byte{{}}); err == nil {
		t.Fatal("an empty file has no byte to prove")
	}
}

// Nodes whose hashes check out but whose UnixFS metadata lies or has the
// wrong shape must still be rejected.
func TestVerifyPathMalformedNodes(t *testing.T) {
	a, b := []byte("aaaa"), []byte("bbbbbb")
	links := []PBLink{{Hash: RawCID(a), Tsize: 4}, {Hash: RawCID(b), Tsize: 6}}
	node := func(d UnixFSData, links []PBLink) (CID, []byte) {
		blk := EncodePBNode(PBNode{Links: links, Data: EncodeUnixFSData(d)})
		return DagPBCID(blk), blk
	}
	good := UnixFSData{Type: UnixFSFile, Filesize: 10, Blocksizes: []uint64{4, 6}}
	if c, blk := node(good, links); VerifyPath(c, 5, 10, [][]byte{blk, b}) != nil {
		t.Fatal("well-formed node rejected")
	}
	for name, d := range map[string]UnixFSData{
		"blocksizes lie":     {Type: UnixFSFile, Filesize: 10, Blocksizes: []uint64{4, 7}},
		"too few blocksizes": {Type: UnixFSFile, Filesize: 10, Blocksizes: []uint64{10}},
		"directory":          {Type: UnixFSDirectory, Filesize: 10, Blocksizes: []uint64{4, 6}},
		"inline data":        {Type: UnixFSFile, Data: []byte("x"), Filesize: 10, Blocksizes: []uint64{4, 6}},
		"shifted sizes":      {Type: UnixFSFile, Filesize: 10, Blocksizes: []uint64{6, 4}},
	} {
		c, blk := node(d, links)
		if err := VerifyPath(c, 5, 10, [][]byte{blk, b}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, blk := DagPBCID([]byte{0x0a, 0x02, 0x08, 0x02}), []byte{0x0a, 0x02, 0x08, 0x02}
	if err := VerifyPath(c, 0, 1, [][]byte{blk}); err == nil {
		t.Error("node without links accepted")
	}
}

func TestProvePathStoreErrors(t *testing.T) {
	data := testData(t, 1000)
	root, _, blocks := importAll(t, data, Options{ChunkSize: 100})
	n, _ := DecodePBNode(blocks[root])
	leaf := n.Links[3].Hash

	blocks[leaf] = append(bytes.Clone(blocks[leaf][:99]), blocks[leaf][99]^1)
	if _, err := ProvePath(context.Background(), blocks.get, root, 350); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt block: %v", err)
	}
	delete(blocks, leaf)
	if _, err := ProvePath(context.Background(), blocks.get, root, 350); err == nil || !strings.Contains(err.Error(), leaf.String()) {
		t.Fatalf("missing block: %v", err)
	}
	// Other leaves remain provable.
	if _, err := ProvePath(context.Background(), blocks.get, root, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProvePath(ctx, blocks.get, root, 0); err == nil {
		t.Fatal("cancelled context ignored")
	}
}
