package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake MCP server when MCP_FAKE_SERVER is set.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_SERVER") == "1" {
		fakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer() {
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	fmt.Println("fake MCP server ready") // a non-JSON banner the client must skip
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		reply := func(result any) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			// Ask the client something first: it must answer, not hang.
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "roots/list"})
			in.Scan()
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info"}})
			reply(map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}})
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Cursor == "" {
				reply(map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}}, "nextCursor": "page2"})
			} else {
				reply(map[string]any{"tools": []any{map[string]any{"name": "sleep", "inputSchema": map[string]any{"type": "object"}}}})
			}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "echo":
				reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": p.Arguments.Text}}, "isError": false})
			case "sleep":
				time.Sleep(10 * time.Second)
			default:
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool " + p.Name}})
			}
		}
	}
}

func start(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, err := Start(ctx, os.Args[0], []string{"-test.run=^$"}, []string{"MCP_FAKE_SERVER=1"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestListAndCall(t *testing.T) {
	c := start(t)
	ctx := context.Background()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "sleep" || !strings.Contains(string(tools[0].InputSchema), `"required":["text"]`) {
		t.Fatalf("unexpected tools %+v", tools)
	}
	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res), `"text":"hello"`) {
		t.Fatalf("unexpected result %s", res)
	}
	if _, err := c.CallTool(ctx, "nope", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected tool error, got %v", err)
	}
}

func TestCallHonorsContext(t *testing.T) {
	c := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if _, err := c.CallTool(ctx, "sleep", nil); err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("cancellation did not stop the call")
	}
}

func TestStartFailure(t *testing.T) {
	if _, err := Start(context.Background(), "/nonexistent/mcp-server", nil, nil, "test"); err == nil {
		t.Fatal("expected an error for a missing command")
	}
}
