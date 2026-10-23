package ipfs

import (
	"context"
	"errors"
	"fmt"
)

// maxDepth bounds proof paths. 174-way trees of 256 KiB chunks reach
// petabytes by depth 5; even 2-way trees of 1-byte chunks stay far below.
const maxDepth = 64

// ProvePath returns the blocks on the path from root down to the leaf that
// holds byte offset of the file: the provider's answer to a storage audit.
// For a file of one chunk that is just the raw leaf. get fetches a block
// (e.g. Kubo's block/get); every block is checked against its CID, so a
// provider learns of corrupt data before it hands out a proof that will fail.
func ProvePath(ctx context.Context, get func(context.Context, CID) ([]byte, error), root CID, offset uint64) ([][]byte, error) {
	c, off := root, offset
	var size uint64
	var path [][]byte
	for depth := 0; depth < maxDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block, err := get(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", c, err)
		}
		if !c.Verify(block) {
			return nil, fmt.Errorf("block %s is corrupt", c)
		}
		if depth == 0 {
			if size, err = dagSize(c, block); err != nil {
				return nil, err
			}
			if off >= size {
				return nil, fmt.Errorf("offset %d is beyond the %d-byte file", off, size)
			}
		}
		path = append(path, block)
		next, nextOff, nextSize, err := descend(c, block, off, size)
		if err != nil {
			return nil, err
		}
		if !next.Defined() {
			return path, nil
		}
		c, off, size = next, nextOff, nextSize
	}
	return nil, errors.New("file DAG is too deep")
}

// VerifyPath checks a ProvePath answer against what the consumer knows: the
// root CID and size of its file. Each block must hash to the CID its parent
// links to (the root first), every node must be a UnixFS file node whose
// sizes add up, the child taken must be the one covering offset, and the
// path must end in a raw leaf holding that byte, with nothing after it.
func VerifyPath(root CID, offset, fileSize uint64, blocks [][]byte) error {
	if offset >= fileSize {
		return fmt.Errorf("offset %d is beyond the %d-byte file", offset, fileSize)
	}
	if len(blocks) > maxDepth {
		return fmt.Errorf("proof has %d blocks, more than any real file", len(blocks))
	}
	c, off, size := root, offset, fileSize
	for i, block := range blocks {
		if !c.Verify(block) {
			return fmt.Errorf("proof block %d does not hash to %s", i, c)
		}
		next, nextOff, nextSize, err := descend(c, block, off, size)
		if err != nil {
			return fmt.Errorf("proof block %d: %w", i, err)
		}
		if !next.Defined() {
			if i != len(blocks)-1 {
				return fmt.Errorf("proof has %d blocks after the leaf", len(blocks)-1-i)
			}
			return nil
		}
		c, off, size = next, nextOff, nextSize
	}
	return errors.New("proof ends before reaching a leaf")
}

// dagSize returns the file bytes under a verified block.
func dagSize(c CID, block []byte) (uint64, error) {
	if c.Codec() == CodecRaw {
		return uint64(len(block)), nil
	}
	_, d, err := fileNode(block)
	if err != nil {
		return 0, err
	}
	return d.Filesize, nil
}

// descend checks that a verified block is a file (sub)DAG of size bytes and
// returns the child covering byte off, with the offset and size within that
// child. At a raw leaf it returns the undefined CID.
func descend(c CID, block []byte, off, size uint64) (CID, uint64, uint64, error) {
	if c.Codec() == CodecRaw {
		if uint64(len(block)) != size {
			return CID{}, 0, 0, fmt.Errorf("leaf holds %d bytes, its parent says %d", len(block), size)
		}
		return CID{}, 0, 0, nil
	}
	n, d, err := fileNode(block)
	if err != nil {
		return CID{}, 0, 0, err
	}
	if d.Filesize != size {
		return CID{}, 0, 0, fmt.Errorf("node covers %d bytes, expected %d", d.Filesize, size)
	}
	var start, sum uint64
	idx := -1
	for i, bs := range d.Blocksizes {
		if sum+bs < sum {
			return CID{}, 0, 0, errors.New("blocksizes overflow")
		}
		if idx < 0 && off < sum+bs {
			idx, start = i, sum
		}
		sum += bs
	}
	if sum != d.Filesize || idx < 0 {
		return CID{}, 0, 0, fmt.Errorf("blocksizes add up to %d, filesize is %d", sum, d.Filesize)
	}
	return n.Links[idx].Hash, off - start, d.Blocksizes[idx], nil
}

// fileNode decodes a dag-pb UnixFS file node in the shape the importer
// builds: children only, no inline data, one blocksize per link.
func fileNode(block []byte) (PBNode, UnixFSData, error) {
	n, err := DecodePBNode(block)
	if err != nil {
		return PBNode{}, UnixFSData{}, err
	}
	if n.Data == nil {
		return PBNode{}, UnixFSData{}, errors.New("dag-pb node has no UnixFS data")
	}
	d, err := DecodeUnixFSData(n.Data)
	if err != nil {
		return PBNode{}, UnixFSData{}, err
	}
	switch {
	case d.Type != UnixFSFile:
		return PBNode{}, UnixFSData{}, fmt.Errorf("UnixFS node type %d is not a file", d.Type)
	case len(d.Data) > 0:
		return PBNode{}, UnixFSData{}, errors.New("file node carries inline data")
	case len(n.Links) == 0 || len(n.Links) != len(d.Blocksizes):
		return PBNode{}, UnixFSData{}, fmt.Errorf("file node has %d links and %d blocksizes", len(n.Links), len(d.Blocksizes))
	}
	return n, d, nil
}
