package ipfs

import (
	"bytes"
	"encoding/hex"
	"io"
	"reflect"
	"testing"
)

// Decoders see bytes from untrusted peers: they must never panic, and what
// they accept must re-encode to something that decodes the same way.

func FuzzParse(f *testing.F) {
	for _, s := range []string{emptyRaw, emptyV0, "bafkqaaa", "b", "Qm", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Parse(s)
		if err != nil {
			return
		}
		if c.String() != s {
			t.Fatalf("%q parsed to %s", s, c)
		}
		if back, err := CIDFromBytes(c.Bytes()); err != nil || back != c {
			t.Fatalf("binary round trip: %v", err)
		}
	})
}

func FuzzDecodePBNode(f *testing.F) {
	root, _ := hex.DecodeString(kuboRoot3000)
	f.Add(root)
	f.Add([]byte{0x0a, 0x00})
	f.Add([]byte{0x12, 0x02, 0x12, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := DecodePBNode(b)
		if err != nil {
			return
		}
		again, err := DecodePBNode(EncodePBNode(n))
		if err != nil || !reflect.DeepEqual(again, n) {
			t.Fatalf("re-encoded node decodes as %+v (%v), want %+v", again, err, n)
		}
		if d, err := DecodeUnixFSData(n.Data); err == nil {
			if again, err := DecodeUnixFSData(EncodeUnixFSData(d)); err != nil || !reflect.DeepEqual(again, d) {
				t.Fatalf("unixfs round trip: %+v %v", again, err)
			}
		}
	})
}

func FuzzCARReader(f *testing.F) {
	var car bytes.Buffer
	_ = WriteCARHeader(&car, RawCID([]byte("x")))
	_ = WriteCARBlock(&car, RawCID([]byte("x")), []byte("x"))
	f.Add(car.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		cr, err := NewCARReader(bytes.NewReader(b))
		if err != nil {
			return
		}
		for range 1000 {
			c, blk, err := cr.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				return
			}
			if !c.Verify(blk) {
				t.Fatal("unverified block returned")
			}
		}
	})
}
