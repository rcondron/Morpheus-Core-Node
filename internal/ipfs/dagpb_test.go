package ipfs

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// kuboRoot3000 is the root block `ipfs block get` returned for 3000 zero
// bytes added with --cid-version=1 --raw-leaves --chunker=size-1024 (Kubo
// v0.43.1): links (Hash, empty Name, Tsize) before the UnixFS data.
const kuboRoot3000 = "" +
	"122b0a2401551220" + "5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef" + "1200" + "188008" +
	"122b0a2401551220" + "5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef" + "1200" + "188008" +
	"122b0a2401551220" + "f438a4e713df6a982afbe2eec993cd582edc37a876fee88e1ddabb478f2b5ee0" + "1200" + "18b807" +
	"0a0e" + "0802" + "18b817" + "208008" + "208008" + "20b807"

func TestKuboRootBlock(t *testing.T) {
	want, _ := hex.DecodeString(kuboRoot3000)
	root, _, blocks := importAll(t, make([]byte, 3000), Options{ChunkSize: 1024})
	if root.String() != "bafybeidwhvz53dbgegn7l7aoe3ormross63ji5weschdbojvjc2ey3jp6a" {
		t.Fatalf("root %s", root)
	}
	if !bytes.Equal(blocks[root], want) {
		t.Fatalf("root block\n got %x\nwant %x", blocks[root], want)
	}

	n, err := DecodePBNode(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Links) != 3 || n.Links[0].Name != "" || n.Links[0].Tsize != 1024 || n.Links[2].Tsize != 952 {
		t.Fatalf("links %+v", n.Links)
	}
	d, err := DecodeUnixFSData(n.Data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != UnixFSFile || d.Filesize != 3000 || len(d.Blocksizes) != 3 || d.Blocksizes[2] != 952 || d.Data != nil {
		t.Fatalf("unixfs %+v", d)
	}
	if !bytes.Equal(EncodeUnixFSData(d), n.Data) || !bytes.Equal(EncodePBNode(n), want) {
		t.Fatal("re-encoding changes the block")
	}
}

func TestDecodePBNodeForms(t *testing.T) {
	leaf := RawCID([]byte("leaf"))
	links := PBNode{Links: []PBLink{{Hash: leaf, Name: "a", Tsize: 4}, {Hash: leaf, Tsize: 4}}, Data: []byte{8, 2}}
	canonical := EncodePBNode(links)

	// Kubo's Import.UnixFSPBNodeFieldOrder=data-first (IPIP-550) writes Data
	// first; readers must accept it.
	dataFirst := append([]byte{0x0a, 2, 8, 2}, canonical[:len(canonical)-4]...)
	n, err := DecodePBNode(dataFirst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(EncodePBNode(n), canonical) {
		t.Fatal("data-first node decodes differently")
	}

	// Absent Name and Tsize, and absent vs empty Data.
	bare := []byte{0x12, 0x26, 0x0a, 0x24}
	bare = append(bare, leaf.Bytes()...)
	n, err = DecodePBNode(bare)
	if err != nil || len(n.Links) != 1 || n.Links[0].Hash != leaf || n.Data != nil {
		t.Fatalf("%+v %v", n, err)
	}
	if n, err = DecodePBNode([]byte{0x0a, 0}); err != nil || n.Data == nil || len(n.Data) != 0 {
		t.Fatalf("empty data: %+v %v", n, err)
	}
	if !bytes.Equal(EncodePBNode(PBNode{Data: []byte{}}), []byte{0x0a, 0}) || len(EncodePBNode(PBNode{})) != 0 {
		t.Fatal("Data presence not preserved")
	}

	link := hex.EncodeToString(append([]byte{0x0a, 0x24}, leaf.Bytes()...))
	for name, h := range map[string]string{
		"duplicate data":     "0a0108" + "0a0108",
		"split links":        "1226" + link + "0a0108" + "1226" + link,
		"unknown field":      "1a00",
		"varint field":       "0801",
		"missing hash":       "12021200",
		"name before hash":   "122812000a24" + link[4:],
		"duplicate tsize":    "122a" + link + "18011801",
		"bad cid":            "12040a020102",
		"truncated":          "1226" + link[:20],
		"tsize wire type":    "1229" + link + "1a0100",
		"link unknown field": "1228" + link + "2001",
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := DecodePBNode(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeUnixFSData(t *testing.T) {
	// Packed blocksizes, plus mode (7) and mtime (8) which we skip.
	b, _ := hex.DecodeString("0802" + "180a" + "2202" + "0307" + "38a403" + "42020801")
	d, err := DecodeUnixFSData(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != UnixFSFile || d.Filesize != 10 || len(d.Blocksizes) != 2 || d.Blocksizes[1] != 7 {
		t.Fatalf("%+v", d)
	}
	for _, h := range []string{"", "1801", "0a00", "08"} {
		b, _ := hex.DecodeString(h)
		if _, err := DecodeUnixFSData(b); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
	// Inline data survives a round trip.
	in := UnixFSData{Type: UnixFSRaw, Data: []byte("hi"), Filesize: 2}
	out, err := DecodeUnixFSData(EncodeUnixFSData(in))
	if err != nil || string(out.Data) != "hi" || out.Filesize != 2 || out.Type != UnixFSRaw {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.HasPrefix(hex.EncodeToString(EncodeUnixFSData(in)), "08001202") {
		t.Fatal("field order")
	}
}
