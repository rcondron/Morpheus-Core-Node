package morrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
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
type StorageReq struct {
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

// StorageRes carries a method's result, signed by the provider.
type StorageRes struct {
	Result    json.RawMessage `json:"result"`
	Timestamp uint64          `json:"timestamp"`
	Signature HexString       `json:"signature,omitempty"`
}

// StorageRequest signs a storage request.
func StorageRequest(id, method string, p StorageReq, s Signer) (*RPCMessage, error) {
	p.Timestamp, p.Signature = Now(), nil
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, method, p)
}

// VerifyStorageRequest decodes a storage request, checks its freshness and
// returns it with the signer's address and public key.
func VerifyStorageRequest(msg *RPCMessage) (*StorageReq, common.Address, []byte, error) {
	var p StorageReq
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, common.Address{}, nil, fmt.Errorf("invalid storage request: %w", err)
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

// StorageResponse signs a storage result.
func StorageResponse(id string, result any, s Signer) (*RPCResponse, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	p := StorageRes{Result: raw, Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// VerifyStorageResponse checks a storage result was signed by a key allowed
// to act for the provider and decodes it into out.
func VerifyStorageResponse(res *RPCResponse, authorize func(signer common.Address) error, out any) error {
	if err := ResponseError(res, nil, nil); err != nil {
		return err
	}
	raw, err := resultOf(res)
	if err != nil {
		return err
	}
	var r StorageRes
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("invalid storage response: %w", err)
	}
	sig := r.Signature
	r.Signature = nil
	_, signer, err := RecoverParams(r, sig)
	if err != nil {
		return fmt.Errorf("%w on storage response", ErrInvalidSignature)
	}
	if err := authorize(signer); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
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
