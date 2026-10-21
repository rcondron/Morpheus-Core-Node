package morrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// ---------------------------------------------------------------------------
// Consumer → provider requests
// ---------------------------------------------------------------------------

func newMessage(id, method string, params any) (*RPCMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &RPCMessage{ID: id, Method: method, Params: raw}, nil
}

// PingRequest builds a signed network.ping.
func PingRequest(id string, nonce []byte, s Signer) (*RPCMessage, error) {
	p := PingReq{Nonce: nonce}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodPing, p)
}

// SessionRequest builds a signed session.request for a bid.
func SessionRequest(id string, spend *big.Int, bidID common.Hash, s Signer) (*RPCMessage, error) {
	p := SessionReq{
		User:      s.Address(),
		Key:       s.PublicKey(),
		Spend:     NewBigInt(spend),
		Timestamp: Now(),
		BidID:     bidID,
	}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodSessionReq, p)
}

// PromptRequest builds a signed session.prompt. message is the JSON request
// body (OpenAI chat/embeddings shape) sent to the provider's model.
func PromptRequest(id string, sessionID common.Hash, message []byte, s Signer) (*RPCMessage, error) {
	p := SessionPromptReq{SessionID: sessionID, Message: string(message), Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodSessionPrompt, p)
}

// ReportRequest builds a signed session.report.
func ReportRequest(id string, sessionID common.Hash, s Signer) (*RPCMessage, error) {
	p := SessionReportReq{Message: sessionID.Hex(), Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newMessage(id, MethodSessionReport, p)
}

// ---------------------------------------------------------------------------
// Consumer-side response validation
// ---------------------------------------------------------------------------

// ErrProviderPlain is wrapped around unsigned (transport/infrastructure) provider errors.
var ErrProviderPlain = errors.New("provider error")

// ResponseError converts an RPC error into a Go error. Signed errors are
// verified and decrypted into an *EngineError; unsigned ones become ErrProviderPlain.
func ResponseError(res *RPCResponse, providerPub []byte, s Signer) error {
	if res.Error == nil {
		return nil
	}
	sig := res.Error.Data.Signature
	if len(sig) == 0 || (len(sig) == 1 && sig[0] == 0) {
		return fmt.Errorf("%w: %s", ErrProviderPlain, res.Error.Message)
	}
	if providerPub == nil || s == nil {
		// Signed errors carry an encrypted body we cannot open in this context.
		return fmt.Errorf("%w (code %d)", ErrProviderPlain, res.Error.Code)
	}
	e := *res.Error
	e.Data.Signature = HexString{}
	if !VerifyParams(e, sig, providerPub) {
		return fmt.Errorf("%w on provider error", ErrInvalidSignature)
	}
	plain, err := DecryptHex(s, res.Error.Message)
	if err != nil {
		return fmt.Errorf("decrypt provider error: %w", err)
	}
	var ae AIEngineError
	if err := json.Unmarshal(plain, &ae); err != nil {
		return fmt.Errorf("%w: %s", ErrProviderPlain, string(plain))
	}
	return &EngineError{AIEngineError: ae, Raw: plain}
}

// EngineError is an error the provider's model backend returned (e.g. 429).
type EngineError struct {
	AIEngineError
	Raw []byte
}

func (e *EngineError) Error() string {
	return fmt.Sprintf("model backend error (status %d): %s", e.StatusCode, string(e.Raw))
}

func resultOf(res *RPCResponse) ([]byte, error) {
	if res.Result == nil || len(*res.Result) == 0 || string(*res.Result) == "null" {
		return nil, errors.New("empty result from provider")
	}
	return *res.Result, nil
}

// VerifyPong validates a pong and returns it with the signer's recovered public key.
func VerifyPong(res *RPCResponse, nonce []byte) (*PongRes, []byte, common.Address, error) {
	if err := ResponseError(res, nil, nil); err != nil {
		return nil, nil, common.Address{}, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, nil, common.Address{}, err
	}
	var pong PongRes
	if err := json.Unmarshal(raw, &pong); err != nil {
		return nil, nil, common.Address{}, fmt.Errorf("invalid pong: %w", err)
	}
	if nonce != nil && string(pong.Nonce) != string(nonce) {
		return nil, nil, common.Address{}, errors.New("pong nonce mismatch")
	}
	sig, models := pong.Signature, pong.Models
	pong.Signature, pong.Models = HexString{}, nil
	pub, addr, err := RecoverParams(pong, sig)
	if err != nil {
		return nil, nil, common.Address{}, err
	}
	pong.Signature, pong.Models = sig, models
	return &pong, pub, addr, nil
}

// VerifySessionRes validates a session approval: the response must be signed by
// the embedded public key, the ABI-encoded approval must match the request, and
// the approval signer must be that same key. authorize decides whether that
// key may act for the bid's provider (the provider itself, a contract owner or
// a delegate), mirroring the Diamond's own receipt check.
func VerifySessionRes(res *RPCResponse, user common.Address, bidID common.Hash, chainID *big.Int, authorize func(signer common.Address) error) (*SessionRes, error) {
	if err := ResponseError(res, nil, nil); err != nil {
		return nil, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, err
	}
	var sr SessionRes
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("invalid session response: %w", err)
	}
	sig := sr.Signature
	sr.Signature = HexString{}
	if !VerifyParams(sr, sig, sr.PubKey) {
		return nil, fmt.Errorf("%w on session approval", ErrInvalidSignature)
	}
	sr.Signature = sig
	keyAddr := PubKeyAddress(sr.PubKey)
	approval, err := DecodeApproval(sr.Approval)
	if err != nil {
		return nil, fmt.Errorf("invalid approval encoding: %w", err)
	}
	if approval.BidID != bidID || approval.User != user || approval.ChainID.Cmp(chainID) != 0 {
		return nil, errors.New("approval does not match the requested bid, user or chain")
	}
	signer, err := RecoverEthMessage(sr.Approval, sr.ApprovalSig)
	if err != nil || signer != keyAddr {
		return nil, errors.New("approval is not signed by the provider key")
	}
	if authorize != nil {
		if err := authorize(signer); err != nil {
			return nil, err
		}
	}
	return &sr, nil
}

// DecodeInference verifies one streamed prompt response and decrypts its payload.
func DecodeInference(res *RPCResponse, providerPub []byte, s Signer) ([]byte, error) {
	if err := ResponseError(res, providerPub, s); err != nil {
		return nil, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, err
	}
	var inf InferenceRes
	if err := json.Unmarshal(raw, &inf); err != nil {
		return nil, fmt.Errorf("invalid inference response: %w", err)
	}
	sig := inf.Signature
	inf.Signature = HexString{}
	if !VerifyParams(inf, sig, providerPub) {
		return nil, fmt.Errorf("%w on inference chunk", ErrInvalidSignature)
	}
	var ct HexString
	if err := json.Unmarshal(inf.Message, &ct); err != nil {
		return nil, fmt.Errorf("invalid inference payload: %w", err)
	}
	return s.Decrypt(ct)
}

// VerifyReportRes validates a provider-signed session report.
func VerifyReportRes(res *RPCResponse, providerPub []byte) (*SessionReportRes, error) {
	if err := ResponseError(res, providerPub, nil); err != nil {
		return nil, err
	}
	raw, err := resultOf(res)
	if err != nil {
		return nil, err
	}
	var rr SessionReportRes
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("invalid report response: %w", err)
	}
	sig := rr.Signature
	rr.Signature = HexString{}
	if !VerifyParams(rr, sig, providerPub) {
		return nil, fmt.Errorf("%w on session report", ErrInvalidSignature)
	}
	rr.Signature = sig
	return &rr, nil
}

// ---------------------------------------------------------------------------
// Provider → consumer responses
// ---------------------------------------------------------------------------

func newResult(id string, v any) (*RPCResponse, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(b)
	return &RPCResponse{ID: id, Result: &raw}, nil
}

// PongResponse answers a ping; models are attached after signing.
func PongResponse(id string, nonce []byte, version string, models []ModelHealthReport, s Signer) (*RPCResponse, error) {
	p := PongRes{Nonce: nonce, Version: version}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Models = models
	p.Signature = sig
	return newResult(id, p)
}

// SessionResponse approves a session request for (bidID, user) on chainID.
func SessionResponse(id string, user common.Address, bidID common.Hash, chainID *big.Int, s Signer) (*RPCResponse, error) {
	ts := Now()
	approval, err := EncodeApproval(bidID, chainID, user, ts)
	if err != nil {
		return nil, err
	}
	approvalSig, err := SignEthMessage(approval, s)
	if err != nil {
		return nil, err
	}
	p := SessionRes{PubKey: s.PublicKey(), Approval: approval, ApprovalSig: approvalSig, User: user, Timestamp: ts}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// PromptResponse wraps an already-encrypted (hex) payload chunk.
func PromptResponse(id string, encryptedHex string, s Signer) (*RPCResponse, error) {
	p := SessionPromptRes{Message: encryptedHex, Timestamp: Now()}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}

// SignedError returns a signed error whose message is encrypted to the user.
func SignedError(id string, encryptedHex string, s Signer) (*RPCResponse, error) {
	e := RPCError{Message: encryptedHex, Code: 400, Data: RPCErrorData{Timestamp: Now()}}
	sig, err := SignParams(e, s)
	if err != nil {
		return nil, err
	}
	e.Data.Signature = sig
	return &RPCResponse{ID: id, Error: &e}, nil
}

// PlainError is an unsigned infrastructure error (signature "0x00"), matching
// what reference consumers expect for errors outside the model backend.
func PlainError(id string, msg string) *RPCResponse {
	return &RPCResponse{ID: id, Error: &RPCError{
		Message: msg,
		Code:    400,
		Data:    RPCErrorData{Timestamp: Now(), Signature: HexString{0}},
	}}
}

// ReportResponse signs a closeout receipt for the consumer to submit on-chain.
func ReportResponse(id string, r Report, s Signer) (*RPCResponse, error) {
	if r.Timestamp == 0 {
		r.Timestamp = Now()
	}
	msg, signed, err := SignedReport(r, s)
	if err != nil {
		return nil, err
	}
	p := SessionReportRes{Message: msg, SignedReport: signed, Timestamp: r.Timestamp}
	sig, err := SignParams(p, s)
	if err != nil {
		return nil, err
	}
	p.Signature = sig
	return newResult(id, p)
}
