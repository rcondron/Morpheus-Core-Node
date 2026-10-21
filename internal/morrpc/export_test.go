package morrpc

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// TestExportVectors writes messages built by this implementation so the
// reference proxy-router's verification code can check them (run with
// MORCORE_EXPORT=path; used for cross-implementation interop checks).
func TestExportVectors(t *testing.T) {
	out := os.Getenv("MORCORE_EXPORT")
	if out == "" {
		t.Skip("MORCORE_EXPORT not set")
	}
	user, _ := wallet.FromHex("0x1111111111111111111111111111111111111111111111111111111111111111")
	prov, _ := wallet.FromHex("0x2222222222222222222222222222222222222222222222222222222222222222")
	bid := common.HexToHash("0xabababababababababababababababababababababababababababababababab")
	sid := common.HexToHash("0xcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	chainID := big.NewInt(84532)

	v := map[string]any{}
	v["sessionRequest"], _ = SessionRequest("1", big.NewInt(42), bid, user)
	prompt, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "hi <b> & ✓"}}})
	v["promptRequest"], _ = PromptRequest("2", sid, prompt, user)
	v["reportRequest"], _ = ReportRequest("3", sid, user)
	ok := true
	v["pongResponse"], _ = PongResponse("4", []byte{1, 2, 3, 4, 5, 6, 7, 8}, "morcore-test", []ModelHealthReport{{ModelID: bid.Hex(), Status: HealthHealthy, PromptCorrect: &ok}}, prov)
	v["sessionResponse"], _ = SessionResponse("5", user.Address(), bid, chainID, prov)
	enc, _ := EncryptTo(user.PublicKey(), []byte(`{"object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`))
	v["promptResponse"], _ = PromptResponse("6", enc, prov)
	encErr, _ := EncryptTo(user.PublicKey(), []byte(`{"providerModelError":{"error":{"message":"busy"}},"statusCode":503}`))
	v["signedError"], _ = SignedError("7", encErr, prov)
	v["reportResponse"], _ = ReportResponse("8", Report{SessionID: sid, ChainID: chainID, TPSScaled1000: 1234, TTFTMs: 56, InputTokens: 7, OutputTokens: 8}, prov)
	v["plainError"] = PlainError("9", "session expired")
	v["userPub"] = HexString(user.PublicKey()).String()
	v["providerPub"] = HexString(prov.PublicKey()).String()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
