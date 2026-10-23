package morrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Storage methods: a morcore extension that sells IPFS storage through
// ordinary marketplace sessions (see docs/storage.md). Requests are signed by
// the user like every MOR-RPC request; results are signed by the provider.
const (
	MethodStorageInfo     = "storage.info"
	MethodStorageAttach   = "storage.attach"
	MethodStoragePutStart = "storage.put.start"
	MethodStoragePutChunk = "storage.put.chunk"
	MethodStoragePutEnd   = "storage.put.end"
	MethodStorageList     = "storage.list"
	MethodStorageGet      = "storage.get"
	MethodStorageProve    = "storage.prove"
	MethodStorageDelete   = "storage.delete"
)

// StorageWindow bounds how old (or early) a signed storage request may be.
const StorageWindow = 10 * time.Minute

// StorageReq is the request for every storage method; each uses a subset.
// Method and Provider are signed with the rest, so a request can neither be
// replayed as another method nor sent to another provider.
type StorageReq struct {
	Method    string    `json:"method"`
	Provider  string    `json:"provider"`          // 0x address of the provider it is for
	Session   string    `json:"session,omitempty"` // 0x session id (attach, put)
	Upload    string    `json:"upload,omitempty"`
	CID       string    `json:"cid,omitempty"`
	Size      uint64    `json:"size,omitempty"`
	Offset    uint64    `json:"offset,omitempty"`
	Length    uint64    `json:"length,omitempty"`
	Label     string    `json:"label,omitempty"`
	Data      string    `json:"data,omitempty"` // base64
	Nonce     string    `json:"nonce,omitempty"`
	Timestamp uint64    `json:"timestamp"`
	Signature HexString `json:"signature,omitempty"`
}

// StorageRes carries a method's result, signed by the provider. Request is
// the hash of the request's signature, binding the result to the request.
type StorageRes struct {
	Result    json.RawMessage `json:"result"`
	Request   string          `json:"request"`
	Timestamp uint64          `json:"timestamp"`
	Signature HexString       `json:"signature,omitempty"`
}

// RequestHash identifies a signed request: the keccak256 of its signature.
func RequestHash(sig HexString) string { return common.BytesToHash(crypto.Keccak256(sig)).Hex() }

// StorageRequest signs a storage request for provider and returns it with
// its RequestHash, which the result must echo.
func StorageRequest(id, method string, provider common.Address, p StorageReq, s Signer) (*RPCMessage, string, error) {
	p.Method, p.Provider = method, provider.Hex()
	p.Timestamp, p.Signature = Now(), nil
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, "", err
	}
	p.Signature = sig
	msg, err := newMessage(id, method, p)
	return msg, RequestHash(sig), err
}

// VerifyStorageRequest decodes a storage request, checks it was signed for
// this method and is fresh, and returns it with the signer's address and
// public key. The caller checks Provider and that the signature is new.
func VerifyStorageRequest(msg *RPCMessage) (*StorageReq, common.Address, []byte, error) {
	var p StorageReq
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, common.Address{}, nil, fmt.Errorf("invalid storage request: %w", err)
	}
	if p.Method != msg.Method {
		return nil, common.Address{}, nil, errors.New("storage request was signed for another method")
	}
	if !common.IsHexAddress(p.Provider) {
		return nil, common.Address{}, nil, errors.New("storage request names no provider")
	}
	ts := time.UnixMilli(int64(p.Timestamp))
	if d := time.Since(ts); d > StorageWindow || d < -2*time.Minute {
		return nil, common.Address{}, nil, errors.New("storage request timestamp is out of range (check the clock)")
	}
	sig := p.Signature
	p.Signature = nil
	pub, addr, err := RecoverParams(p, sig)
	if err != nil {
		return nil, common.Address{}, nil, ErrInvalidSignature
	}
	p.Signature = sig
	return &p, addr, pub, nil
}

// StorageResponse signs a storage result for the request with requestHash.
func StorageResponse(id string, result any, requestHash string, s Signer) (*RPCResponse, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	p := StorageRes{Result: raw, Request: requestHash, Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// VerifyStorageResponse checks a storage result answers the request with
// requestHash, was signed by a key allowed to act for the provider, and
// decodes it into out. It returns the signer's public key.
func VerifyStorageResponse(res *RPCResponse, requestHash string, authorize func(signer common.Address) error, out any) ([]byte, error) {
	if err := ResponseError(res, nil, nil); err != nil {
		return nil, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, err
	}
	var r StorageRes
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("invalid storage response: %w", err)
	}
	sig := r.Signature
	r.Signature = nil
	pub, signer, err := RecoverParams(r, sig)
	if err != nil {
		return nil, fmt.Errorf("%w on storage response", ErrInvalidSignature)
	}
	if r.Request != requestHash {
		return nil, errors.New("storage response answers another request")
	}
	if err := authorize(signer); err != nil {
		return nil, err
	}
	if out == nil {
		return pub, nil
	}
	return pub, json.Unmarshal(r.Result, out)
}

// Storage results.
type (
	// StorageListing is a storage product a provider serves: a marketplace
	// model whose sessions each grant QuotaBytes.
	StorageListing struct {
		ID         string `json:"id"`
		QuotaBytes uint64 `json:"quotaBytes"`
	}
	StorageInfo struct {
		Version      string           `json:"version"`
		Listings     []StorageListing `json:"listings"`
		MaxFileBytes uint64           `json:"maxFileBytes"`
		GraceSeconds uint64           `json:"graceSeconds"`
		Payment      string           `json:"payment"` // "direct" or "any"
		Gateway      string           `json:"gateway,omitempty"`
		ChunkBytes   uint64           `json:"chunkBytes"`
		Nonce        string           `json:"nonce"`
	}
	// StorageAccount is a user's standing with a provider.
	StorageAccount struct {
		QuotaBytes   uint64 `json:"quotaBytes"`
		UsedBytes    uint64 `json:"usedBytes"`
		CoveredUntil int64  `json:"coveredUntil"` // unix seconds; files are kept GraceSeconds longer
	}
	StorageUpload struct {
		Upload     string `json:"upload"`
		ChunkBytes uint64 `json:"chunkBytes"`
		Received   uint64 `json:"received"`
	}
	// StorageReceipt is the provider's signed acknowledgement of a stored file.
	StorageReceipt struct {
		CID      string `json:"cid"`
		Size     uint64 `json:"size"`
		Owner    string `json:"owner"`
		StoredAt int64  `json:"storedAt"`
	}
	StorageFile struct {
		CID      string `json:"cid"`
		Size     uint64 `json:"size"`
		Label    string `json:"label,omitempty"`
		StoredAt int64  `json:"storedAt"`
	}
	StorageList struct {
		StorageAccount
		Files []StorageFile `json:"files"`
	}
	StorageData struct {
		CID    string `json:"cid"`
		Offset uint64 `json:"offset"`
		Data   string `json:"data"` // base64
	}
	// StorageProof is the chain of DAG blocks from the root to the leaf that
	// holds Offset, which the user checks against the CID it computed.
	StorageProof struct {
		CID    string   `json:"cid"`
		Offset uint64   `json:"offset"`
		Nonce  string   `json:"nonce"`
		Blocks []string `json:"blocks"` // base64
	}
	StorageDeleted struct {
		CID        string `json:"cid"`
		FreedBytes uint64 `json:"freedBytes"`
	}
)
