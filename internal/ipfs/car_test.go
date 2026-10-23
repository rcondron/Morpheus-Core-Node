package ipfs

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCARHeaderBytes(t *testing.T) {
	// The same bytes `ipfs dag export` writes (see TestKuboDAGBytes).
	want := "3a" + "a2" + "65" + hex.EncodeToString([]byte("roots")) + "81" + "d82a" + "5825" + "00" +
		"01551220" + "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" +
		"67" + hex.EncodeToString([]byte("version")) + "01"
	var b bytes.Buffer
	if err := WriteCARHeader(&b, RawCID(nil)); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b.Bytes()) != want {
		t.Fatalf("got  %x\nwant %s", b.Bytes(), want)
	}
	if WriteCARHeader(&b) == nil || WriteCARHeader(&b, CID{}) == nil {
		t.Fatal("a CAR needs defined roots")
	}
}

func TestCARRoundTrip(t *testing.T) {
	data := testData(t, 5000)
	opts := Options{ChunkSize: 256, MaxLinks: 4}
	root, size, blocks := importAll(t, data, opts)

	var car bytes.Buffer
	n, err := WriteCAR(&car, bytes.NewReader(data), root, opts)
	if err != nil || n != size {
		t.Fatal(n, err)
	}
	cr, err := NewCARReader(bytes.NewReader(car.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Roots) != 1 || cr.Roots[0] != root {
		t.Fatalf("roots %v", cr.Roots)
	}
	got := blockMap{}
	for {
		c, b, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[c] = b
	}
	if len(got) != len(blocks) {
		t.Fatalf("%d blocks, want %d", len(got), len(blocks))
	}
	for c, b := range blocks {
		if !bytes.Equal(got[c], b) {
			t.Fatalf("block %s differs", c)
		}
	}

	// The CAR names the expected root up front; different content fails.
	car.Reset()
	if _, err := WriteCAR(&car, bytes.NewReader(data[1:]), root, opts); err == nil || !strings.Contains(err.Error(), root.String()) {
		t.Fatalf("mismatch: %v", err)
	}
}

func TestCARRoots(t *testing.T) {
	v0, _ := Parse(emptyV0)
	roots := []CID{v0, RawCID([]byte("a")), DagPBCID([]byte("b"))}
	var b bytes.Buffer
	if err := WriteCARHeader(&b, roots...); err != nil {
		t.Fatal(err)
	}
	cr, err := NewCARReader(&b)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Roots) != 3 || cr.Roots[0] != v0 || cr.Roots[2] != roots[2] {
		t.Fatalf("roots %v", cr.Roots)
	}
	if _, _, err := cr.Next(); err != io.EOF {
		t.Fatalf("empty body: %v", err)
	}
}

func TestCARReaderRejects(t *testing.T) {
	var good bytes.Buffer
	block := []byte("some block")
	_ = WriteCARHeader(&good, RawCID(block))
	_ = WriteCARBlock(&good, RawCID(block), block)
	full := good.Bytes()

	corrupt := bytes.Clone(full)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := readAllCAR(corrupt); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("corrupt block: %v", err)
	}
	if _, err := readAllCAR(full[:len(full)-3]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated block: %v", err)
	}
	if _, err := readAllCAR(full[:20]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header: %v", err)
	}
	if _, err := readAllCAR(nil); err == nil {
		t.Fatal("empty stream accepted")
	}

	header := func(h string) []byte {
		b, _ := hex.DecodeString(h)
		return append([]byte{byte(len(b))}, b...)
	}
	for name, h := range map[string]string{
		"version 2":     "a1" + "67" + hex.EncodeToString([]byte("version")) + "02",
		"no roots":      "a2" + "65" + hex.EncodeToString([]byte("roots")) + "80" + "67" + hex.EncodeToString([]byte("version")) + "01",
		"unknown field": "a1" + "63" + hex.EncodeToString([]byte("foo")) + "01",
		"not a map":     "80",
		"untagged root": "a2" + "65" + hex.EncodeToString([]byte("roots")) + "81" + "4100" + "67" + hex.EncodeToString([]byte("version")) + "01",
	} {
		if _, err := NewCARReader(bytes.NewReader(header(h))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	var big bytes.Buffer
	_ = WriteCARHeader(&big, RawCID(nil))
	_ = WriteCARBlock(&big, RawCID(nil), make([]byte, MaxBlockSize+100))
	if _, err := readAllCAR(big.Bytes()); err == nil {
		t.Fatal("oversized block accepted")
	}
}

func readAllCAR(b []byte) (int, error) {
	cr, err := NewCARReader(bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		if _, _, err := cr.Next(); err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
		}
		n++
	}
}
