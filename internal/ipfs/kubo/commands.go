package kubo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
)

// Version returns the node's Kubo version, e.g. "0.43.1".
func (c *Client) Version(ctx context.Context) (string, error) {
	var version string
	err := call(ctx, c, "version", nil, nil, "", func(v struct{ Version string }) error {
		version = v.Version
		return nil
	})
	return version, err
}

// RepoStat is the node's storage usage.
type RepoStat struct {
	RepoSize   uint64 // bytes stored
	StorageMax uint64 // Datastore.StorageMax: the GC watermark, not a hard quota
	NumObjects uint64
	RepoPath   string
	Version    string
}

// RepoStat reports storage usage (Kubo walks the blockstore to count objects).
func (c *Client) RepoStat(ctx context.Context) (RepoStat, error) {
	var st RepoStat
	err := call(ctx, c, "repo/stat", nil, nil, "", func(v RepoStat) error {
		st = v
		return nil
	})
	return st, err
}

// DagImport streams a CAR v1 file into the node and pins its roots, returning
// them. Kubo pins a root only when the CAR plus its blockstore hold the root's
// entire DAG, and never fetches from the network during an import; a root
// that failed to pin is an error carrying Kubo's reason.
func (c *Client) DagImport(ctx context.Context, car io.Reader) ([]string, error) {
	var roots []string
	var pinErr error
	err := upload(ctx, c, "dag/import", url.Values{"pin-roots": {"true"}}, "import.car", car, func(v struct {
		Root *struct {
			Cid struct {
				Link string `json:"/"`
			}
			PinErrorMsg string
		}
	}) error {
		if v.Root == nil { // e.g. a Stats line
			return nil
		}
		if v.Root.PinErrorMsg != "" {
			pinErr = errors.Join(pinErr, &Error{Command: "dag/import", Status: http.StatusOK,
				Message: fmt.Sprintf("pin root %s: %s", v.Root.Cid.Link, v.Root.PinErrorMsg)})
			return nil
		}
		roots = append(roots, v.Root.Cid.Link)
		return nil
	})
	if err != nil {
		return roots, err
	}
	return roots, pinErr
}

// AddOptions are `ipfs add` flags. Zero values keep the node's configured
// defaults, except Pin, which is always sent.
type AddOptions struct {
	CidVersion int    // 1 for CIDv1 (implies raw leaves in Kubo); 0 keeps the default
	RawLeaves  bool   // force raw leaves
	Chunker    string // e.g. "size-262144"
	Pin        bool
	OnlyHash   bool // compute the CID without storing anything
}

// AddResult describes an added file.
type AddResult struct {
	Name string
	Hash string
	Size uint64 // cumulative size of the file's DAG in bytes
}

// Add imports r as a file named name. Its CID depends on the node's import
// settings unless opts pins them down; storage providers should prefer
// ipfs.WriteCAR with DagImport, which leaves Kubo no say in the CID. A name
// with slashes makes Kubo wrap the file in directories; the result is still
// the file.
func (c *Client) Add(ctx context.Context, r io.Reader, name string, opts AddOptions) (AddResult, error) {
	q := url.Values{"pin": {strconv.FormatBool(opts.Pin)}}
	if opts.CidVersion != 0 {
		q.Set("cid-version", strconv.Itoa(opts.CidVersion))
	}
	if opts.RawLeaves {
		q.Set("raw-leaves", "true")
	}
	if opts.Chunker != "" {
		q.Set("chunker", opts.Chunker)
	}
	if opts.OnlyHash {
		q.Set("only-hash", "true")
	}
	var res AddResult
	err := upload(ctx, c, "add", q, name, r, func(v struct{ Name, Hash, Size string }) error {
		// Skip progress lines, and directories Kubo reports after the file.
		if v.Hash == "" || res.Hash != "" {
			return nil
		}
		size, err := strconv.ParseUint(v.Size, 10, 64)
		if err != nil {
			return fmt.Errorf("kubo add: bad size %q", v.Size)
		}
		res = AddResult{Name: v.Name, Hash: v.Hash, Size: size}
		return nil
	})
	if err == nil && res.Hash == "" {
		err = errors.New("kubo add: no result")
	}
	return res, err
}

// Cat streams the file at cid from byte offset, at most length bytes
// (length <= 0 reads to the end). It reads local blocks only (offline=true):
// a provider must never fetch from the network what it claims to store. The
// reader fails, instead of ending early, if Kubo hits a missing block.
func (c *Client) Cat(ctx context.Context, cid string, offset, length int64) (io.ReadCloser, error) {
	q := url.Values{"arg": {cid}, "offline": {"true"}}
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	if length > 0 {
		q.Set("length", strconv.FormatInt(length, 10))
	}
	resp, err := c.post(ctx, "cat", q, nil, "")
	if err != nil {
		return nil, err
	}
	want := int64(-1)
	if v, err := strconv.ParseInt(resp.Header.Get("X-Content-Length"), 10, 64); err == nil {
		want = v
	}
	return &stream{resp: resp, want: want}, nil
}

// stream checks a cat body at EOF: for Kubo's error trailer and, as a
// fallback for proxies that drop trailers, against the advertised length.
type stream struct {
	resp *http.Response
	want int64
	n    int64
}

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.resp.Body.Read(p)
	s.n += int64(n)
	if err == io.EOF {
		if terr := trailerError("cat", s.resp); terr != nil {
			return n, terr
		}
		if s.want >= 0 && s.n != s.want {
			return n, fmt.Errorf("kubo cat: stream ended after %d of %d bytes: %w", s.n, s.want, io.ErrUnexpectedEOF)
		}
	}
	return n, err
}

func (s *stream) Close() error { return s.resp.Body.Close() }

// BlockGet returns the raw bytes of the block cid from the local blockstore
// (offline=true), unverified: callers check them against the CID, as
// ipfs.ProvePath does.
func (c *Client) BlockGet(ctx context.Context, cid string) ([]byte, error) {
	resp, err := c.post(ctx, "block/get", url.Values{"arg": {cid}, "offline": {"true"}}, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, ipfs.MaxBlockSize+1))
	if err != nil {
		return nil, fmt.Errorf("kubo block/get: %w", err)
	}
	if len(b) > ipfs.MaxBlockSize {
		return nil, fmt.Errorf("kubo block/get: block %s exceeds %d bytes", cid, ipfs.MaxBlockSize)
	}
	if err := trailerError("block/get", resp); err != nil {
		return nil, err
	}
	return b, nil
}

// PinAdd pins cid recursively. Blocks missing locally are fetched from the
// network for as long as ctx allows.
func (c *Client) PinAdd(ctx context.Context, cid string) error {
	return call[struct{}](ctx, c, "pin/add", url.Values{"arg": {cid}, "recursive": {"true"}}, nil, "", nil)
}

// PinRm removes the recursive pin on cid; unpinned blocks become garbage. An
// unpinned cid yields an error matching ErrNotPinned.
func (c *Client) PinRm(ctx context.Context, cid string) error {
	return call[struct{}](ctx, c, "pin/rm", url.Values{"arg": {cid}, "recursive": {"true"}}, nil, "", nil)
}

// PinLs lists recursive pins as CID → pin type, all of them when cid is empty.
// A cid that is not pinned yields an empty map, not an error.
func (c *Client) PinLs(ctx context.Context, cid string) (map[string]string, error) {
	q := url.Values{"type": {"recursive"}, "stream": {"true"}}
	if cid != "" {
		q.Set("arg", cid)
	}
	pins := map[string]string{}
	err := call(ctx, c, "pin/ls", q, nil, "", func(v struct {
		Cid, Type string
		Keys      map[string]struct{ Type string } // non-streamed form
	}) error {
		if v.Cid != "" {
			pins[v.Cid] = v.Type
		}
		for k, t := range v.Keys {
			pins[k] = t.Type
		}
		return nil
	})
	if cid != "" && errors.Is(err, ErrNotPinned) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return pins, nil
}
