// Package mcp is a minimal Model Context Protocol client for stdio servers:
// enough to list and call tools, which is what marketplace agents sell.
// Messages are newline-delimited JSON-RPC 2.0 on the server's stdin/stdout.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// Tool describes one tool as listed by the server. InputSchema is kept raw so
// schemas pass through unchanged.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client talks to one running MCP server process.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	stderr *tailBuffer

	callMu sync.Mutex // one request in flight: responses are read in order
	mu     sync.Mutex
	nextID int64
	closed bool
}

// Start launches command with args and extra environment (KEY=VALUE) and
// performs the MCP initialize handshake. The process ends when ctx is
// cancelled or Close is called.
func Start(ctx context.Context, command string, args []string, env []string, version string) (*Client, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailBuffer{max: 4 << 10}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	c := &Client{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 64<<10), stderr: tail}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "morcore", "version": version},
	}
	if err := c.call(ctx, "initialize", params, &init); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify("notifications/initialized"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for range 100 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
	return all, errors.New("tools/list did not finish paging")
}

// CallTool invokes a tool and returns the raw CallToolResult object
// ({"content":[…],"isError":…}); tool-level failures are results, not errors.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	var result json.RawMessage
	err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result)
	return result, err
}

// Close stops the server.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) notify(method string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method})
}

// call sends a request and waits for its response, answering server-initiated
// requests (roots, sampling, …) with "method not found" so servers never hang.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return c.fail(err)
	}
	type line struct {
		b   []byte
		err error
	}
	for {
		ch := make(chan line, 1)
		go func() {
			b, err := c.out.ReadBytes('\n')
			ch <- line{b, err}
		}()
		var l line
		select {
		case <-ctx.Done():
			c.Close()
			return ctx.Err()
		case l = <-ch:
		}
		if l.err != nil {
			return c.fail(l.err)
		}
		var env envelope
		if json.Unmarshal(l.b, &env) != nil {
			continue // servers sometimes print banners; ignore non-JSON lines
		}
		if env.Method != "" {
			if len(env.ID) > 0 { // a request from the server
				_ = c.write(map[string]any{"jsonrpc": "2.0", "id": env.ID,
					"error": map[string]any{"code": -32601, "message": "method not supported by client"}})
			}
			continue // notification (logging, progress, …)
		}
		if string(env.ID) != fmt.Sprint(id) {
			continue
		}
		if env.Error != nil {
			return env.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(env.Result, out)
	}
}

func (c *Client) fail(err error) error {
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return fmt.Errorf("%w (server stderr: %s)", err, msg)
	}
	return err
}

// tailBuffer keeps the last max bytes written, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
