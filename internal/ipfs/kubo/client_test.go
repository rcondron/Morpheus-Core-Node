package kubo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/ipfstest"
)

func TestNewAddresses(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:5001":            "http://127.0.0.1:5001",
		"http://127.0.0.1:5001/":           "http://127.0.0.1:5001",
		"https://kubo.example/api/v0/":     "https://kubo.example",
		"https://kubo.example/prefix":      "https://kubo.example/prefix",
		"/ip4/127.0.0.1/tcp/5001":          "http://127.0.0.1:5001",
		"/ip6/::1/tcp/5001":                "http://[::1]:5001",
		"/dns/kubo.internal/tcp/443/https": "https://kubo.internal:443",
		" /dns4/kubo/tcp/5001/http ":       "http://kubo:5001",
	} {
		c, err := New(in)
		if err != nil || c.base != want {
			t.Errorf("New(%q) = %v %v, want %s", in, c, err, want)
		}
	}
	for _, in := range []string{"", "127.0.0.1:5001", "ftp://host", "/ip4/127.0.0.1/udp/5001", "/unix/tmp/sock", "/ip4/1.2.3.4/tcp/1/ws"} {
		if _, err := New(in); err == nil {
			t.Errorf("New(%q) accepted", in)
		}
	}
}

// carPipe streams the CAR of data, announced as root, like a provider does.
func carPipe(data []byte, root ipfs.CID, opts ipfs.Options) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		_, err := ipfs.WriteCAR(pw, bytes.NewReader(data), root, opts)
		pw.CloseWithError(err)
	}()
	return pr
}

func testData(n int) []byte {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

func TestClientAgainstFake(t *testing.T) {
	ctx := context.Background()
	node := ipfstest.New()
	defer node.Close()
	kc, err := New(node.URL)
	if err != nil {
		t.Fatal(err)
	}

	if v, err := kc.Version(ctx); err != nil || v != "0.43.1" {
		t.Fatalf("version %q %v", v, err)
	}

	data := testData(5*1024 + 77)
	opts := ipfs.Options{ChunkSize: 1024}
	im, _ := ipfs.NewImporter(opts, nil)
	_, _ = im.Write(data)
	root, size, _ := im.Finish()

	car := carPipe(data, root, opts)
	roots, err := kc.DagImport(ctx, car)
	car.Close()
	if err != nil || len(roots) != 1 || roots[0] != root.String() {
		t.Fatalf("import %v %v", roots, err)
	}
	if !node.Pinned(root) {
		t.Fatal("root not pinned")
	}
	st, err := kc.RepoStat(ctx)
	if err != nil || st.NumObjects != 7 || st.RepoSize < size || st.StorageMax == 0 {
		t.Fatalf("repo stat %+v %v", st, err)
	}

	pins, err := kc.PinLs(ctx, root.String())
	if err != nil || pins[root.String()] != "recursive" || len(pins) != 1 {
		t.Fatalf("pin ls %v %v", pins, err)
	}
	if all, err := kc.PinLs(ctx, ""); err != nil || len(all) != 1 {
		t.Fatalf("pin ls all %v %v", all, err)
	}

	for _, r := range []struct{ off, n int64 }{{0, 0}, {0, 10}, {1000, 100}, {1023, 2}, {5000, 0}, {5*1024 + 77, 0}, {9999, 5}} {
		rc, err := kc.Cat(ctx, root.String(), r.off, r.n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		end := int64(len(data))
		if r.n > 0 {
			end = min(end, r.off+r.n)
		}
		want := data[min(r.off, end):end]
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("cat %d+%d: %d bytes, %v", r.off, r.n, len(got), err)
		}
	}

	get := func(ctx context.Context, c ipfs.CID) ([]byte, error) { return kc.BlockGet(ctx, c.String()) }
	proof, err := ipfs.ProvePath(ctx, get, root, 4321)
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.VerifyPath(root, 4321, size, proof); err != nil {
		t.Fatal(err)
	}

	// add as Kubo would with matching flags; only-hash stores nothing.
	before := node.Calls("add")
	res, err := kc.Add(ctx, bytes.NewReader(data), "dir/notes +1%.txt", AddOptions{CidVersion: 1, RawLeaves: true, Chunker: "size-1024", OnlyHash: true})
	if err != nil || res.Hash != root.String() || res.Name != "dir/notes +1%.txt" || node.Calls("add") != before+1 {
		t.Fatalf("add %+v %v", res, err)
	}
	other := testData(3000)[1000:]
	res, err = kc.Add(ctx, bytes.NewReader(other), "x", AddOptions{Pin: true})
	if err != nil || res.Size != uint64(len(other)) || res.Hash != ipfs.RawCID(other).String() {
		t.Fatalf("add %+v %v", res, err)
	}
	if c, _ := ipfs.Parse(res.Hash); !node.Pinned(c) {
		t.Fatal("added file not pinned")
	}

	if err := kc.PinRm(ctx, root.String()); err != nil {
		t.Fatal(err)
	}
	if pins, err := kc.PinLs(ctx, root.String()); err != nil || len(pins) != 0 {
		t.Fatalf("after rm: %v %v", pins, err)
	}
	if err := kc.PinRm(ctx, root.String()); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("second rm: %v", err)
	}
	if err := kc.PinAdd(ctx, root.String()); err != nil || !node.Pinned(root) {
		t.Fatalf("pin add: %v", err)
	}
}

func TestDagImportFailures(t *testing.T) {
	ctx := context.Background()
	node := ipfstest.New()
	defer node.Close()
	kc, _ := New(node.URL)
	data := testData(4096)
	opts := ipfs.Options{ChunkSize: 1000}

	// The consumer announced a different CID: WriteCAR aborts the stream,
	// and the upload fails with that reason.
	wrong := ipfs.RawCID([]byte("something else"))
	car := carPipe(data, wrong, opts)
	_, err := kc.DagImport(ctx, car)
	car.Close()
	if err == nil || !strings.Contains(err.Error(), wrong.String()) {
		t.Fatalf("mismatch: %v", err)
	}
	if node.Pinned(wrong) {
		t.Fatal("pinned the announced root")
	}

	// A CAR whose root's DAG is incomplete imports but does not pin.
	var b bytes.Buffer
	root := ipfs.DagPBCID([]byte("missing"))
	_ = ipfs.WriteCARHeader(&b, root)
	_ = ipfs.WriteCARBlock(&b, ipfs.RawCID([]byte("x")), []byte("x"))
	roots, err := kc.DagImport(ctx, &b)
	if err == nil || len(roots) != 0 || !strings.Contains(err.Error(), root.String()) {
		t.Fatalf("incomplete DAG: %v %v", roots, err)
	}

	// A corrupt block is rejected by the CAR reader.
	b.Reset()
	_ = ipfs.WriteCARHeader(&b, ipfs.RawCID([]byte("x")))
	_ = ipfs.WriteCARBlock(&b, ipfs.RawCID([]byte("x")), []byte("y"))
	var kerr *Error
	if _, err := kc.DagImport(ctx, &b); !errors.As(err, &kerr) || kerr.Status != http.StatusInternalServerError {
		t.Fatalf("corrupt CAR: %v", err)
	}
}

func TestStreamErrors(t *testing.T) {
	ctx := context.Background()
	node := ipfstest.New()
	defer node.Close()
	kc, _ := New(node.URL)
	data := testData(10 * 1000)
	im, _ := ipfs.NewImporter(ipfs.Options{ChunkSize: 1000}, nil)
	_, _ = im.Write(data)
	root, _, _ := im.Finish()
	car := carPipe(data, root, ipfs.Options{ChunkSize: 1000})
	if _, err := kc.DagImport(ctx, car); err != nil {
		t.Fatal(err)
	}
	car.Close()
	lost := ipfs.RawCID(data[5000:6000])
	node.DeleteBlock(lost)

	// Kubo has already sent 200 when it reaches the missing block, so the
	// error arrives in the trailer; the reader must not report a clean EOF.
	rc, err := kc.Cat(ctx, root.String(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err == nil || !strings.Contains(err.Error(), lost.String()) || !bytes.Equal(got, data[:5000]) {
		t.Fatalf("cat over a lost block: %d bytes, %v", len(got), err)
	}
	if rc, err := kc.Cat(ctx, root.String(), 6000, 0); err != nil {
		t.Fatal(err)
	} else if got, err := io.ReadAll(rc); err != nil || !bytes.Equal(got, data[6000:]) {
		t.Fatalf("cat after the lost block: %v", err)
	}
	if _, err := kc.BlockGet(ctx, lost.String()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("block get: %v", err)
	}

	node.CorruptBlock(root)
	b, err := kc.BlockGet(ctx, root.String())
	if err != nil || root.Verify(b) {
		t.Fatalf("a corrupt block is served as is: %v", err)
	}
}

func TestErrorResponses(t *testing.T) {
	ctx := context.Background()
	var handler http.HandlerFunc
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	defer srv.Close()
	kc, _ := New(srv.URL)

	handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"Message":"merkledag: not found","Code":0,"Type":"error"}`+"\n")
	}
	var kerr *Error
	if _, err := kc.Version(ctx); !errors.As(err, &kerr) || kerr.Message != "merkledag: not found" || kerr.Command != "version" || kerr.Status != 500 {
		t.Fatalf("json error: %v", err)
	}

	handler = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}
	if _, err := kc.RepoStat(ctx); err == nil || !strings.Contains(err.Error(), "404 page not found") {
		t.Fatalf("text error: %v", err)
	}

	// A stream cut short with no trailer (e.g. a proxy dropped it) is caught
	// by the advertised length.
	handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Length", "10")
		_, _ = io.WriteString(w, "12345")
	}
	rc, err := kc.Cat(ctx, "x", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short cat: %v", err)
	}
	rc.Close()

	handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Stream-Error")
		_, _ = io.WriteString(w, `{"Pins":["a"]}`+"\n")
		w.Header().Set("X-Stream-Error", "context canceled")
	}
	if err := kc.PinAdd(ctx, "a"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("trailer: %v", err)
	}

	handler = func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{not json") }
	if _, err := kc.Version(ctx); err == nil {
		t.Fatal("garbage accepted")
	}

	handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(make([]byte, ipfs.MaxBlockSize+1)) }
	if _, err := kc.BlockGet(ctx, "x"); err == nil {
		t.Fatal("oversized block accepted")
	}
}

func TestAuthorization(t *testing.T) {
	node := ipfstest.New()
	defer node.Close()
	node.Auth = "Bearer sekrit"
	kc, _ := New(node.URL)
	var kerr *Error
	if _, err := kc.Version(context.Background()); !errors.As(err, &kerr) || kerr.Status != http.StatusForbidden {
		t.Fatalf("no auth: %v", err)
	}
	kc, _ = New(node.URL, WithAuthorization("Bearer sekrit"), WithHTTPClient(&http.Client{}))
	if _, err := kc.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
}
