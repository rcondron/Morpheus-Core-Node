// Package kubo is a minimal client for the RPC API of a Kubo (go-ipfs) node,
// covering what a storage provider needs: CAR imports that pin, reads, pins
// and repository statistics. Every command is an HTTP POST to /api/v0/<cmd>.
//
// Kubo reports a failure either as a non-200 response with a JSON body
// ({"Message": …, "Code": 0, "Type": "error"}) or, once a streamed response
// has begun, as an X-Stream-Error trailer. Both surface as *Error, so a
// truncated stream is never mistaken for a complete one.
package kubo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

const streamErrHeader = "X-Stream-Error"

// Client talks to one Kubo node. It is safe for concurrent use; deadlines
// come from the caller's context.
type Client struct {
	base string // scheme://host:port[/prefix], without /api/v0
	auth string
	hc   *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithAuthorization sends value as the Authorization header ("Basic …" or
// "Bearer …"), for nodes that restrict their RPC with API.Authorizations.
func WithAuthorization(value string) Option { return func(c *Client) { c.auth = value } }

// WithHTTPClient replaces http.DefaultClient, e.g. to set a transport.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// New returns a client for the RPC API at api, given as a URL
// ("http://127.0.0.1:5001") or as the multiaddr Kubo prints and writes to
// $IPFS_PATH/api ("/ip4/127.0.0.1/tcp/5001").
func New(api string, opts ...Option) (*Client, error) {
	base, err := baseURL(strings.TrimSpace(api))
	if err != nil {
		return nil, err
	}
	c := &Client{base: base, hc: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func baseURL(api string) (string, error) {
	if strings.HasPrefix(api, "/") {
		p := strings.Split(api[1:], "/")
		if len(p) < 4 || len(p) > 5 || p[2] != "tcp" {
			return "", fmt.Errorf("kubo API address %q: want /ip4|ip6|dns/<host>/tcp/<port>", api)
		}
		host := p[1]
		switch p[0] {
		case "ip4", "dns", "dns4", "dns6":
		case "ip6":
			host = "[" + host + "]"
		default:
			return "", fmt.Errorf("kubo API address %q: unsupported protocol %s", api, p[0])
		}
		scheme := "http"
		if len(p) == 5 {
			if p[4] != "http" && p[4] != "https" {
				return "", fmt.Errorf("kubo API address %q: unsupported protocol %s", api, p[4])
			}
			scheme = p[4]
		}
		return scheme + "://" + host + ":" + p[3], nil
	}
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("kubo API address %q: want an http(s) URL or a multiaddr", api)
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimSuffix(strings.TrimSuffix(u.String(), "/"), "/api/v0"), nil
}

// Error is a failure reported by Kubo.
type Error struct {
	Command string // e.g. "pin/rm"
	Status  int    // HTTP status; 200 when the error arrived in the trailer
	Message string
}

func (e *Error) Error() string { return "kubo " + e.Command + ": " + e.Message }

// ErrNotPinned matches (with errors.Is) Kubo's answer that a CID is not pinned.
var ErrNotPinned = errors.New("not pinned")

// Is lets errors.Is recognise ErrNotPinned, which Kubo only reports as text.
func (e *Error) Is(target error) bool {
	return target == ErrNotPinned && strings.Contains(e.Message, "not pinned")
}

// post sends a command and returns the response once Kubo has accepted it.
func (c *Client) post(ctx context.Context, cmd string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.base + "/api/v0/" + cmd
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubo %s: %w", cmd, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	var kerr struct{ Message string }
	if json.Unmarshal(raw, &kerr) == nil && kerr.Message != "" {
		msg = kerr.Message
	}
	if msg == "" {
		msg = resp.Status
	}
	return nil, &Error{Command: cmd, Status: resp.StatusCode, Message: msg}
}

// trailerError returns the error Kubo sent after the response began; it is
// only known once the body has been read to EOF.
func trailerError(cmd string, resp *http.Response) error {
	if msg := resp.Trailer.Get(streamErrHeader); msg != "" {
		return &Error{Command: cmd, Status: resp.StatusCode, Message: msg}
	}
	return nil
}

// call runs a command that answers with a stream of JSON values (often just
// one), passing each to fn.
func call[T any](ctx context.Context, c *Client, cmd string, q url.Values, body io.Reader, contentType string, fn func(T) error) error {
	resp, err := c.post(ctx, cmd, q, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 256<<20))
	for {
		var v T
		if err := dec.Decode(&v); err == io.EOF {
			return trailerError(cmd, resp)
		} else if err != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // reach EOF for the trailer
			if terr := trailerError(cmd, resp); terr != nil {
				return terr
			}
			return fmt.Errorf("kubo %s: decode response: %w", cmd, err)
		}
		if fn != nil {
			if err := fn(v); err != nil {
				return err
			}
		}
	}
}

// upload runs a command that takes r as its file argument, streamed as
// multipart form data without buffering. It returns once r is no longer read
// (or ctx ends). An error reading r wins over the HTTP error it causes, since
// it is what broke the upload.
func upload[T any](ctx context.Context, c *Client, cmd string, q url.Values, name string, r io.Reader, fn func(T) error) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	src := &sourceReader{r: r}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h := make(textproto.MIMEHeader)
		// Kubo URL-unescapes the file name.
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+url.QueryEscape(name)+`"`)
		h.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(h)
		if err == nil {
			_, err = io.Copy(part, src)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	err := call(ctx, c, cmd, q, pr, mw.FormDataContentType(), fn)
	_ = pr.Close() // unblocks the writer if Kubo stopped reading
	select {
	case <-done:
		if src.err != nil {
			return fmt.Errorf("kubo %s: read upload: %w", cmd, src.err)
		}
	case <-ctx.Done():
	}
	return err
}

// sourceReader remembers the error, other than EOF, its reader returned.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}
