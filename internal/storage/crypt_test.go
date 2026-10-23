package storage

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

func TestSealRoundTrip(t *testing.T) {
	owner, _ := wallet.Generate()
	for _, size := range []int{0, 1, SegmentSize - 1, SegmentSize, SegmentSize + 1, 3*SegmentSize + 77} {
		plain := make([]byte, size)
		rand.Read(plain)
		var sealed bytes.Buffer
		if err := Seal(&sealed, bytes.NewReader(plain), Meta{Name: "report.pdf", ContentType: "application/pdf", Size: int64(size)}, owner.PublicKey()); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		hdrLen, err := HeaderLength(sealed.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if want := SealedSize(int64(size), int(hdrLen-8)); int64(sealed.Len()) != want {
			t.Fatalf("size %d: sealed %d bytes, SealedSize says %d", size, sealed.Len(), want)
		}
		if bytes.Contains(sealed.Bytes(), []byte("report.pdf")) {
			t.Fatal("the file name is visible in the container")
		}
		var out bytes.Buffer
		m, err := Unseal(&out, bytes.NewReader(sealed.Bytes()), owner)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), plain) || m.Name != "report.pdf" || m.Size != int64(size) {
			t.Fatalf("size %d: round trip mismatch", size)
		}
	}
}

func TestUnsealRejectsTampering(t *testing.T) {
	owner, _ := wallet.Generate()
	plain := bytes.Repeat([]byte("morpheus "), 3*SegmentSize/9)
	var sealed bytes.Buffer
	if err := Seal(&sealed, bytes.NewReader(plain), Meta{Name: "x", Size: int64(len(plain))}, owner.PublicKey()); err != nil {
		t.Fatal(err)
	}
	data := sealed.Bytes()
	hdr, _ := HeaderLength(data)

	flip := append([]byte(nil), data...)
	flip[hdr+100] ^= 1
	truncated := data[:len(data)-SegmentSize-tagSize] // drop the final segment
	swapped := append([]byte(nil), data[:hdr]...)
	seg := SegmentSize + tagSize
	swapped = append(swapped, data[hdr+int64(seg):hdr+2*int64(seg)]...)
	swapped = append(swapped, data[hdr:hdr+int64(seg)]...)
	swapped = append(swapped, data[hdr+2*int64(seg):]...)
	extra := append(append([]byte(nil), data...), 0)

	for name, c := range map[string][]byte{"bit flip": flip, "truncated": truncated, "reordered": swapped, "trailing": extra} {
		if _, err := Unseal(&bytes.Buffer{}, bytes.NewReader(c), owner); err == nil {
			t.Errorf("%s container was accepted", name)
		}
	}
	other, _ := wallet.Generate()
	if _, err := Unseal(&bytes.Buffer{}, bytes.NewReader(data), other); err == nil || !strings.Contains(err.Error(), "not encrypted to this wallet") {
		t.Fatalf("another wallet opened the file: %v", err)
	}
}

func TestSealChecksDeclaredSize(t *testing.T) {
	owner, _ := wallet.Generate()
	if err := Seal(&bytes.Buffer{}, bytes.NewReader(make([]byte, 10)), Meta{Size: 5}, owner.PublicKey()); err == nil {
		t.Fatal("longer input than declared was accepted")
	}
	if err := Seal(&bytes.Buffer{}, bytes.NewReader(make([]byte, 5)), Meta{Size: 10}, owner.PublicKey()); err == nil {
		t.Fatal("shorter input than declared was accepted")
	}
}
