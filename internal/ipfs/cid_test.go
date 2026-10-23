package ipfs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const (
	emptyRaw = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku" // Kubo: empty file, CIDv1
	emptyV0  = "QmbFMke1KXqnYyBBWxB74N4c5SBnJMVAiMNRcGu6x1AwQH"              // Kubo: empty file, CIDv0
)

func TestCIDRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		s       string
		version int
		codec   uint64
	}{
		{emptyRaw, 1, CodecRaw},
		{"bafybeidwhvz53dbgegn7l7aoe3ormross63ji5weschdbojvjc2ey3jp6a", 1, CodecDagPB},
		{emptyV0, 0, CodecDagPB},
	} {
		c, err := Parse(tc.s)
		if err != nil {
			t.Fatalf("%s: %v", tc.s, err)
		}
		if c.String() != tc.s || c.Version() != tc.version || c.Codec() != tc.codec || len(c.Digest()) != 32 || !c.Defined() {
			t.Fatalf("%s: got %s v%d codec 0x%x", tc.s, c, c.Version(), c.Codec())
		}
		back, err := CIDFromBytes(c.Bytes())
		if err != nil || back != c {
			t.Fatalf("%s: binary round trip gave %s (%v)", tc.s, back, err)
		}
	}
	if RawCID(nil).String() != emptyRaw {
		t.Fatalf("RawCID(empty) = %s", RawCID(nil))
	}
	if !RawCID([]byte("x")).Verify([]byte("x")) || RawCID([]byte("x")).Verify([]byte("y")) || (CID{}).Verify(nil) {
		t.Fatal("Verify")
	}
	if DagPBCID([]byte("x")) == RawCID([]byte("x")) {
		t.Fatal("codec must be part of the identity")
	}
	if (CID{}).String() != "" || (CID{}).Defined() {
		t.Fatal("zero CID")
	}
}

func TestParseRejects(t *testing.T) {
	v1 := func(b ...byte) string { return "b" + b32.EncodeToString(append(b, make([]byte, 32)...)) }
	for name, s := range map[string]string{
		"empty":                 "",
		"base32 upper":          strings.ToUpper(emptyRaw),
		"base58 multibase":      "zb2rhe5P4gXftAwvA4eXQ5HJwsER2owDyS9sKaQRRVQPn93bA",
		"hex multibase":         "f01551220e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"short v0":              emptyV0[:45],
		"not base58":            "Qm0FMke1KXqnYyBBWxB74N4c5SBnJMVAiMNRcGu6x1AwQH",
		"trailing garbage":      emptyRaw + "a",
		"non-zero padding bits": emptyRaw[:len(emptyRaw)-1] + "v",
		"identity multihash":    "bafkqaaa",
		"dag-cbor":              v1(1, 0x71, 0x12, 0x20),
		"sha2-512":              v1(1, 0x55, 0x13, 0x20),
		"version 2":             v1(2, 0x55, 0x12, 0x20),
		"padded varint":         "b" + b32.EncodeToString(append([]byte{0x81, 0x00, 0x55, 0x12, 0x20}, make([]byte, 32)...)),
		"v0 bytes in base32":    "b" + b32.EncodeToString(append([]byte{0x12, 0x20}, make([]byte, 32)...)),
	} {
		if c, err := Parse(s); err == nil {
			t.Errorf("%s: Parse(%q) = %s, want error", name, s, c)
		}
	}
	if _, err := CIDFromBytes(append(RawCID(nil).Bytes(), 0)); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := CIDFromBytes(RawCID(nil).Bytes()[:35]); err == nil {
		t.Error("truncated CID accepted")
	}
}

func TestCIDText(t *testing.T) {
	type doc struct {
		Root CID `json:"root"`
		None CID `json:"none"`
	}
	c, _ := Parse(emptyRaw)
	b, err := json.Marshal(doc{Root: c})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"root":"`+emptyRaw+`","none":""}` {
		t.Fatalf("json %s", b)
	}
	var back doc
	if err := json.Unmarshal(b, &back); err != nil || back.Root != c || back.None.Defined() {
		t.Fatalf("unmarshal %+v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"root":"nope"}`), &back); err == nil {
		t.Fatal("invalid CID accepted")
	}
}

func TestBase58(t *testing.T) {
	for _, b := range [][]byte{{}, {0}, {0, 0, 1}, {0xff}, bytes.Repeat([]byte{0xab}, 40)} {
		s := base58Encode(b)
		back, err := base58Decode(s)
		if err != nil || !bytes.Equal(back, b) {
			t.Fatalf("%x -> %q -> %x (%v)", b, s, back, err)
		}
	}
	if base58Encode([]byte{0, 0, 0x01, 0x02}) != "115T" {
		t.Fatalf("got %q", base58Encode([]byte{0, 0, 0x01, 0x02}))
	}
}
