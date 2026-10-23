package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Upload sends a file to the provider in signed chunks
// (session.prompt.stream.*) and then the request that consumes it, such as an
// audio transcription. Responses arrive like Send's.
func (m *Manager) Upload(ctx context.Context, l *Lease, file io.Reader, size int64, contentType string, request []byte, onChunk func(payload []byte) error) (*Result, error) {
	if size <= 0 {
		return nil, errors.New("upload is empty")
	}
	s := l.Session
	before := func(err error) error { return &ProviderError{Err: err, BeforeOutput: true} }
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	streamID := hex.EncodeToString(idBytes)
	total := uint32((size + morrpc.UploadChunkSize - 1) / morrpc.UploadChunkSize)

	msg, err := morrpc.StreamStartRequest(newRequestID(), s.ID, streamID, total, uint64(size), contentType, m.w)
	if err != nil {
		return nil, err
	}
	res, err := m.rpc.Call(ctx, s.Endpoint, msg)
	if err != nil {
		return nil, before(err)
	}
	acked, err := morrpc.VerifyStreamStart(res, s.ProviderKey, m.w)
	if err != nil {
		return nil, before(err)
	}
	if acked != "" {
		streamID = acked
	}
	buf := make([]byte, morrpc.UploadChunkSize)
	for i := uint32(0); i < total; i++ {
		n, err := io.ReadFull(file, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("read upload: %w", err)
		}
		msg, err := morrpc.StreamChunkRequest(newRequestID(), s.ID, streamID, i, buf[:n], m.w)
		if err != nil {
			return nil, err
		}
		res, err := m.rpc.Call(ctx, s.Endpoint, msg)
		if err != nil {
			return nil, before(err)
		}
		if err := morrpc.VerifyStreamChunk(res, s.ProviderKey, m.w, i); err != nil {
			return nil, before(err)
		}
	}
	end, err := morrpc.StreamEndRequest(newRequestID(), s.ID, streamID, request, m.w)
	if err != nil {
		return nil, err
	}
	return m.exchange(ctx, l, end, request, onChunk)
}

// Agent lists an MCP agent's tools (tool == "") or calls one with JSON args.
// It returns the provider's decrypted result.
func (m *Manager) Agent(ctx context.Context, l *Lease, tool string, args json.RawMessage) ([]byte, error) {
	s := l.Session
	var msg *morrpc.RPCMessage
	var err error
	if tool == "" {
		msg, err = morrpc.AgentToolsRequest(newRequestID(), s.ID, m.w)
	} else {
		msg, err = morrpc.AgentCallRequest(newRequestID(), s.ID, tool, args, m.w)
	}
	if err != nil {
		return nil, err
	}
	// Tools can run for minutes: wait as long as for a model's first token.
	var res *morrpc.RPCResponse
	err = m.rpc.Stream(ctx, s.Endpoint, msg, m.opt.ProviderTimeout, m.opt.ProviderTimeout, func(r *morrpc.RPCResponse) (bool, error) {
		res = r
		return true, nil
	})
	if err == nil && res == nil {
		err = errors.New("provider closed the connection without replying")
	}
	if err != nil {
		return nil, &ProviderError{Err: err, BeforeOutput: true}
	}
	out, err := morrpc.DecodeAgent(res, s.ProviderKey, m.w)
	if err != nil {
		var ee *morrpc.EngineError
		if errors.As(err, &ee) {
			return nil, err
		}
		return nil, &ProviderError{Err: err, BeforeOutput: true}
	}
	return out, nil
}
