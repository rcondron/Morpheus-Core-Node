// Package ipfs implements the slice of IPFS a storage marketplace needs, using
// only the standard library and byte-for-byte compatible with Kubo: CIDs, a
// streaming UnixFS file importer that reproduces
// `ipfs add --cid-version=1 --raw-leaves --chunker=size-262144`, the dag-pb and
// UnixFS codecs, CAR v1 streams and Merkle-path storage proofs.
//
// The intended flow: a consumer computes a file's CID locally (Import) before
// uploading it; the provider re-imports the bytes into a CAR stream (WriteCAR)
// for Kubo's dag/import, so the pinned CID never depends on the provider's
// Kubo import settings; the consumer then audits the provider by asking for
// the blocks from the root down to a random byte (ProvePath, VerifyPath).
package ipfs

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// Multicodec codes of the block formats a CID can name.
const (
	CodecRaw   = 0x55
	CodecDagPB = 0x70
)

// sha256Code is the sha2-256 multihash code, the only hash supported.
const sha256Code = 0x12

// b32 is multibase "b": RFC 4648 base32, lower case, no padding.
var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// CID identifies a sha2-256 block: CIDv1 with the raw or dag-pb codec, or
// CIDv0 (dag-pb). It holds the binary form, so CIDs compare with == and work
// as map keys. The zero value is undefined.
type CID struct{ b string }

// RawCID returns the CIDv1 of a raw block, the form of every UnixFS leaf.
func RawCID(block []byte) CID { return cidV1(CodecRaw, block) }

// DagPBCID returns the CIDv1 of a dag-pb block.
func DagPBCID(block []byte) CID { return cidV1(CodecDagPB, block) }

func cidV1(codec byte, block []byte) CID {
	sum := sha256.Sum256(block)
	// Every field is a single-byte varint: version 1, codec, sha2-256, 32 bytes.
	return CID{string(append([]byte{1, codec, sha256Code, 32}, sum[:]...))}
}

// CIDFromBytes parses a binary CID.
func CIDFromBytes(b []byte) (CID, error) {
	c, n, err := readCID(b)
	if err != nil {
		return CID{}, err
	}
	if n != len(b) {
		return CID{}, errors.New("cid: trailing bytes")
	}
	return c, nil
}

// readCID parses the binary CID at the start of b (as found in dag-pb links
// and CAR sections) and returns its length.
func readCID(b []byte) (CID, int, error) {
	if len(b) >= 2 && b[0] == sha256Code && b[1] == 32 {
		// CIDv0 is a bare sha2-256 multihash.
		if len(b) < 34 {
			return CID{}, 0, errors.New("cid: truncated")
		}
		return CID{string(b[:34])}, 34, nil
	}
	// CIDv1: varint version, codec, multihash code and digest length.
	var f [4]uint64
	n := 0
	for i := range f {
		v, m, err := uvarint(b[n:])
		if err != nil {
			return CID{}, 0, fmt.Errorf("cid: %w", err)
		}
		f[i] = v
		n += m
	}
	switch {
	case f[0] != 1:
		return CID{}, 0, fmt.Errorf("cid: unsupported version %d", f[0])
	case f[1] != CodecRaw && f[1] != CodecDagPB:
		return CID{}, 0, fmt.Errorf("cid: unsupported codec 0x%x (want raw or dag-pb)", f[1])
	case f[2] != sha256Code || f[3] != 32:
		return CID{}, 0, fmt.Errorf("cid: unsupported multihash 0x%x (want sha2-256)", f[2])
	case len(b) < n+32:
		return CID{}, 0, errors.New("cid: truncated")
	}
	return CID{string(b[:n+32])}, n + 32, nil
}

// Parse decodes a CIDv1 in base32 ("b…", how Kubo prints CIDv1) or a CIDv0
// ("Qm…"). Other multibases are rejected rather than half-supported, and so
// is any string that is not the canonical encoding of its CID.
func Parse(s string) (CID, error) {
	var b []byte
	var err error
	switch {
	case len(s) == 46 && strings.HasPrefix(s, "Qm"):
		b, err = base58Decode(s)
	case strings.HasPrefix(s, "b") && len(s) <= 128: // CIDv1 strings here are 59 bytes
		b, err = b32.DecodeString(s[1:])
	default:
		return CID{}, fmt.Errorf("unsupported CID %.80q: want CIDv1 in base32 (b…) or CIDv0 (Qm…)", s)
	}
	if err != nil {
		return CID{}, fmt.Errorf("invalid CID %.80q: %w", s, err)
	}
	c, err := CIDFromBytes(b)
	if err != nil {
		return CID{}, fmt.Errorf("invalid CID %.80q: %w", s, err)
	}
	if c.String() != s {
		return CID{}, fmt.Errorf("invalid CID %.80q: not canonical (want %s)", s, c)
	}
	return c, nil
}

// String encodes a CIDv1 as base32 with the "b" multibase prefix and a CIDv0
// as base58btc, as Kubo does. An undefined CID is "".
func (c CID) String() string {
	switch len(c.b) {
	case 0:
		return ""
	case 34:
		return base58Encode([]byte(c.b))
	}
	return "b" + b32.EncodeToString([]byte(c.b))
}

// Bytes returns the binary form.
func (c CID) Bytes() []byte { return []byte(c.b) }

// Defined reports whether c is not the zero value.
func (c CID) Defined() bool { return c.b != "" }

// Version is 1 for CIDv1 and 0 for CIDv0.
func (c CID) Version() int {
	if len(c.b) == 36 {
		return 1
	}
	return 0
}

// Codec returns CodecRaw or CodecDagPB (always dag-pb for CIDv0).
func (c CID) Codec() uint64 {
	switch len(c.b) {
	case 36:
		return uint64(c.b[1])
	case 34:
		return CodecDagPB
	}
	return 0
}

// Digest returns the 32-byte sha2-256 digest.
func (c CID) Digest() []byte {
	if !c.Defined() {
		return nil
	}
	return []byte(c.b[len(c.b)-32:])
}

// Verify reports whether block hashes to c.
func (c CID) Verify(block []byte) bool {
	if !c.Defined() {
		return false
	}
	sum := sha256.Sum256(block)
	return string(sum[:]) == c.b[len(c.b)-32:]
}

// MarshalText encodes c as String does, so CIDs read naturally in JSON.
func (c CID) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText parses s with Parse; an empty string is the undefined CID.
func (c *CID) UnmarshalText(s []byte) error {
	if len(s) == 0 {
		*c = CID{}
		return nil
	}
	p, err := Parse(string(s))
	if err != nil {
		return err
	}
	*c = p
	return nil
}
