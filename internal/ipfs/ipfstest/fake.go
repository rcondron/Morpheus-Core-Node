// Package ipfstest provides an in-memory imitation of a Kubo node's RPC API,
// served over HTTP, for tests of code built on the kubo client and for the
// devnet's storage providers. It is never linked into the node binary.
//
// It answers like Kubo (status codes, JSON error bodies, NDJSON streams, the
// X-Stream-Error trailer) with these simplifications: add always builds CIDv1
// with raw leaves using the requested size-N chunker (cid-version and
// raw-leaves are ignored), every pin is recursive, nothing is ever fetched
// from a network, and reads do not re-hash blocks (Kubo's default
// Datastore.HashOnRead=false), so a corrupted block is served as is.
package ipfstest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs"
)

// Node is a fake Kubo node. Set the exported fields before the first request.
type Node struct {
	Server *httptest.Server
	URL    string // RPC API address for kubo.New

	// Auth, when set, is the Authorization header every request must carry,
	// as with Kubo's API.Authorizations.
	Auth       string
	Version    string // reported by version (default "0.43.1")
	StorageMax uint64 // reported by repo/stat (default 10 GB, Kubo's default)

	mu     sync.Mutex
	blocks map[ipfs.CID][]byte
	pins   map[ipfs.CID]bool
	calls  map[string]int
}

// New starts a fake node.
func New() *Node {
	n := &Node{
		Version:    "0.43.1",
		StorageMax: 10_000_000_000,
		blocks:     map[ipfs.CID][]byte{},
		pins:       map[ipfs.CID]bool{},
		calls:      map[string]int{},
	}
	n.Server = httptest.NewServer(n)
	n.URL = n.Server.URL
	return n
}

// Close shuts the server down.
func (n *Node) Close() { n.Server.Close() }

// Calls returns how many times a command (e.g. "dag/import") was invoked.
func (n *Node) Calls(cmd string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[cmd]
}

// AddFile imports data with ipfs.Import's defaults, stores and pins it, as a
// provider that already holds the file would have.
func (n *Node) AddFile(data []byte) ipfs.CID {
	root, _, err := ipfs.Import(bytes.NewReader(data), n.put)
	if err != nil {
		panic(err) // storing in memory cannot fail
	}
	n.mu.Lock()
	n.pins[root] = true
	n.mu.Unlock()
	return root
}

// Has reports whether block c is stored.
func (n *Node) Has(c ipfs.CID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.blocks[c]
	return ok
}

// Pinned reports whether c is pinned.
func (n *Node) Pinned(c ipfs.CID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pins[c]
}

// DeleteBlock drops a block, as if the provider lost it: audits touching it
// fail and reads through it end with a stream error.
func (n *Node) DeleteBlock(c ipfs.CID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.blocks, c)
}

// CorruptBlock flips a bit of a stored block (or grows an empty one), so it
// no longer hashes to its CID.
func (n *Node) CorruptBlock(c ipfs.CID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, ok := n.blocks[c]
	if !ok {
		return
	}
	if len(b) == 0 {
		n.blocks[c] = []byte{0}
		return
	}
	b = bytes.Clone(b)
	b[len(b)/2] ^= 1
	n.blocks[c] = b
}

func (n *Node) put(c ipfs.CID, block []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.blocks[c]; !ok {
		n.blocks[c] = bytes.Clone(block)
	}
	return nil
}

func (n *Node) get(c ipfs.CID) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, ok := n.blocks[c]
	if !ok {
		return nil, fmt.Errorf("block was not found locally (offline): ipld: could not find %s", c)
	}
	return b, nil
}

// complete reports the first block missing from the DAG under root.
func (n *Node) complete(root ipfs.CID) error {
	seen := map[ipfs.CID]bool{}
	for stack := []ipfs.CID{root}; len(stack) > 0; {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c] {
			continue
		}
		seen[c] = true
		b, err := n.get(c)
		if err != nil {
			return err
		}
		if c.Codec() != ipfs.CodecDagPB {
			continue
		}
		nd, err := ipfs.DecodePBNode(b)
		if err != nil {
			return err
		}
		for _, l := range nd.Links {
			stack = append(stack, l.Hash)
		}
	}
	return nil
}

// ServeHTTP implements the RPC API.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if n.Auth != "" && r.Header.Get("Authorization") != n.Auth {
		http.Error(w, "Kubo RPC Access Denied: Please provide a valid authorization token as defined in the API.Authorizations configuration.", http.StatusForbidden)
		return
	}
	cmd, ok := strings.CutPrefix(r.URL.Path, "/api/v0/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	n.mu.Lock()
	n.calls[cmd]++
	n.mu.Unlock()
	q := r.URL.Query()
	switch cmd {
	case "version":
		writeJSON(w, map[string]string{"Version": n.Version, "Commit": "ipfstest", "Repo": "18", "System": runtime.GOARCH + "/" + runtime.GOOS, "Golang": runtime.Version()})
	case "repo/stat":
		n.mu.Lock()
		var size uint64
		for _, b := range n.blocks {
			size += uint64(len(b))
		}
		st := map[string]any{"RepoSize": size, "StorageMax": n.StorageMax, "NumObjects": len(n.blocks), "RepoPath": "/ipfstest", "Version": "fs-repo@18"}
		n.mu.Unlock()
		writeJSON(w, st)
	case "dag/import":
		n.dagImport(w, r, q)
	case "add":
		n.add(w, r, q)
	case "cat":
		n.cat(w, q)
	case "block/get":
		c, err := arg(q)
		if err != nil {
			kuboError(w, http.StatusBadRequest, err.Error())
			return
		}
		b, err := n.get(c)
		if err != nil {
			kuboError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(b)
	case "pin/add":
		c, err := arg(q)
		if err == nil {
			err = n.complete(c)
		}
		if err != nil {
			kuboError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n.mu.Lock()
		n.pins[c] = true
		n.mu.Unlock()
		writeJSON(w, map[string][]string{"Pins": {c.String()}})
	case "pin/rm":
		c, err := arg(q)
		if err != nil {
			kuboError(w, http.StatusBadRequest, err.Error())
			return
		}
		n.mu.Lock()
		pinned := n.pins[c]
		delete(n.pins, c)
		n.mu.Unlock()
		if !pinned {
			kuboError(w, http.StatusInternalServerError, "not pinned or pinned indirectly")
			return
		}
		writeJSON(w, map[string][]string{"Pins": {c.String()}})
	case "pin/ls":
		n.pinLs(w, q)
	default:
		http.NotFound(w, r)
	}
}

func (n *Node) dagImport(w http.ResponseWriter, r *http.Request, q url.Values) {
	var roots []ipfs.CID
	err := eachFile(r, func(_ string, f io.Reader) error {
		cr, err := ipfs.NewCARReader(f)
		if err != nil {
			return err
		}
		roots = append(roots, cr.Roots...)
		for {
			c, b, err := cr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("import failed: %w", err)
			}
			_ = n.put(c, b)
		}
	})
	if err != nil {
		kuboError(w, http.StatusInternalServerError, err.Error())
		return
	}
	streamHeaders(w)
	if q.Get("pin-roots") == "false" {
		return
	}
	enc := json.NewEncoder(w)
	done := map[ipfs.CID]bool{}
	for _, c := range roots {
		if done[c] {
			continue
		}
		done[c] = true
		msg := ""
		if err := n.complete(c); err != nil {
			msg = err.Error()
		} else {
			n.mu.Lock()
			n.pins[c] = true
			n.mu.Unlock()
		}
		_ = enc.Encode(map[string]any{"Root": map[string]any{"Cid": map[string]string{"/": c.String()}, "PinErrorMsg": msg}})
	}
}

func (n *Node) add(w http.ResponseWriter, r *http.Request, q url.Values) {
	chunk := 0
	if s := q.Get("chunker"); s != "" {
		v, err := strconv.Atoi(strings.TrimPrefix(s, "size-"))
		if err != nil || !strings.HasPrefix(s, "size-") {
			kuboError(w, http.StatusInternalServerError, fmt.Sprintf("unrecognized chunker option: %s", s))
			return
		}
		chunk = v
	}
	onlyHash := q.Get("only-hash") == "true"
	pin := q.Get("pin") != "false"
	type added struct{ Name, Hash, Size string }
	var out []added
	err := eachFile(r, func(name string, f io.Reader) error {
		var dagSize uint64
		im, err := ipfs.NewImporter(ipfs.Options{ChunkSize: chunk}, func(c ipfs.CID, b []byte) error {
			dagSize += uint64(len(b)) // every block once per occurrence: the root's Tsize
			if onlyHash {
				return nil
			}
			return n.put(c, b)
		})
		if err != nil {
			return err
		}
		if _, err := im.ReadFrom(f); err != nil {
			return err
		}
		root, _, err := im.Finish()
		if err != nil {
			return err
		}
		if pin && !onlyHash {
			n.mu.Lock()
			n.pins[root] = true
			n.mu.Unlock()
		}
		out = append(out, added{name, root.String(), strconv.FormatUint(dagSize, 10)})
		return nil
	})
	if err == nil && len(out) == 0 {
		err = errors.New("file argument 'path' is required")
	}
	if err != nil {
		kuboError(w, http.StatusInternalServerError, err.Error())
		return
	}
	streamHeaders(w)
	enc := json.NewEncoder(w)
	for _, a := range out {
		_ = enc.Encode(a)
	}
}

func (n *Node) cat(w http.ResponseWriter, q url.Values) {
	c, err := arg(q)
	if err != nil {
		kuboError(w, http.StatusBadRequest, err.Error())
		return
	}
	var offset uint64
	if s := q.Get("offset"); s != "" {
		if offset, err = strconv.ParseUint(s, 10, 64); err != nil {
			kuboError(w, http.StatusBadRequest, "cannot specify negative offset")
			return
		}
	}
	root, err := n.get(c)
	var size uint64
	if err == nil {
		size, err = fileSize(c, root)
	}
	if err != nil {
		kuboError(w, http.StatusInternalServerError, err.Error())
		return
	}
	end := size
	if s := q.Get("length"); s != "" {
		l, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			kuboError(w, http.StatusBadRequest, "cannot specify negative length")
			return
		}
		end = min(size, offset+l)
	}
	offset = min(offset, end)
	w.Header().Set("X-Content-Length", strconv.FormatUint(end-offset, 10))
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Trailer", "X-Stream-Error")
	w.WriteHeader(http.StatusOK)
	if err := n.writeRange(w, c, offset, end); err != nil {
		w.Header().Set("X-Stream-Error", err.Error())
	}
}

// writeRange writes bytes [off, end) of the file DAG under c.
func (n *Node) writeRange(w io.Writer, c ipfs.CID, off, end uint64) error {
	b, err := n.get(c)
	if err != nil {
		return err
	}
	if c.Codec() == ipfs.CodecRaw {
		if end > uint64(len(b)) {
			return fmt.Errorf("block %s is shorter than its parent claims", c)
		}
		_, err := w.Write(b[off:end])
		return err
	}
	nd, err := ipfs.DecodePBNode(b)
	if err != nil {
		return err
	}
	d, err := ipfs.DecodeUnixFSData(nd.Data)
	if err != nil {
		return err
	}
	if len(d.Blocksizes) != len(nd.Links) {
		return fmt.Errorf("node %s has %d links and %d blocksizes", c, len(nd.Links), len(d.Blocksizes))
	}
	// File bytes are the node's inline data followed by its children's.
	start := uint64(len(d.Data))
	if off < start {
		if _, err := w.Write(d.Data[off:min(end, start)]); err != nil {
			return err
		}
	}
	for i, l := range nd.Links {
		s, e := start, start+d.Blocksizes[i]
		start = e
		if e <= off || s >= end {
			continue
		}
		if err := n.writeRange(w, l.Hash, max(off, s)-s, min(end, e)-s); err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) pinLs(w http.ResponseWriter, q url.Values) {
	typ := q.Get("type")
	if typ == "" {
		typ = "all"
	}
	var list []ipfs.CID
	n.mu.Lock()
	if typ == "all" || typ == "recursive" {
		for c := range n.pins {
			list = append(list, c)
		}
	}
	n.mu.Unlock()
	if a := q.Get("arg"); a != "" {
		c, err := arg(q)
		if err != nil {
			kuboError(w, http.StatusBadRequest, err.Error())
			return
		}
		found := false
		for _, p := range list {
			found = found || p == c
		}
		if !found {
			kuboError(w, http.StatusInternalServerError, fmt.Sprintf("path '%s' is not pinned", a))
			return
		}
		list = []ipfs.CID{c}
	}
	if q.Get("stream") != "true" {
		keys := map[string]map[string]string{}
		for _, c := range list {
			keys[c.String()] = map[string]string{"Type": "recursive"}
		}
		writeJSON(w, map[string]any{"Keys": keys})
		return
	}
	streamHeaders(w)
	enc := json.NewEncoder(w)
	for _, c := range list {
		_ = enc.Encode(map[string]string{"Cid": c.String(), "Type": "recursive"})
	}
}

func fileSize(c ipfs.CID, b []byte) (uint64, error) {
	if c.Codec() == ipfs.CodecRaw {
		return uint64(len(b)), nil
	}
	nd, err := ipfs.DecodePBNode(b)
	if err != nil {
		return 0, err
	}
	d, err := ipfs.DecodeUnixFSData(nd.Data)
	if err != nil {
		return 0, err
	}
	if d.Type != ipfs.UnixFSFile && d.Type != ipfs.UnixFSRaw {
		return 0, errors.New("this dag node is not a file")
	}
	return d.Filesize, nil
}

// arg parses the command's CID argument, with or without /ipfs/.
func arg(q url.Values) (ipfs.CID, error) {
	a := q.Get("arg")
	if a == "" {
		return ipfs.CID{}, errors.New("argument \"ipfs-path\" is required")
	}
	return ipfs.Parse(strings.TrimPrefix(a, "/ipfs/"))
}

// eachFile calls fn for every file part of a multipart request, with its
// URL-unescaped name as Kubo reads it.
func eachFile(r *http.Request, fn func(name string, f io.Reader) error) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, params, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		name, ok := params["filename"]
		if !ok {
			continue
		}
		if un, err := url.QueryUnescape(name); err == nil {
			name = un
		}
		if err := fn(name, p); err != nil {
			return err
		}
	}
}

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Chunked-Output", "1")
	w.Header().Set("Trailer", "X-Stream-Error")
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// kuboError answers like go-ipfs-cmds: a JSON error with HTTP 500 (or 400
// for bad arguments).
func kuboError(w http.ResponseWriter, status int, msg string) {
	code := 0
	if status == http.StatusBadRequest {
		code = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"Message": msg, "Code": code, "Type": "error"})
}
