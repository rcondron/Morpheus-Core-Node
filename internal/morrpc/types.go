// Package morrpc implements the MOR-RPC wire protocol spoken between consumer
// and provider nodes on the Morpheus compute marketplace (TCP, provider port 3333).
//
// Every message is a single JSON object on a fresh TCP connection. Payloads are
// signed with secp256k1 over keccak256(json(params with an empty signature)), so
// the struct definitions below deliberately mirror the reference
// Morpheus-Lumerin-Node proxy-router field-for-field: field order, JSON tags and
// omitempty flags all change the signed bytes.
package morrpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"github.com/ethereum/go-ethereum/common"
)

// Method names.
const (
	MethodPing          = "network.ping"
	MethodSessionReq    = "session.request"
	MethodSessionPrompt = "session.prompt"
	MethodSessionReport = "session.report"
)

var hexRe = regexp.MustCompile(`^(0[xX])?[0-9a-fA-F]+$`)

// HexString is a byte slice that marshals as 0x-prefixed lowercase hex.
type HexString []byte

func (b HexString) String() string { return "0x" + hex.EncodeToString(b) }

func (b HexString) MarshalJSON() ([]byte, error) { return []byte(`"` + b.String() + `"`), nil }

func (b *HexString) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) > 0 && s[0] == '"' {
		if len(s) < 2 || s[len(s)-1] != '"' {
			return fmt.Errorf("invalid hex string %s", s)
		}
		s = s[1 : len(s)-1]
	}
	if s == "" {
		*b = HexString{}
		return nil
	}
	if !hexRe.MatchString(s) {
		return fmt.Errorf("invalid hex string %q", s)
	}
	*b = common.FromHex(s)
	return nil
}

// BigInt marshals as a quoted decimal string.
type BigInt struct{ big.Int }

func NewBigInt(v *big.Int) BigInt {
	var b BigInt
	if v != nil {
		b.Int.Set(v)
	}
	return b
}

func (b BigInt) MarshalJSON() ([]byte, error) { return []byte(`"` + b.String() + `"`), nil }

func (b *BigInt) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) > 1 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	if _, ok := b.Int.SetString(s, 10); !ok {
		return fmt.Errorf("invalid big integer %q", s)
	}
	return nil
}

// RPCMessage is a request.
type RPCMessage struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// RPCResponse is a response; a provider may stream several per request.
type RPCResponse struct {
	ID     string           `json:"id"`
	Result *json.RawMessage `json:"result"`
	Error  *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Message string       `json:"message"`
	Code    int          `json:"code"`
	Data    RPCErrorData `json:"data"`
}

type RPCErrorData struct {
	Timestamp uint64    `json:"timestamp"`
	Signature HexString `json:"signature"`
}

type PingReq struct {
	Nonce     HexString `json:"nonce"`
	Signature HexString `json:"signature"`
}

// PongRes: Models is attached after signing and excluded from the signed payload.
type PongRes struct {
	Nonce     HexString           `json:"nonce"`
	Version   string              `json:"version,omitempty"`
	Models    []ModelHealthReport `json:"models,omitempty"`
	Signature HexString           `json:"signature"`
}

// ModelHealthReport is a provider's self-reported model status.
type ModelHealthReport struct {
	ModelID       string `json:"modelId"`
	ModelName     string `json:"modelName,omitempty"`
	ModelType     string `json:"modelType,omitempty"`
	HasActiveBid  bool   `json:"hasActiveBid"`
	BidID         string `json:"bidId,omitempty"`
	Status        string `json:"status"`
	LastHealthy   int64  `json:"lastHealthy,omitempty"`
	LastChecked   int64  `json:"lastChecked"`
	LatencyMs     int64  `json:"latencyMs,omitempty"`
	PromptCorrect *bool  `json:"promptCorrect,omitempty"`
	ErrorKind     string `json:"errorKind,omitempty"`
	HTTPStatus    int    `json:"httpStatus,omitempty"`
}

// Model health statuses (see nodedocs.mor.org/providers/full/model-health).
const (
	HealthHealthy       = "healthy"
	HealthDegraded      = "degraded"
	HealthUnhealthy     = "unhealthy"
	HealthSkipped       = "skipped"
	HealthNoModel       = "no_model_configured"
	HealthNoBid         = "no_bid"
	HealthTeeUnverified = "tee_unverified"
)

type SessionReq struct {
	Signature HexString      `json:"signature,omitempty"`
	User      common.Address `json:"user"`
	Key       HexString      `json:"key"`
	Spend     BigInt         `json:"spend"`
	Timestamp uint64         `json:"timestamp"`
	BidID     common.Hash    `json:"bidid"`
}

type SessionRes struct {
	PubKey      HexString      `json:"message"`
	Approval    HexString      `json:"approval"`
	ApprovalSig HexString      `json:"approvalSig"`
	User        common.Address `json:"user"`
	Timestamp   uint64         `json:"timestamp"`
	Signature   HexString      `json:"signature,omitempty"`
}

type SessionPromptReq struct {
	Signature HexString   `json:"signature,omitempty"`
	SessionID common.Hash `json:"sessionid"`
	Message   string      `json:"message"`
	Timestamp uint64      `json:"timestamp"`
}

type SessionPromptRes struct {
	Message   string    `json:"message"`
	Signature HexString `json:"signature,omitempty"`
	Timestamp uint64    `json:"timestamp"`
}

// InferenceRes is how consumers decode SessionPromptRes (message kept raw).
type InferenceRes struct {
	Signature HexString       `json:"signature,omitempty"`
	Message   json.RawMessage `json:"message"`
	Timestamp uint64          `json:"timestamp"`
}

type SessionReportReq struct {
	Signature HexString `json:"signature,omitempty"`
	Message   string    `json:"message"`
	Timestamp uint64    `json:"timestamp"`
}

type SessionReportRes struct {
	Message      HexString `json:"message"`
	SignedReport HexString `json:"signedReport"`
	Signature    HexString `json:"signature,omitempty"`
	Timestamp    uint64    `json:"timestamp"`
}

// AIEngineError is the (encrypted) body of a signed provider error: the
// backend model's error payload and its upstream HTTP status.
type AIEngineError struct {
	ProviderModelError any `json:"providerModelError"`
	StatusCode         int `json:"statusCode,omitempty"`
}

// HTTPStatus returns the upstream status when known, otherwise 400.
func (e *AIEngineError) HTTPStatus() int {
	if e.StatusCode >= 400 && e.StatusCode <= 599 {
		return e.StatusCode
	}
	return 400
}

// StreamDone is the control message a provider sends when a stream completes.
const StreamDone = "[DONE]"
