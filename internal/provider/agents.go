package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/mcp"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Agent is an MCP tool server sold on the marketplace. Like the reference
// node, each call starts the server, runs one operation and stops it, so no
// state leaks between consumers.
type Agent struct {
	ID             common.Hash
	Name           string
	Command        string
	Args           []string
	Env            map[string]string
	MaxSessions    int
	CapacityPolicy string
}

// agentTimeout bounds one tool listing or call.
const agentTimeout = 2 * time.Minute

func (a *Agent) start(ctx context.Context, version string) (*mcp.Client, error) {
	env := make([]string, 0, len(a.Env))
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+a.Env[k])
	}
	return mcp.Start(ctx, a.Command, a.Args, env, version)
}

func (p *Provider) agentFor(si *sessionInfo) (*Agent, error) {
	p.mu.Lock()
	a := p.agents[si.ModelID]
	p.mu.Unlock()
	if a == nil {
		return nil, errors.New("this session is not for an agent served by this provider")
	}
	return a, nil
}

func (p *Provider) agentTools(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.AgentToolsReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid agent request: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	si, userPub, err := p.authorize(ctx, req.SessionID, req, sig)
	if err != nil {
		return err
	}
	a, err := p.agentFor(si)
	if err != nil {
		return err
	}
	release, err := p.acquire(ctx, si)
	if err != nil {
		return err
	}
	defer release()
	cctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	c, err := a.start(cctx, p.opt.Version)
	if err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	defer c.Close()
	tools, err := c.ListTools(cctx)
	p.recordAgent(si)
	if err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	if tools == nil {
		tools = []mcp.Tool{}
	}
	return p.agentReply(msg.ID, userPub, tools, send)
}

func (p *Provider) agentCall(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.AgentCallReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid agent request: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	si, userPub, err := p.authorize(ctx, req.SessionID, req, sig)
	if err != nil {
		return err
	}
	a, err := p.agentFor(si)
	if err != nil {
		return err
	}
	args := json.RawMessage(req.Message)
	if !json.Valid(args) {
		return errors.New("tool arguments must be a JSON object")
	}
	release, err := p.acquire(ctx, si)
	if err != nil {
		return err
	}
	defer release()
	cctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	c, err := a.start(cctx, p.opt.Version)
	if err != nil {
		return fmt.Errorf("agent %s: %w", a.Name, err)
	}
	defer c.Close()
	result, err := c.CallTool(cctx, req.ToolName, args)
	p.recordAgent(si)
	if err != nil {
		return fmt.Errorf("agent %s tool %s: %w", a.Name, req.ToolName, err)
	}
	return p.agentReply(msg.ID, userPub, result, send)
}

func (p *Provider) agentReply(id string, userPub []byte, v any, send func(*morrpc.RPCResponse) error) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	enc, err := morrpc.EncryptTo(userPub, b)
	if err != nil {
		return err
	}
	res, err := morrpc.AgentResponse(id, enc, p.w)
	if err != nil {
		return err
	}
	return send(res)
}

func (p *Provider) recordAgent(si *sessionInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served++
	si.Requests++
	si.LastUsed = time.Now()
	p.dirty = true
}
