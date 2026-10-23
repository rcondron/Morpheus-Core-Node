package ipfs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PBNode is a dag-pb node: a list of links plus opaque data (UnixFS metadata
// for file nodes).
type PBNode struct {
	Links []PBLink
	Data  []byte // nil when the field is absent
}

// PBLink is a dag-pb link. Kubo always serializes Name and Tsize, Name being
// empty for file chunks, so the encoder always writes both; the decoder
// accepts their absence (reading them as zero values).
type PBLink struct {
	Hash  CID
	Name  string
	Tsize uint64 // cumulative serialized size of the linked DAG
}

// EncodePBNode serializes n in canonical dag-pb form: links (field 2) before
// data (field 1), which is what Kubo writes by default.
func EncodePBNode(n PBNode) []byte {
	var b, link []byte
	for _, l := range n.Links {
		link = appendBytesField(link[:0], 1, l.Hash.Bytes())
		link = appendBytesField(link, 2, []byte(l.Name))
		link = binary.AppendUvarint(append(link, 3<<3|0), l.Tsize)
		b = appendBytesField(b, 2, link)
	}
	if n.Data != nil {
		b = appendBytesField(b, 1, n.Data)
	}
	return b
}

func appendBytesField(b []byte, field byte, v []byte) []byte {
	b = append(b, field<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// DecodePBNode parses a dag-pb block with the same strictness as Kubo's
// decoder: only the Data and Links fields, links contiguous, Data at most once
// (before or after the links), and Hash, Name, Tsize in that order in a link.
func DecodePBNode(b []byte) (PBNode, error) {
	var n PBNode
	haveData, inLinks, linksDone := false, false, false
	for len(b) > 0 {
		f, rest, err := nextField(b)
		if err != nil {
			return PBNode{}, fmt.Errorf("dag-pb: %w", err)
		}
		b = rest
		if f.typ != 2 {
			return PBNode{}, fmt.Errorf("dag-pb: field %d has wire type %d, want 2", f.num, f.typ)
		}
		switch f.num {
		case 1:
			if haveData {
				return PBNode{}, errors.New("dag-pb: duplicate Data field")
			}
			haveData = true
			if inLinks {
				inLinks, linksDone = false, true
			}
			n.Data = append([]byte{}, f.data...)
		case 2:
			if linksDone {
				return PBNode{}, errors.New("dag-pb: links are not contiguous")
			}
			inLinks = true
			l, err := decodePBLink(f.data)
			if err != nil {
				return PBNode{}, err
			}
			n.Links = append(n.Links, l)
		default:
			return PBNode{}, fmt.Errorf("dag-pb: unexpected field %d", f.num)
		}
	}
	return n, nil
}

func decodePBLink(b []byte) (PBLink, error) {
	var l PBLink
	last := uint64(0) // highest field seen; fields must strictly increase
	for len(b) > 0 {
		f, rest, err := nextField(b)
		if err != nil {
			return PBLink{}, fmt.Errorf("dag-pb link: %w", err)
		}
		b = rest
		if f.num <= last {
			return PBLink{}, fmt.Errorf("dag-pb link: field %d out of order or duplicated", f.num)
		}
		last = f.num
		switch {
		case f.num == 1 && f.typ == 2:
			if l.Hash, err = CIDFromBytes(f.data); err != nil {
				return PBLink{}, fmt.Errorf("dag-pb link: %w", err)
			}
		case f.num == 2 && f.typ == 2:
			l.Name = string(f.data)
		case f.num == 3 && f.typ == 0:
			l.Tsize = f.v
		default:
			return PBLink{}, fmt.Errorf("dag-pb link: unexpected field %d (wire type %d)", f.num, f.typ)
		}
	}
	if !l.Hash.Defined() {
		return PBLink{}, errors.New("dag-pb link: missing Hash")
	}
	return l, nil
}

// pbField is one protobuf field: a varint (wire type 0) or bytes (type 2).
type pbField struct {
	num  uint64
	typ  uint64
	v    uint64
	data []byte
}

// nextField decodes the protobuf field at the start of b. Fixed-width fields
// (wire types 1 and 5) are skipped over so unknown fields can be ignored.
func nextField(b []byte) (pbField, []byte, error) {
	key, n, err := uvarint(b)
	if err != nil {
		return pbField{}, nil, err
	}
	b = b[n:]
	f := pbField{num: key >> 3, typ: key & 7}
	if f.num == 0 {
		return pbField{}, nil, errors.New("invalid field number 0")
	}
	switch f.typ {
	case 0:
		if f.v, n, err = uvarint(b); err != nil {
			return pbField{}, nil, err
		}
		b = b[n:]
	case 2:
		size, n, err := uvarint(b)
		if err != nil {
			return pbField{}, nil, err
		}
		b = b[n:]
		if size > uint64(len(b)) {
			return pbField{}, nil, errors.New("truncated field")
		}
		f.data, b = b[:size], b[size:]
	case 1, 5:
		width := 8
		if f.typ == 5 {
			width = 4
		}
		if len(b) < width {
			return pbField{}, nil, errors.New("truncated field")
		}
		b = b[width:]
	default:
		return pbField{}, nil, fmt.Errorf("unsupported wire type %d", f.typ)
	}
	return f, b, nil
}
