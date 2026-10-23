package morrpc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// UploadChunkSize is how much file data each stream chunk carries (the
// reference consumer's choice; providers accept up to 8 MiB).
const UploadChunkSize = 1 << 20

// StreamStartRequest announces a chunked upload of fileSize bytes.
func StreamStartRequest(id string, sessionID common.Hash, streamID string, totalChunks uint32, fileSize uint64, contentType string, s Signer) (*RPCMessage, error) {
	p := StreamStartReq{
		SessionID: sessionID, StreamID: streamID, TotalChunks: totalChunks,
		FileSize: fileSize, ContentType: contentType, Timestamp: Now(),
	}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodStreamStart, p)
}

// StreamChunkRequest carries chunk index of an upload.
func StreamChunkRequest(id string, sessionID common.Hash, streamID string, index uint32, data []byte, s Signer) (*RPCMessage, error) {
	p := StreamChunkReq{
		SessionID: sessionID, StreamID: streamID, ChunkIndex: index,
		ChunkData: base64.StdEncoding.EncodeToString(data), Timestamp: Now(),
	}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodStreamChunk, p)
}

// StreamEndRequest completes an upload and submits the request (JSON) that uses it.
func StreamEndRequest(id string, sessionID common.Hash, streamID string, request []byte, s Signer) (*RPCMessage, error) {
	p := StreamEndReq{SessionID: sessionID, StreamID: streamID, AudioRequestParam: string(request), Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodStreamEnd, p)
}

// VerifyStreamStart checks a provider's signed acknowledgement of an upload
// and returns the stream id to use from then on.
func VerifyStreamStart(res *RPCResponse, providerPub []byte, s Signer) (string, error) {
	if err := ResponseError(res, providerPub, s); err != nil {
		return "", err
	}
	raw, err := resultOf(res)
	if err != nil {
		return "", err
	}
	var r StreamStartRes
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("invalid stream start response: %w", err)
	}
	sig := r.Signature
	r.Signature = HexString{}
	if !VerifyParams(r, sig, providerPub) {
		return "", fmt.Errorf("%w on stream start", ErrInvalidSignature)
	}
	return r.StreamID, nil
}

// VerifyStreamChunk checks a provider's signed receipt for one chunk.
func VerifyStreamChunk(res *RPCResponse, providerPub []byte, s Signer, index uint32) error {
	if err := ResponseError(res, providerPub, s); err != nil {
		return err
	}
	raw, err := resultOf(res)
	if err != nil {
		return err
	}
	var r StreamChunkRes
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("invalid stream chunk response: %w", err)
	}
	sig := r.Signature
	r.Signature = HexString{}
	if !VerifyParams(r, sig, providerPub) {
		return fmt.Errorf("%w on stream chunk", ErrInvalidSignature)
	}
	if r.ChunkIndex != index {
		return fmt.Errorf("provider acknowledged chunk %d, sent %d", r.ChunkIndex, index)
	}
	return nil
}

// StreamStartResponse acknowledges an upload.
func StreamStartResponse(id, streamID string, s Signer) (*RPCResponse, error) {
	p := StreamStartRes{StreamID: streamID, Status: "started", Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// StreamChunkResponse acknowledges one chunk.
func StreamChunkResponse(id, streamID string, index uint32, s Signer) (*RPCResponse, error) {
	p := StreamChunkRes{StreamID: streamID, ChunkIndex: index, Status: "received", Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// AgentToolsRequest asks the provider for an agent's MCP tool list.
func AgentToolsRequest(id string, sessionID common.Hash, s Signer) (*RPCMessage, error) {
	p := AgentToolsReq{SessionID: sessionID, Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodAgentTools, p)
}

// AgentCallRequest calls one MCP tool with JSON arguments.
func AgentCallRequest(id string, sessionID common.Hash, tool string, args json.RawMessage, s Signer) (*RPCMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	p := AgentCallReq{SessionID: sessionID, ToolName: tool, Message: string(args), Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodAgentCall, p)
}

// AgentResponse wraps an encrypted (hex) agent result.
func AgentResponse(id, encryptedHex string, s Signer) (*RPCResponse, error) {
	p := AgentRes{Message: encryptedHex, Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// DecodeAgent verifies and decrypts an agent response.
func DecodeAgent(res *RPCResponse, providerPub []byte, s Signer) ([]byte, error) {
	if err := ResponseError(res, providerPub, s); err != nil {
		return nil, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, err
	}
	var r AgentRes
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("invalid agent response: %w", err)
	}
	sig := r.Signature
	r.Signature = HexString{}
	if !VerifyParams(r, sig, providerPub) {
		return nil, fmt.Errorf("%w on agent response", ErrInvalidSignature)
	}
	return DecryptHex(s, r.Message)
}
