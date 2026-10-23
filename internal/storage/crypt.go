// Package storage sells and buys IPFS storage through Morpheus marketplace
// sessions: a provider-side service that pins users' files on a Kubo node,
// and a consumer-side store that encrypts, uploads, renews leases and audits.
package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Encrypted container ("MCE1"):
//
//	magic "MCE1" | header length (uint32 BE) | header | segments
//
// The header is the file's metadata and data key, ECIES-encrypted to the
// owner's wallet key, so only that wallet can read the name or the content.
// Segments are AES-256-GCM over SegmentSize bytes of plaintext each; the nonce
// is an 8-byte random prefix and a 4-byte counter, and the additional data is
// one byte marking the final segment, so segments cannot be reordered,
// dropped or truncated undetected.
const (
	containerMagic = "MCE1"
	SegmentSize    = 64 << 10
	tagSize        = 16
	maxHeader      = 1 << 16
)

// Meta is what the encrypted header holds.
type Meta struct {
	Name        string `json:"name"`
	ContentType string `json:"type,omitempty"`
	Size        int64  `json:"size"` // plaintext bytes
	Created     int64  `json:"created"`
	Key         string `json:"key"`   // hex AES-256 key
	Nonce       string `json:"nonce"` // hex 8-byte nonce prefix
}

// SealedSize is the container size for size bytes of plaintext and a header
// of headerLen bytes.
func SealedSize(size int64, headerLen int) int64 {
	segs := (size + SegmentSize - 1) / SegmentSize
	if segs == 0 {
		segs = 1 // an empty file is one empty final segment
	}
	return int64(len(containerMagic)+4+headerLen) + size + segs*tagSize
}

// Seal writes plaintext from r as a container encrypted to ownerPub. meta.Size
// must be the exact plaintext length.
func Seal(w io.Writer, r io.Reader, meta Meta, ownerPub []byte) error {
	key := make([]byte, 32)
	prefix := make([]byte, 8)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	meta.Key, meta.Nonce = hex.EncodeToString(key), hex.EncodeToString(prefix)
	hdrPlain, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	encHex, err := morrpc.EncryptTo(ownerPub, hdrPlain)
	if err != nil {
		return fmt.Errorf("encrypt header: %w", err)
	}
	hdr, _ := hex.DecodeString(encHex)
	var lead [8]byte
	copy(lead[:], containerMagic)
	binary.BigEndian.PutUint32(lead[4:], uint32(len(hdr)))
	if _, err := w.Write(lead[:]); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	buf := make([]byte, SegmentSize)
	out := make([]byte, 0, SegmentSize+tagSize)
	var written int64
	for i := uint32(0); ; i++ {
		n, rerr := io.ReadFull(r, buf)
		if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
			return rerr
		}
		written += int64(n)
		final := written >= meta.Size
		if written > meta.Size {
			return errors.New("input is longer than the declared size")
		}
		if n < SegmentSize && !final {
			return errors.New("input is shorter than the declared size")
		}
		out = aead.Seal(out[:0], nonce(prefix, i), buf[:n], finalAD(final))
		if _, err := w.Write(out); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

// Decrypter opens data encrypted to the owner's key (a wallet).
type Decrypter interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// ReadMeta reads and decrypts a container header, leaving r at the first segment.
func ReadMeta(r io.Reader, d Decrypter) (*Meta, error) {
	var lead [8]byte
	if _, err := io.ReadFull(r, lead[:]); err != nil {
		return nil, fmt.Errorf("read container: %w", err)
	}
	if string(lead[:4]) != containerMagic {
		return nil, errors.New("not an encrypted morcore file")
	}
	n := binary.BigEndian.Uint32(lead[4:])
	if n == 0 || n > maxHeader {
		return nil, errors.New("corrupt container header")
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("read container header: %w", err)
	}
	plain, err := d.Decrypt(hdr)
	if err != nil {
		return nil, errors.New("this file was not encrypted to this wallet")
	}
	var m Meta
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("corrupt container header: %w", err)
	}
	return &m, nil
}

// HeaderLength reads the 8-byte lead of a container and returns the length
// of magic, length field and header together (the offset of the first segment).
func HeaderLength(lead []byte) (int64, error) {
	if len(lead) < 8 || string(lead[:4]) != containerMagic {
		return 0, errors.New("not an encrypted morcore file")
	}
	n := binary.BigEndian.Uint32(lead[4:8])
	if n == 0 || n > maxHeader {
		return 0, errors.New("corrupt container header")
	}
	return 8 + int64(n), nil
}

// Unseal decrypts a container from r into w and returns its metadata. Every
// segment is authenticated; a modified, reordered or truncated container fails.
func Unseal(w io.Writer, r io.Reader, d Decrypter) (*Meta, error) {
	m, err := ReadMeta(r, d)
	if err != nil {
		return nil, err
	}
	key, err1 := hex.DecodeString(m.Key)
	prefix, err2 := hex.DecodeString(m.Nonce)
	if err1 != nil || err2 != nil || len(key) != 32 || len(prefix) != 8 || m.Size < 0 {
		return nil, errors.New("corrupt container header")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, SegmentSize+tagSize)
	plain := make([]byte, 0, SegmentSize)
	remaining := m.Size
	for i := uint32(0); ; i++ {
		segLen := int64(SegmentSize)
		if remaining < segLen {
			segLen = remaining
		}
		final := remaining <= SegmentSize
		if _, err := io.ReadFull(r, buf[:segLen+tagSize]); err != nil {
			return nil, fmt.Errorf("container is truncated: %w", err)
		}
		plain, err = aead.Open(plain[:0], nonce(prefix, i), buf[:segLen+tagSize], finalAD(final))
		if err != nil {
			return nil, errors.New("container failed authentication (corrupted or tampered)")
		}
		if _, err := w.Write(plain); err != nil {
			return nil, err
		}
		remaining -= segLen
		if final {
			break
		}
	}
	var extra [1]byte
	if n, _ := r.Read(extra[:]); n > 0 {
		return nil, errors.New("container has trailing data")
	}
	return m, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, i uint32) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[8:], i)
	return n
}

func finalAD(final bool) []byte {
	if final {
		return []byte{1}
	}
	return []byte{0}
}

// IsSealed reports whether data starts like an encrypted container.
func IsSealed(data []byte) bool { return bytes.HasPrefix(data, []byte(containerMagic)) }
