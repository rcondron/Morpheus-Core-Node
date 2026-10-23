package kubo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
)

// TestKuboDaemon runs the client against a real `ipfs daemon --offline` on a
// throwaway repo. It is skipped unless MORCORE_KUBO_BIN names a Kubo binary.
// Verified against Kubo v0.43.1 (kubo_v0.43.1_linux-amd64.tar.gz, published
// sha512 checked).
func TestKuboDaemon(t *testing.T) {
	bin := os.Getenv("MORCORE_KUBO_BIN")
	if bin == "" {
		t.Skip("MORCORE_KUBO_BIN not set")
	}
	repo := t.TempDir()
	env := append(os.Environ(), "IPFS_PATH="+repo)
	ipfsCLI := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ipfs %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	ipfsCLI("init", "--profile=test")
	// Port 0 everywhere: the daemon writes the API address it got to
	// $IPFS_PATH/api. No gateway; RPC requires a bearer token.
	ipfsCLI("config", "Addresses.API", "/ip4/127.0.0.1/tcp/0")
	ipfsCLI("config", "--json", "Addresses.Gateway", "[]")
	ipfsCLI("config", "--json", "API.Authorizations", `{"morcore":{"AuthSecret":"bearer:sekrit","AllowedPaths":["/api/v0"]}}`)

	logf, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(bin, "daemon", "--offline")
	daemon.Env, daemon.Stdout, daemon.Stderr = env, logf, logf
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = daemon.Process.Kill()
			<-exited
		}
	})
	daemonLog := func() string { b, _ := os.ReadFile(logf.Name()); return string(b) }

	var api []byte
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if api, err = os.ReadFile(filepath.Join(repo, "api")); err == nil && len(api) > 0 {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("daemon exited: %v\n%s", err, daemonLog())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start\n%s", daemonLog())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var kerr *Error
	anon, _ := New(string(api))
	if _, err := anon.Version(ctx); !errors.As(err, &kerr) || kerr.Status != http.StatusForbidden {
		t.Fatalf("unauthenticated call: %v", err)
	}
	kc, err := New(string(api), WithAuthorization("Bearer sekrit"))
	if err != nil {
		t.Fatal(err)
	}
	version, err := kc.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Kubo %s at %s", version, api)
	if st, err := kc.RepoStat(ctx); err != nil || st.StorageMax == 0 || st.RepoPath == "" {
		t.Fatalf("repo stat %+v %v", st, err)
	}

	type file struct {
		data []byte
		opts ipfs.Options
		root ipfs.CID
		size uint64
	}
	files := []*file{
		{data: testData(3<<20 + 12345)},                                              // default settings, depth 1
		{data: testData(50 << 10), opts: ipfs.Options{ChunkSize: 1024, MaxLinks: 4}}, // depth 3
	}
	for _, f := range files {
		im, _ := ipfs.NewImporter(f.opts, nil)
		_, _ = im.Write(f.data)
		f.root, f.size, _ = im.Finish()

		pr, pw := io.Pipe()
		go func() {
			_, err := ipfs.WriteCAR(pw, bytes.NewReader(f.data), f.root, f.opts)
			pw.CloseWithError(err)
		}()
		roots, err := kc.DagImport(ctx, pr)
		pr.Close()
		if err != nil || len(roots) != 1 || roots[0] != f.root.String() {
			t.Fatalf("dag import: %v %v", roots, err)
		}
		if pins, err := kc.PinLs(ctx, f.root.String()); err != nil || pins[f.root.String()] != "recursive" {
			t.Fatalf("pin ls %v %v", pins, err)
		}

		n := int64(len(f.data))
		for _, r := range [][2]int64{{0, 0}, {0, 1}, {1023, 1025}, {n / 3, 70000}, {n - 1, 0}, {n, 0}, {n + 100, 10}} {
			rc, err := kc.Cat(ctx, f.root.String(), r[0], r[1])
			if err != nil {
				t.Fatalf("cat %v: %v", r, err)
			}
			got, err := io.ReadAll(rc)
			rc.Close()
			end := n
			if r[1] > 0 {
				end = min(n, r[0]+r[1])
			}
			if err != nil || !bytes.Equal(got, f.data[min(r[0], end):end]) {
				t.Fatalf("cat %v: %d bytes, %v", r, len(got), err)
			}
		}

		block, err := kc.BlockGet(ctx, f.root.String())
		if err != nil || !f.root.Verify(block) {
			t.Fatalf("block get: %v", err)
		}
		get := func(ctx context.Context, c ipfs.CID) ([]byte, error) { return kc.BlockGet(ctx, c.String()) }
		for _, off := range []uint64{0, 1024, f.size / 2, f.size - 1} {
			proof, err := ipfs.ProvePath(ctx, get, f.root, off)
			if err != nil {
				t.Fatal(err)
			}
			if err := ipfs.VerifyPath(f.root, off, f.size, proof); err != nil {
				t.Fatalf("offset %d: %v", off, err)
			}
		}
	}
	a, b := files[0], files[1]
	if all, err := kc.PinLs(ctx, ""); err != nil || all[a.root.String()] == "" || all[b.root.String()] == "" {
		t.Fatalf("pin ls all: %v %v", all, err)
	}

	// Kubo's add with the matching flags agrees on the CID and DAG size, and
	// takes the escaped file name back apart; the slash makes Kubo also report
	// a wrapping directory, which Add skips.
	var dagSize uint64
	im, _ := ipfs.NewImporter(ipfs.Options{}, func(_ ipfs.CID, blk []byte) error { dagSize += uint64(len(blk)); return nil })
	_, _ = im.Write(a.data)
	_, _, _ = im.Finish()
	res, err := kc.Add(ctx, bytes.NewReader(a.data), "dir/notes +1%.txt", AddOptions{CidVersion: 1, RawLeaves: true, Chunker: "size-262144", OnlyHash: true})
	if err != nil || res.Hash != a.root.String() || res.Size != dagSize || res.Name != "dir/notes +1%.txt" {
		t.Fatalf("add %+v %v (want %s, %d bytes)", res, err, a.root, dagSize)
	}
	extra := testData(1000)
	res, err = kc.Add(ctx, bytes.NewReader(extra), "extra", AddOptions{CidVersion: 1, Pin: true})
	if err != nil || res.Hash != ipfs.RawCID(extra).String() {
		t.Fatalf("add %+v %v", res, err)
	}
	if pins, _ := kc.PinLs(ctx, res.Hash); len(pins) != 1 {
		t.Fatal("added file not pinned")
	}

	// Kubo advertises cat lengths, which Cat checks when trailers are lost.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, kc.base+"/api/v0/cat?arg="+a.root.String()+"&offset=10&length=100", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.Header.Get("X-Content-Length") != "100" {
		t.Fatalf("X-Content-Length: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	if err := kc.PinRm(ctx, a.root.String()); err != nil {
		t.Fatal(err)
	}
	if pins, err := kc.PinLs(ctx, a.root.String()); err != nil || len(pins) != 0 {
		t.Fatalf("after rm: %v %v", pins, err)
	}
	if err := kc.PinRm(ctx, a.root.String()); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("second rm: %v", err)
	}
	if err := kc.PinAdd(ctx, a.root.String()); err != nil {
		t.Fatal(err)
	}

	// Content that does not match the announced CID never gets pinned.
	wrong := ipfs.RawCID([]byte("announced"))
	pr, pw := io.Pipe()
	go func() {
		_, err := ipfs.WriteCAR(pw, bytes.NewReader(extra[1:]), wrong, ipfs.Options{})
		pw.CloseWithError(err)
	}()
	if _, err := kc.DagImport(ctx, pr); err == nil || !strings.Contains(err.Error(), wrong.String()) {
		t.Fatalf("mismatched import: %v", err)
	}
	pr.Close()
	if pins, err := kc.PinLs(ctx, wrong.String()); err != nil || len(pins) != 0 {
		t.Fatalf("mismatched root: %v %v", pins, err)
	}
	var car bytes.Buffer
	missing := ipfs.DagPBCID([]byte("not in the CAR"))
	_ = ipfs.WriteCARHeader(&car, missing)
	_ = ipfs.WriteCARBlock(&car, ipfs.RawCID(extra), extra)
	roots, err := kc.DagImport(ctx, &car)
	if err == nil || len(roots) != 0 || !strings.Contains(err.Error(), missing.String()) {
		t.Fatalf("incomplete CAR: %v %v", roots, err)
	}
	t.Logf("incomplete CAR: %v", err)

	// Lose a block from file b: reads through it fail mid-stream (the error
	// arrives as a trailer), audits of it fail, other leaves still prove.
	if err := kc.PinRm(ctx, b.root.String()); err != nil {
		t.Fatal(err)
	}
	lost := ipfs.RawCID(b.data[20*1024 : 21*1024])
	ipfsCLI("--api-auth=bearer:sekrit", "block", "rm", lost.String())
	rc, err := kc.Cat(ctx, b.root.String(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err == nil || !bytes.Equal(got, b.data[:20*1024]) {
		t.Fatalf("cat over a lost block: %d bytes, %v", len(got), err)
	}
	t.Logf("cat over a lost block: %v", err)
	if _, err := kc.BlockGet(ctx, lost.String()); err == nil {
		t.Fatal("block get of a lost block succeeded")
	}
	get := func(ctx context.Context, c ipfs.CID) ([]byte, error) { return kc.BlockGet(ctx, c.String()) }
	if _, err := ipfs.ProvePath(ctx, get, b.root, 20*1024+5); err == nil {
		t.Fatal("proved a lost block")
	}
	if _, err := ipfs.ProvePath(ctx, get, b.root, 0); err != nil {
		t.Fatal(err)
	}
	if err := kc.PinAdd(ctx, b.root.String()); err == nil {
		t.Fatal("pinned an incomplete DAG offline")
	}
	if st, err := kc.RepoStat(ctx); err != nil || st.NumObjects == 0 || st.RepoSize == 0 {
		t.Fatalf("repo stat %+v %v", st, err)
	}
}
