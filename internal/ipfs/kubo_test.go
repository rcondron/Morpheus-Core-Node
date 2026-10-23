package ipfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The tests in this file run a real Kubo binary and are skipped unless
// MORCORE_KUBO_BIN names one (e.g. the `ipfs` from a Kubo release tarball).
// Verified against Kubo v0.43.1 (kubo_v0.43.1_linux-amd64.tar.gz, published
// sha512 checked).

// kuboCLI returns a function running the Kubo CLI on a fresh offline repo.
func kuboCLI(t *testing.T) func(stdin io.Reader, args ...string) []byte {
	t.Helper()
	bin := os.Getenv("MORCORE_KUBO_BIN")
	if bin == "" {
		t.Skip("MORCORE_KUBO_BIN not set")
	}
	env := append(os.Environ(), "IPFS_PATH="+t.TempDir())
	run := func(stdin io.Reader, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Stdin = stdin
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("ipfs %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}
		return out
	}
	run(nil, "init", "--profile=test")
	return run
}

func TestKuboCompat(t *testing.T) {
	ipfs := kuboCLI(t)
	t.Logf("Kubo %s", bytes.TrimSpace(ipfs(nil, "version", "--number")))
	for _, v := range kuboVectors {
		args := []string{"add", "-Q", "--only-hash", "--cid-version=1", "--raw-leaves=true", fmt.Sprintf("--chunker=size-%d", v.chunk)}
		if v.links != 0 {
			args = append(args, fmt.Sprintf("--max-file-links=%d", v.links))
		}
		kubo := string(bytes.TrimSpace(ipfs(newTestReader(v.size), args...)))
		im, err := NewImporter(Options{ChunkSize: v.chunk, MaxLinks: v.links}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := im.ReadFrom(newTestReader(v.size)); err != nil {
			t.Fatal(err)
		}
		ours, _, err := im.Finish()
		if err != nil {
			t.Fatal(err)
		}
		if kubo != ours.String() || kubo != v.cid {
			t.Errorf("size %d chunk %d links %d: Kubo %s, ours %s, golden %s", v.size, v.chunk, v.links, kubo, ours, v.cid)
		}
	}
}

// TestKuboDAGBytes checks block-level agreement: Kubo's exported CAR has our
// exact header bytes and exactly our blocks, which decode and re-encode
// byte for byte, and proofs built from Kubo's blocks verify.
func TestKuboDAGBytes(t *testing.T) {
	ipfs := kuboCLI(t)
	// 1024-byte chunks over 300 KiB: a depth-2 tree whose last subtree has
	// fewer children than the others.
	data := testData(t, 300<<10+5)
	opts := Options{ChunkSize: 1024}
	root, size, ours := importAll(t, data, opts)
	added := string(bytes.TrimSpace(ipfs(bytes.NewReader(data), "add", "-Q", "--cid-version=1", "--raw-leaves=true", "--chunker=size-1024")))
	if added != root.String() {
		t.Fatalf("Kubo added %s, we computed %s", added, root)
	}

	car := ipfs(nil, "dag", "export", root.String())
	var hdr bytes.Buffer
	if err := WriteCARHeader(&hdr, root); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(car, hdr.Bytes()) {
		t.Fatalf("CAR header differs:\nKubo %x\nours %x", car[:hdr.Len()], hdr.Bytes())
	}
	cr, err := NewCARReader(bytes.NewReader(car))
	if err != nil {
		t.Fatal(err)
	}
	kubo := blockMap{}
	for {
		c, b, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kubo[c] = b
		if c.Codec() != CodecDagPB {
			continue
		}
		n, err := DecodePBNode(b)
		if err != nil {
			t.Fatal(err)
		}
		d, err := DecodeUnixFSData(n.Data)
		if err != nil {
			t.Fatal(err)
		}
		n.Data = EncodeUnixFSData(d)
		if !bytes.Equal(EncodePBNode(n), b) {
			t.Fatalf("re-encoding %s changes its bytes", c)
		}
	}
	if len(kubo) != len(ours) {
		t.Fatalf("Kubo exported %d blocks, we built %d", len(kubo), len(ours))
	}
	for c, b := range ours {
		if !bytes.Equal(kubo[c], b) {
			t.Fatalf("block %s differs", c)
		}
	}
	for _, off := range []uint64{0, 1023, 1024, 174 * 1024, size - 1} {
		proof, err := ProvePath(context.Background(), kubo.get, root, off)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyPath(root, off, size, proof); err != nil {
			t.Fatalf("offset %d: %v", off, err)
		}
	}
}
