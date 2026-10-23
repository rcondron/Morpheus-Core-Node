package ipfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The CID must not depend on how the stream is cut into writes.
func TestImporterWriteSizes(t *testing.T) {
	data := testData(t, 20000)
	opts := Options{ChunkSize: 1000, MaxLinks: 4}
	want, _, _ := importAll(t, data, opts)
	for _, step := range []int{1, 7, 999, 1000, 1001, 4096} {
		im, err := NewImporter(opts, nil)
		if err != nil {
			t.Fatal(err)
		}
		for p := data; len(p) > 0; {
			k := min(step, len(p))
			if n, err := im.Write(p[:k]); err != nil || n != k {
				t.Fatalf("write %d: %d %v", k, n, err)
			}
			p = p[k:]
		}
		got, size, err := im.Finish()
		if err != nil || got != want || size != uint64(len(data)) {
			t.Fatalf("step %d: %s %d %v, want %s", step, got, size, err, want)
		}
	}
	got, size, err := func() (CID, uint64, error) {
		im, _ := NewImporter(opts, nil)
		if _, err := io.Copy(im, iotestHalfReader{bytes.NewReader(data)}); err != nil {
			return CID{}, 0, err
		}
		return im.Finish()
	}()
	if err != nil || got != want || size != uint64(len(data)) {
		t.Fatalf("ReadFrom: %s %d %v", got, size, err)
	}
}

// iotestHalfReader returns short reads to exercise ReadFrom's refill loop.
type iotestHalfReader struct{ r io.Reader }

func (h iotestHalfReader) Read(p []byte) (int, error) {
	return h.r.Read(p[:(len(p)+1)/2])
}

// Walk the emitted DAG and check the shape Kubo's balanced layout produces.
func TestImporterDAGShape(t *testing.T) {
	data := testData(t, 3*3*3*100+150) // 29 leaves of 100 bytes with fan-out 3: depth 4
	var order []CID
	blocks := blockMap{}
	im, err := NewImporter(Options{ChunkSize: 100, MaxLinks: 3}, func(c CID, b []byte) error {
		if !c.Verify(b) {
			t.Fatalf("emitted block does not hash to %s", c)
		}
		order = append(order, c)
		return blocks.put(c, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := im.Write(data); err != nil {
		t.Fatal(err)
	}
	root, _, err := im.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if order[len(order)-1] != root {
		t.Fatal("root must be emitted last")
	}
	seen := map[CID]bool{}
	var out []byte
	var walk func(c CID, depth int) (size, tsize uint64)
	walk = func(c CID, depth int) (uint64, uint64) {
		if seen[c] {
			t.Fatalf("%s reached twice", c)
		}
		seen[c] = true
		b := blocks[c]
		if c.Codec() == CodecRaw {
			if depth != 4 {
				t.Fatalf("leaf at depth %d, want every leaf at depth 4", depth)
			}
			out = append(out, b...)
			return uint64(len(b)), uint64(len(b))
		}
		n, d, err := fileNode(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(n.Links) > 3 {
			t.Fatalf("%d links", len(n.Links))
		}
		var size, tsize uint64
		for i, l := range n.Links {
			s, ts := walk(l.Hash, depth+1)
			if s != d.Blocksizes[i] || ts != l.Tsize || l.Name != "" {
				t.Fatalf("link %d: size %d/%d tsize %d/%d", i, s, d.Blocksizes[i], ts, l.Tsize)
			}
			size += s
			tsize += ts
		}
		if size != d.Filesize {
			t.Fatalf("filesize %d, children hold %d", d.Filesize, size)
		}
		return size, tsize + uint64(len(b))
	}
	walk(root, 0)
	if !bytes.Equal(out, data) || len(seen) != len(blocks) {
		t.Fatalf("DAG does not reproduce the file (%d of %d blocks reachable)", len(seen), len(blocks))
	}
}

func TestImporterErrors(t *testing.T) {
	for _, opts := range []Options{{ChunkSize: -1}, {ChunkSize: MaxChunkSize + 1}, {MaxLinks: 1}, {MaxLinks: -5}, {MaxLinks: linksLimit + 1}} {
		if _, err := NewImporter(opts, nil); err == nil {
			t.Errorf("%+v accepted", opts)
		}
	}
	if _, err := NewImporter(Options{ChunkSize: MaxChunkSize, MaxLinks: linksLimit}, nil); err != nil {
		t.Fatal(err)
	}
	// The widest node, with the largest varints, stays importable everywhere.
	n := PBNode{Links: make([]PBLink, linksLimit)}
	d := UnixFSData{Type: UnixFSFile, Filesize: 1 << 62, Blocksizes: make([]uint64, linksLimit)}
	for i := range n.Links {
		n.Links[i] = PBLink{Hash: DagPBCID(nil), Tsize: 1 << 62}
		d.Blocksizes[i] = 1 << 50
	}
	n.Data = EncodeUnixFSData(d)
	if size := len(EncodePBNode(n)); size > 1<<20 {
		t.Fatalf("widest node is %d bytes", size)
	}

	boom := errors.New("disk full")
	calls := 0
	im, _ := NewImporter(Options{ChunkSize: 10}, func(CID, []byte) error {
		if calls++; calls == 3 {
			return boom
		}
		return nil
	})
	if _, err := im.Write(make([]byte, 35)); !errors.Is(err, boom) {
		t.Fatalf("write: %v", err)
	}
	if _, err := im.Write([]byte{1}); !errors.Is(err, boom) {
		t.Fatal("emit errors must be sticky")
	}
	if _, _, err := im.Finish(); !errors.Is(err, boom) {
		t.Fatalf("finish: %v", err)
	}

	im, _ = NewImporter(Options{}, nil)
	if _, _, err := im.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := im.Finish(); err == nil {
		t.Fatal("second Finish succeeded")
	}
	if _, err := im.Write([]byte{1}); err == nil {
		t.Fatal("Write after Finish succeeded")
	}

	readErr := errors.New("connection reset")
	if _, _, err := Import(io.MultiReader(bytes.NewReader(make([]byte, 5)), errReader{readErr}), nil); !errors.Is(err, readErr) {
		t.Fatalf("read error: %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestImportAndVerify(t *testing.T) {
	data := testData(t, 3*DefaultChunkSize+17)
	root, size, err := Import(bytes.NewReader(data), nil)
	if err != nil || size != uint64(len(data)) {
		t.Fatal(size, err)
	}
	if err := Verify(bytes.NewReader(data), root); err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := Verify(bytes.NewReader(data), root); err == nil {
		t.Fatal("tampered content verified")
	}
	small := []byte("hello")
	if c, _, _ := Import(bytes.NewReader(small), nil); c != RawCID(small) {
		t.Fatal("a one-chunk file is its raw leaf")
	}
}
