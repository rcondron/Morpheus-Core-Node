package ipfs

import (
	"errors"
	"fmt"
	"io"
)

const (
	// DefaultChunkSize is Kubo's default leaf size (--chunker=size-262144).
	DefaultChunkSize = 256 << 10
	// DefaultMaxLinks is Kubo's default fan-out of file nodes
	// (Import.UnixFSFileMaxLinks, --max-file-links).
	DefaultMaxLinks = 174
	// MaxChunkSize keeps every leaf importable by any Kubo release: older
	// ones refuse chunks and imported blocks over 1 MiB. It is also the
	// largest chunk the IPIP-499 import profiles use.
	MaxChunkSize = 1 << 20
	// MaxBlockSize is the largest block Kubo exchanges over bitswap and
	// accepts on import (since 0.3x; 1 MiB before); readers allow up to it.
	MaxBlockSize = 2 << 20

	// linksLimit bounds Options.MaxLinks so file nodes stay far below 1 MiB.
	linksLimit = 8192
)

// Options tunes the importer. Zero fields take Kubo's defaults.
type Options struct {
	ChunkSize int // leaf size in bytes, as in --chunker=size-<ChunkSize>
	MaxLinks  int // children per file node, as in --max-file-links
}

// Importer builds the UnixFS DAG of one file as it is written, exactly as
// `ipfs add --cid-version=1 --raw-leaves --chunker=size-<ChunkSize>` does with
// Kubo's default balanced layout: raw leaves, dag-pb file nodes with up to
// Options.MaxLinks children, every leaf at the same depth, sha2-256 throughout.
//
// Each block is passed to emit as soon as it is final: children before their
// parent and the root last (CAR files may list blocks in any order). A chunk
// that repeats is emitted once per occurrence. The block slice is only valid
// during the call. Memory stays at one chunk plus, per tree level, the links
// of one unfinished node.
type Importer struct {
	maxLinks int
	emit     func(CID, []byte) error
	buf      []byte  // current leaf, cap = chunk size
	levels   [][]ref // pending children per level; levels[0] holds leaves
	size     uint64
	err      error // sticky: an emit failure or Finish ends the import
}

// ref is a finished subtree waiting for its parent.
type ref struct {
	cid   CID
	tsize uint64 // serialized bytes of the subtree (dag-pb Tsize)
	size  uint64 // file bytes under it (UnixFS blocksize)
}

var errFinished = errors.New("ipfs: importer already finished")

// NewImporter returns an importer that passes every block to emit, which may
// be nil when only the CID is needed.
func NewImporter(opts Options, emit func(c CID, block []byte) error) (*Importer, error) {
	if opts.ChunkSize == 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if opts.MaxLinks == 0 {
		opts.MaxLinks = DefaultMaxLinks
	}
	if opts.ChunkSize < 0 || opts.ChunkSize > MaxChunkSize {
		return nil, fmt.Errorf("ipfs: chunk size %d out of range (1-%d)", opts.ChunkSize, MaxChunkSize)
	}
	// With one link per node a tree never grows past one leaf (Kubo loops
	// forever on --max-file-links=1).
	if opts.MaxLinks < 2 || opts.MaxLinks > linksLimit {
		return nil, fmt.Errorf("ipfs: max links %d out of range (2-%d)", opts.MaxLinks, linksLimit)
	}
	return &Importer{maxLinks: opts.MaxLinks, emit: emit, buf: make([]byte, 0, opts.ChunkSize)}, nil
}

// Write adds file bytes; it implements io.Writer.
func (im *Importer) Write(p []byte) (int, error) {
	if im.err != nil {
		return 0, im.err
	}
	n := 0
	for n < len(p) {
		k := copy(im.buf[len(im.buf):cap(im.buf)], p[n:])
		im.buf = im.buf[:len(im.buf)+k]
		n += k
		if len(im.buf) == cap(im.buf) {
			if err := im.leaf(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// ReadFrom adds everything r yields until EOF, reading straight into the
// chunk buffer; it implements io.ReaderFrom.
func (im *Importer) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for im.err == nil {
		n, err := io.ReadFull(r, im.buf[len(im.buf):cap(im.buf)])
		im.buf = im.buf[:len(im.buf)+n]
		total += int64(n)
		if len(im.buf) == cap(im.buf) {
			if err := im.leaf(); err != nil {
				return total, err
			}
		}
		switch err {
		case nil:
		case io.EOF, io.ErrUnexpectedEOF:
			return total, nil
		default:
			return total, err
		}
	}
	return total, im.err
}

// Finish emits the last leaf and the remaining internal nodes and returns the
// root CID and the file size. An empty file is a single empty raw leaf, and a
// file of at most one chunk is just its raw leaf.
func (im *Importer) Finish() (CID, uint64, error) {
	if im.err != nil {
		return CID{}, 0, im.err
	}
	if len(im.buf) > 0 || len(im.levels) == 0 {
		if err := im.leaf(); err != nil {
			return CID{}, 0, err
		}
	}
	// Close every level bottom-up. Lower levels may hold a single child (the
	// rightmost spine of a balanced tree); the top level becomes the root once
	// it is down to one entry.
	for i := 0; ; i++ {
		if i == len(im.levels)-1 && len(im.levels[i]) == 1 {
			im.err = errFinished
			return im.levels[i][0].cid, im.size, nil
		}
		r, err := im.node(im.levels[i])
		if err != nil {
			return CID{}, 0, err
		}
		im.levels[i] = im.levels[i][:0]
		if err := im.add(i+1, r); err != nil {
			return CID{}, 0, err
		}
	}
}

func (im *Importer) out(c CID, block []byte) error {
	if im.emit == nil {
		return nil
	}
	if err := im.emit(c, block); err != nil {
		im.err = err
		return err
	}
	return nil
}

// leaf emits the buffered chunk as a raw block.
func (im *Importer) leaf() error {
	c := RawCID(im.buf)
	if err := im.out(c, im.buf); err != nil {
		return err
	}
	n := uint64(len(im.buf))
	im.size += n
	im.buf = im.buf[:0]
	return im.add(0, ref{cid: c, tsize: n, size: n})
}

// add appends r to a level. A full level is only closed when another entry
// arrives, so a file that ends exactly on a boundary gets no extra level,
// which is what Kubo's balanced builder produces.
func (im *Importer) add(level int, r ref) error {
	if level == len(im.levels) {
		im.levels = append(im.levels, make([]ref, 0, im.maxLinks))
	}
	if len(im.levels[level]) == im.maxLinks {
		parent, err := im.node(im.levels[level])
		if err != nil {
			return err
		}
		im.levels[level] = im.levels[level][:0]
		if err := im.add(level+1, parent); err != nil {
			return err
		}
	}
	im.levels[level] = append(im.levels[level], r)
	return nil
}

// node emits a UnixFS file node over children.
func (im *Importer) node(children []ref) (ref, error) {
	n := PBNode{Links: make([]PBLink, len(children))}
	d := UnixFSData{Type: UnixFSFile, Blocksizes: make([]uint64, len(children))}
	var tsize uint64
	for i, c := range children {
		n.Links[i] = PBLink{Hash: c.cid, Tsize: c.tsize}
		d.Blocksizes[i] = c.size
		d.Filesize += c.size
		tsize += c.tsize
	}
	n.Data = EncodeUnixFSData(d)
	block := EncodePBNode(n)
	c := DagPBCID(block)
	if err := im.out(c, block); err != nil {
		return ref{}, err
	}
	return ref{cid: c, tsize: tsize + uint64(len(block)), size: d.Filesize}, nil
}

// Import reads r to EOF with the default options, passes every block to emit
// (which may be nil) as Importer does, and returns the root CID and the file
// size.
func Import(r io.Reader, emit func(c CID, block []byte) error) (CID, uint64, error) {
	im, err := NewImporter(Options{}, emit)
	if err != nil {
		return CID{}, 0, err
	}
	if _, err := im.ReadFrom(r); err != nil {
		return CID{}, 0, err
	}
	return im.Finish()
}

// Verify re-imports r with the default options and fails unless it hashes to
// want, e.g. to check a download against the CID the consumer computed.
func Verify(r io.Reader, want CID) error {
	got, _, err := Import(r, nil)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("ipfs: content is %s, want %s", got, want)
	}
	return nil
}
