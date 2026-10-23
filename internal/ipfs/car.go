package ipfs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxCARHeader bounds the header a CARReader accepts; real ones are ~60 bytes
// per root.
const maxCARHeader = 1 << 20

// WriteCARHeader writes a CAR v1 header naming roots: a varint length then
// the DAG-CBOR map {"roots": [CIDs], "version": 1}, keys in canonical order.
func WriteCARHeader(w io.Writer, roots ...CID) error {
	if len(roots) == 0 {
		return errors.New("car: at least one root is required")
	}
	h := []byte{0xa2} // map, 2 entries
	h = appendCBORText(h, "roots")
	h = appendCBORHead(h, 4, uint64(len(roots)))
	for _, c := range roots {
		if !c.Defined() {
			return errors.New("car: undefined root")
		}
		// A CID is tag 42 over its bytes behind a 0x00 multibase prefix.
		h = append(h, 0xd8, 42)
		h = appendCBORHead(h, 2, uint64(len(c.b)+1))
		h = append(h, 0)
		h = append(h, c.b...)
	}
	h = appendCBORText(h, "version")
	h = append(h, 0x01)
	_, err := w.Write(append(binary.AppendUvarint(nil, uint64(len(h))), h...))
	return err
}

// WriteCARBlock writes one CAR v1 section: varint length, CID, block.
func WriteCARBlock(w io.Writer, c CID, block []byte) error {
	if !c.Defined() {
		return errors.New("car: undefined CID")
	}
	head := binary.AppendUvarint(make([]byte, 0, 48), uint64(len(c.b)+len(block)))
	if _, err := w.Write(append(head, c.b...)); err != nil {
		return err
	}
	_, err := w.Write(block)
	return err
}

// WriteCAR imports r with opts and streams the DAG to w as a CAR v1 naming
// root, the CID the content is expected to have (the header comes first, so
// it must be known up front, e.g. announced by the consumer). It returns the
// file size, and an error after the last block if the content does not hash
// to root; a caller piping w into Kubo's dag/import should then abort the
// upload. Kubo only pins a root whose whole DAG it holds, so a mismatched CAR
// can never pin the wrong content either way.
// ErrRootMismatch reports content that does not hash to the announced root.
var ErrRootMismatch = errors.New("car: content does not match the root")

func WriteCAR(w io.Writer, r io.Reader, root CID, opts Options) (uint64, error) {
	if err := WriteCARHeader(w, root); err != nil {
		return 0, err
	}
	im, err := NewImporter(opts, func(c CID, block []byte) error { return WriteCARBlock(w, c, block) })
	if err != nil {
		return 0, err
	}
	if _, err := im.ReadFrom(r); err != nil {
		return 0, err
	}
	got, size, err := im.Finish()
	if err != nil {
		return 0, err
	}
	if got != root {
		return 0, fmt.Errorf("%w: content is %s, want %s", ErrRootMismatch, got, root)
	}
	return size, nil
}

func appendCBORHead(b []byte, major byte, n uint64) []byte {
	m := major << 5
	switch {
	case n < 24:
		return append(b, m|byte(n))
	case n <= 0xff:
		return append(b, m|24, byte(n))
	case n <= 0xffff:
		return binary.BigEndian.AppendUint16(append(b, m|25), uint16(n))
	case n <= 0xffffffff:
		return binary.BigEndian.AppendUint32(append(b, m|26), uint32(n))
	}
	return binary.BigEndian.AppendUint64(append(b, m|27), n)
}

func appendCBORText(b []byte, s string) []byte {
	return append(appendCBORHead(b, 3, uint64(len(s))), s...)
}

// CARReader reads a CAR v1 stream and checks every block against its CID.
type CARReader struct {
	Roots []CID
	r     *bufio.Reader
}

// NewCARReader reads the header of a CAR v1 stream.
func NewCARReader(r io.Reader) (*CARReader, error) {
	cr := &CARReader{r: bufio.NewReader(r)}
	h, err := cr.section(maxCARHeader)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, fmt.Errorf("car header: %w", err)
	}
	if cr.Roots, err = parseCARHeader(h); err != nil {
		return nil, fmt.Errorf("car header: %w", err)
	}
	return cr, nil
}

// Next returns the next block, or io.EOF after the last one. Blocks larger
// than MaxBlockSize are rejected, as Kubo does.
func (cr *CARReader) Next() (CID, []byte, error) {
	s, err := cr.section(MaxBlockSize + 64)
	if err != nil {
		return CID{}, nil, err
	}
	c, n, err := readCID(s)
	if err != nil {
		return CID{}, nil, fmt.Errorf("car: %w", err)
	}
	block := s[n:]
	if !c.Verify(block) {
		return CID{}, nil, fmt.Errorf("car: block does not hash to %s", c)
	}
	return c, block, nil
}

// section reads one varint-length-prefixed section.
func (cr *CARReader) section(limit uint64) ([]byte, error) {
	n, err := binary.ReadUvarint(cr.r)
	if err != nil {
		return nil, err // io.EOF only on a clean section boundary
	}
	if n == 0 || n > limit {
		return nil, fmt.Errorf("car: section length %d out of range", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(cr.r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// parseCARHeader decodes the {"roots", "version"} DAG-CBOR map and nothing
// else; CARv2 and unknown fields are rejected.
func parseCARHeader(b []byte) ([]CID, error) {
	p := cborParser{b: b}
	entries, err := p.expect(5)
	if err != nil {
		return nil, err
	}
	var roots []CID
	version := uint64(0)
	seen := map[string]bool{}
	for range entries {
		klen, err := p.expect(3)
		if err != nil {
			return nil, err
		}
		key, err := p.take(klen)
		if err != nil {
			return nil, err
		}
		if seen[string(key)] {
			return nil, fmt.Errorf("duplicate field %.40q", key)
		}
		seen[string(key)] = true
		switch string(key) {
		case "roots":
			n, err := p.expect(4)
			if err != nil {
				return nil, err
			}
			if n > uint64(len(b)) {
				return nil, errors.New("too many roots")
			}
			for range n {
				if tag, err := p.expect(6); err != nil || tag != 42 {
					return nil, errors.New("root is not a CID")
				}
				blen, err := p.expect(2)
				if err != nil {
					return nil, err
				}
				raw, err := p.take(blen)
				if err != nil {
					return nil, err
				}
				if len(raw) == 0 || raw[0] != 0 {
					return nil, errors.New("root CID lacks the 0x00 prefix")
				}
				c, err := CIDFromBytes(raw[1:])
				if err != nil {
					return nil, err
				}
				roots = append(roots, c)
			}
		case "version":
			if version, err = p.expect(0); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unexpected field %.40q", key)
		}
	}
	if len(p.b) != 0 {
		return nil, errors.New("trailing bytes")
	}
	if version != 1 {
		return nil, fmt.Errorf("unsupported CAR version %d", version)
	}
	if len(roots) == 0 {
		return nil, errors.New("no roots")
	}
	return roots, nil
}

// cborParser reads the few CBOR items a CAR header uses.
type cborParser struct{ b []byte }

// expect reads an item head of the given major type and returns its argument
// (a value, length, entry count or tag number).
func (p *cborParser) expect(major byte) (uint64, error) {
	if len(p.b) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if p.b[0]>>5 != major {
		return 0, fmt.Errorf("cbor: got major type %d, want %d", p.b[0]>>5, major)
	}
	info := p.b[0] & 0x1f
	p.b = p.b[1:]
	if info < 24 {
		return uint64(info), nil
	}
	if info > 27 {
		return 0, errors.New("cbor: indefinite or reserved length")
	}
	n := 1 << (info - 24)
	if len(p.b) < n {
		return 0, io.ErrUnexpectedEOF
	}
	var v uint64
	for _, c := range p.b[:n] {
		v = v<<8 | uint64(c)
	}
	p.b = p.b[n:]
	return v, nil
}

func (p *cborParser) take(n uint64) ([]byte, error) {
	if n > uint64(len(p.b)) {
		return nil, io.ErrUnexpectedEOF
	}
	v := p.b[:n]
	p.b = p.b[n:]
	return v, nil
}
