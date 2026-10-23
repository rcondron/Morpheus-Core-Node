package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Upload limits, matching the reference provider.
const (
	uploadMaxFile       = 256 << 20
	uploadMaxChunk      = 8 << 20
	uploadMaxChunks     = 4096
	uploadMaxPerSession = 4
	uploadMaxInflight   = 2 << 30
	uploadTTL           = time.Hour
)

// audioExtensions maps upload content types to the file extension backends
// use to detect the audio format.
var audioExtensions = map[string]string{
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/wav": ".wav", "audio/x-wav": ".wav",
	"audio/ogg": ".ogg", "audio/flac": ".flac", "audio/aac": ".aac", "audio/mp4": ".m4a",
	"audio/x-m4a": ".m4a", "audio/webm": ".webm", "audio/opus": ".opus", "audio/x-ms-wma": ".wma",
	"audio/amr": ".amr", "audio/3gpp": ".3gp", "audio/aiff": ".aiff", "audio/x-aiff": ".aiff",
	"video/mp4": ".mp4", "video/webm": ".webm",
}

type upload struct {
	session common.Hash
	id      string
	total   uint32
	size    uint64
	next    uint32
	written uint64
	path    string
	f       *os.File
	created time.Time
}

type uploads struct {
	mu       sync.Mutex
	m        map[string]*upload
	inflight uint64
}

func uploadKey(session common.Hash, id string) string { return session.Hex() + "/" + id }

func validStreamID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (u *uploads) remove(key string) {
	u.mu.Lock()
	up := u.m[key]
	delete(u.m, key)
	if up != nil {
		u.inflight -= up.size
	}
	u.mu.Unlock()
	if up != nil {
		up.f.Close()
		os.Remove(up.path)
	}
}

// expire drops uploads abandoned for longer than the TTL.
func (u *uploads) expire() { u.expireOlder(uploadTTL) }

// expireAll removes every pending upload (shutdown).
func (u *uploads) expireAll() { u.expireOlder(-1) }

func (u *uploads) expireOlder(age time.Duration) {
	u.mu.Lock()
	var stale []string
	for k, up := range u.m {
		if time.Since(up.created) > age {
			stale = append(stale, k)
		}
	}
	u.mu.Unlock()
	for _, k := range stale {
		u.remove(k)
	}
}

func (p *Provider) streamStart(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.StreamStartReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid stream start: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	si, _, err := p.authorize(ctx, req.SessionID, req, sig)
	if err != nil {
		return err
	}
	if m := p.model(si.ModelID); m == nil || !m.servesUploads() {
		return errors.New("this model does not accept file uploads")
	}
	switch {
	case !validStreamID(req.StreamID):
		return errors.New("invalid stream id")
	case req.FileSize == 0 || req.FileSize > uploadMaxFile:
		return fmt.Errorf("file size must be between 1 byte and %d MiB", uploadMaxFile>>20)
	case req.TotalChunks == 0 || req.TotalChunks > uploadMaxChunks:
		return fmt.Errorf("chunk count must be between 1 and %d", uploadMaxChunks)
	}
	ext := audioExtensions[strings.ToLower(strings.TrimSpace(req.ContentType))]
	key := uploadKey(req.SessionID, req.StreamID)

	p.uploads.mu.Lock()
	perSession := 0
	for _, up := range p.uploads.m {
		if up.session == req.SessionID {
			perSession++
		}
	}
	switch {
	case p.uploads.m[key] != nil:
		p.uploads.mu.Unlock()
		return errors.New("stream id already in use")
	case perSession >= uploadMaxPerSession:
		p.uploads.mu.Unlock()
		return errors.New("too many uploads in progress for this session")
	case p.uploads.inflight+req.FileSize > uploadMaxInflight:
		p.uploads.mu.Unlock()
		return errors.New("provider upload capacity reached; retry later")
	}
	f, err := os.CreateTemp("", "morcore-upload-*"+ext)
	if err != nil {
		p.uploads.mu.Unlock()
		return fmt.Errorf("create upload file: %w", err)
	}
	p.uploads.m[key] = &upload{session: req.SessionID, id: req.StreamID, total: req.TotalChunks, size: req.FileSize, path: f.Name(), f: f, created: time.Now()}
	p.uploads.inflight += req.FileSize
	p.uploads.mu.Unlock()

	res, err := morrpc.StreamStartResponse(msg.ID, req.StreamID, p.w)
	if err != nil {
		p.uploads.remove(key)
		return err
	}
	return send(res)
}

func (p *Provider) streamChunk(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.StreamChunkReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid stream chunk: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	if _, _, err := p.authorize(ctx, req.SessionID, req, sig); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(req.ChunkData)
	if err != nil {
		return fmt.Errorf("chunk data is not base64: %w", err)
	}
	if len(data) == 0 || len(data) > uploadMaxChunk {
		return fmt.Errorf("chunk size must be between 1 byte and %d MiB", uploadMaxChunk>>20)
	}
	key := uploadKey(req.SessionID, req.StreamID)
	p.uploads.mu.Lock()
	up := p.uploads.m[key]
	if up == nil {
		p.uploads.mu.Unlock()
		return errors.New("unknown upload stream")
	}
	if req.ChunkIndex != up.next {
		p.uploads.mu.Unlock()
		return fmt.Errorf("expected chunk %d, got %d", up.next, req.ChunkIndex)
	}
	if up.next >= up.total || up.written+uint64(len(data)) > up.size {
		p.uploads.mu.Unlock()
		return errors.New("chunk exceeds the announced upload size")
	}
	_, err = up.f.Write(data)
	if err == nil {
		up.next++
		up.written += uint64(len(data))
	}
	p.uploads.mu.Unlock()
	if err != nil {
		p.uploads.remove(key)
		return fmt.Errorf("store chunk: %w", err)
	}
	res, err := morrpc.StreamChunkResponse(msg.ID, req.StreamID, req.ChunkIndex, p.w)
	if err != nil {
		return err
	}
	return send(res)
}

// streamEnd runs the request that consumes a completed upload; responses go
// out as ordinary prompt responses.
func (p *Provider) streamEnd(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.StreamEndReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid stream end: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	si, userPub, err := p.authorize(ctx, req.SessionID, req, sig)
	if err != nil {
		return err
	}
	key := uploadKey(req.SessionID, req.StreamID)
	p.uploads.mu.Lock()
	up := p.uploads.m[key]
	complete := up != nil && up.next == up.total && up.written == up.size
	p.uploads.mu.Unlock()
	if up == nil {
		return errors.New("unknown upload stream")
	}
	defer p.uploads.remove(key)
	if !complete {
		return fmt.Errorf("incomplete upload: received %d of %d chunks", up.next, up.total)
	}
	if err := up.f.Sync(); err != nil {
		return err
	}
	r, err := parseRequest([]byte(req.AudioRequestParam))
	if err == nil && r.kind == morrpc.TypeChat {
		r.kind = morrpc.TypeTranscription
	}
	if err == nil && r.kind != morrpc.TypeTranscription {
		err = invalid("uploads are only used for %s requests", morrpc.TypeTranscription)
	}
	if err != nil {
		e := &emitter{p: p, id: msg.ID, userPub: userPub, send: send, st: &forwardStats{}, start: time.Now()}
		return e.reject(err)
	}
	r.file = up.path
	return p.serve(ctx, si, r, userPub, msg.ID, send)
}
