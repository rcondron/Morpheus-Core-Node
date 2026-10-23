package ipfs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// UnixFS node types (the Data.DataType enum).
const (
	UnixFSRaw       = 0
	UnixFSDirectory = 1
	UnixFSFile      = 2
	UnixFSMetadata  = 3
	UnixFSSymlink   = 4
	UnixFSHAMTShard = 5
)

// UnixFSData is the UnixFS message carried in the Data field of a dag-pb
// node. Fields this package has no use for (hashType, fanout, mode, mtime)
// are skipped when decoding.
type UnixFSData struct {
	Type       uint64
	Data       []byte // inline file bytes; nil when absent
	Filesize   uint64
	Blocksizes []uint64 // file bytes under each link, in link order
}

// EncodeUnixFSData serializes d as Kubo does for file nodes: Type, Data when
// set, Filesize always, then one unpacked field per blocksize.
func EncodeUnixFSData(d UnixFSData) []byte {
	b := make([]byte, 0, 16+len(d.Data)+4*len(d.Blocksizes))
	b = append(b, 1<<3|0)
	b = binary.AppendUvarint(b, d.Type)
	if d.Data != nil {
		b = appendBytesField(b, 2, d.Data)
	}
	b = append(b, 3<<3|0)
	b = binary.AppendUvarint(b, d.Filesize)
	for _, s := range d.Blocksizes {
		b = append(b, 4<<3|0)
		b = binary.AppendUvarint(b, s)
	}
	return b
}

// DecodeUnixFSData parses a UnixFS Data message. Blocksizes may be packed or
// unpacked, as protobuf decoders must accept both.
func DecodeUnixFSData(b []byte) (UnixFSData, error) {
	var d UnixFSData
	haveType := false
	for len(b) > 0 {
		f, rest, err := nextField(b)
		if err != nil {
			return UnixFSData{}, fmt.Errorf("unixfs: %w", err)
		}
		b = rest
		switch {
		case f.num == 1 && f.typ == 0:
			d.Type, haveType = f.v, true
		case f.num == 2 && f.typ == 2:
			d.Data = append([]byte{}, f.data...)
		case f.num == 3 && f.typ == 0:
			d.Filesize = f.v
		case f.num == 4 && f.typ == 0:
			d.Blocksizes = append(d.Blocksizes, f.v)
		case f.num == 4 && f.typ == 2:
			for p := f.data; len(p) > 0; {
				v, n, err := uvarint(p)
				if err != nil {
					return UnixFSData{}, fmt.Errorf("unixfs: blocksizes: %w", err)
				}
				d.Blocksizes = append(d.Blocksizes, v)
				p = p[n:]
			}
		case f.num <= 4:
			return UnixFSData{}, fmt.Errorf("unixfs: field %d has wire type %d", f.num, f.typ)
		}
	}
	if !haveType {
		return UnixFSData{}, errors.New("unixfs: missing Type")
	}
	return d, nil
}
