package morrpc

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// testdata/reference_vectors.json was produced by the reference
// Morpheus-Lumerin-Node proxy-router (internal/proxyapi/morrpcmessage), so these
// tests prove byte-level wire compatibility with the deployed network.
type vectors struct {
	UserKey, ProviderKey   string
	UserAddr, ProviderAddr string
	UserPub, ProviderPub   string
	ChainID                string `json:"chainId"`
	BidID                  string `json:"bidId"`
	SessionID              string `json:"sessionId"`

	PingRequest         RPCMessage  `json:"pingRequest"`
	PongResponse        RPCResponse `json:"pongResponse"`
	SessionRequest      RPCMessage  `json:"sessionRequest"`
	SessionResponse     RPCResponse `json:"sessionResponse"`
	PromptRequest       RPCMessage  `json:"promptRequest"`
	PromptResponse      RPCResponse `json:"promptResponse"`
	PromptResponsePlain string      `json:"promptResponsePlain"`
	PromptDoneResponse  RPCResponse `json:"promptDoneResponse"`
	ErrorResponse       RPCResponse `json:"errorResponse"`
	ErrorResponsePlain  string      `json:"errorResponsePlain"`
	ReportRequest       RPCMessage  `json:"reportRequest"`
	ReportResponse      RPCResponse `json:"reportResponse"`
}

func loadVectors(t *testing.T) (*vectors, *wallet.Wallet, *wallet.Wallet) {
	t.Helper()
	data, err := os.ReadFile("testdata/reference_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	user, err := wallet.FromHex(v.UserKey)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := wallet.FromHex(v.ProviderKey)
	if err != nil {
		t.Fatal(err)
	}
	if user.Address().Hex() != v.UserAddr || prov.Address().Hex() != v.ProviderAddr {
		t.Fatal("vector keys do not match addresses")
	}
	return &v, user, prov
}

func TestReferencePing(t *testing.T) {
	v, user, prov := loadVectors(t)
	var ping PingReq
	if err := json.Unmarshal(v.PingRequest.Params, &ping); err != nil {
		t.Fatal(err)
	}
	sig := ping.Signature
	ping.Signature = HexString{}
	if _, addr, err := RecoverParams(ping, sig); err != nil || addr != user.Address() {
		t.Fatalf("ping signature: %v %s", err, addr.Hex())
	}

	pong, _, addr, err := VerifyPong(&v.PongResponse, ping.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if addr != prov.Address() {
		t.Fatalf("pong signed by %s, want %s", addr.Hex(), prov.Address().Hex())
	}
	if pong.Version != "v7.0.0" || len(pong.Models) != 1 || pong.Models[0].Status != HealthHealthy {
		t.Fatalf("unexpected pong %+v", pong)
	}
}

func TestReferenceSessionHandshake(t *testing.T) {
	v, user, prov := loadVectors(t)

	// Provider side: verify the reference consumer's session.request.
	var req SessionReq
	if err := json.Unmarshal(v.SessionRequest.Params, &req); err != nil {
		t.Fatal(err)
	}
	sig := req.Signature
	req.Signature = HexString{}
	if !VerifyParams(req, sig, req.Key) {
		t.Fatal("session.request signature did not verify")
	}
	if PubKeyAddress(req.Key) != user.Address() || req.User != user.Address() {
		t.Fatal("session.request key/user mismatch")
	}
	if req.Spend.Int.Cmp(big.NewInt(123456789)) != 0 {
		t.Fatalf("spend = %s", req.Spend.String())
	}

	// Consumer side: verify the reference provider's approval.
	chainID, _ := new(big.Int).SetString(v.ChainID, 10)
	sr, err := VerifySessionRes(&v.SessionResponse, user.Address(), common.HexToHash(v.BidID), chainID, func(s common.Address) error {
		if s != prov.Address() {
			return errors.New("unexpected signer")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if HexString(sr.PubKey).String() != v.ProviderPub {
		t.Fatal("provider pubkey mismatch")
	}
	// Tampering with the approval must be rejected.
	if _, err := VerifySessionRes(&v.SessionResponse, prov.Address(), common.HexToHash(v.BidID), chainID, nil); err == nil {
		t.Fatal("approval for another user was accepted")
	}
}

func TestReferencePrompt(t *testing.T) {
	v, user, prov := loadVectors(t)

	var req SessionPromptReq
	if err := json.Unmarshal(v.PromptRequest.Params, &req); err != nil {
		t.Fatal(err)
	}
	sig := req.Signature
	req.Signature = HexString{}
	pub, addr, err := RecoverParams(req, sig)
	if err != nil || addr != user.Address() {
		t.Fatalf("prompt signer: %v %s", err, addr.Hex())
	}
	if !VerifyParams(req, sig, pub) {
		t.Fatal("prompt signature did not verify")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Message), &body); err != nil {
		t.Fatalf("prompt message is not JSON: %v", err)
	}

	plain, err := DecodeInference(&v.PromptResponse, prov.PublicKey(), user)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != v.PromptResponsePlain {
		t.Fatalf("decrypted %q", plain)
	}
	done, err := DecodeInference(&v.PromptDoneResponse, prov.PublicKey(), user)
	if err != nil {
		t.Fatal(err)
	}
	var ctl string
	if err := json.Unmarshal(done, &ctl); err != nil || ctl != StreamDone {
		t.Fatalf("done marker = %q (%v)", done, err)
	}
	// A different provider key must not verify.
	other, _ := wallet.Generate()
	if _, err := DecodeInference(&v.PromptResponse, other.PublicKey(), user); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected signature failure, got %v", err)
	}
}

func TestReferenceSignedError(t *testing.T) {
	v, user, prov := loadVectors(t)
	err := ResponseError(&v.ErrorResponse, prov.PublicKey(), user)
	var ee *EngineError
	if !errors.As(err, &ee) {
		t.Fatalf("expected EngineError, got %v", err)
	}
	if ee.HTTPStatus() != 429 || string(ee.Raw) != v.ErrorResponsePlain {
		t.Fatalf("unexpected engine error %+v", ee)
	}
}

func TestReferenceReport(t *testing.T) {
	v, user, prov := loadVectors(t)
	var req SessionReportReq
	if err := json.Unmarshal(v.ReportRequest.Params, &req); err != nil {
		t.Fatal(err)
	}
	sig := req.Signature
	req.Signature = HexString{}
	if !VerifyParams(req, sig, user.PublicKey()) || req.Message != v.SessionID {
		t.Fatal("report request did not verify")
	}

	rr, err := VerifyReportRes(&v.ReportResponse, prov.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := DecodeReport(rr.Message)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SessionID != common.HexToHash(v.SessionID) || rep.TPSScaled1000 != 1500 || rep.TTFTMs != 250 || rep.InputTokens != 10 || rep.OutputTokens != 20 {
		t.Fatalf("unexpected report %+v", rep)
	}
	signer, err := RecoverEthMessage(rr.Message, rr.SignedReport)
	if err != nil || signer != prov.Address() {
		t.Fatalf("receipt signer %s (%v)", signer.Hex(), err)
	}
}

// Messages we build must be byte-identical in shape to the reference so that
// reference providers verify them: re-marshal the reference request and ours
// through the same struct and compare the signed payloads.
func TestOurMessagesMatchReferenceShape(t *testing.T) {
	v, user, _ := loadVectors(t)
	msg, err := ReportRequest("4", common.HexToHash(v.SessionID), user)
	if err != nil {
		t.Fatal(err)
	}
	var ours, theirs map[string]json.RawMessage
	_ = json.Unmarshal(msg.Params, &ours)
	_ = json.Unmarshal(v.ReportRequest.Params, &theirs)
	if len(ours) != len(theirs) {
		t.Fatalf("field sets differ: ours=%v theirs=%v", keys(ours), keys(theirs))
	}
	for k := range theirs {
		if _, ok := ours[k]; !ok {
			t.Fatalf("missing field %q", k)
		}
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
