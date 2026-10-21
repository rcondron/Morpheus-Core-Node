package chain

import (
	_ "embed"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Minimal ABIs extracted from the deployed Diamond facets, the MOR ERC-20 and
// Multicall3, limited to what the node calls.

//go:embed abi/diamond.json
var diamondJSON string

//go:embed abi/erc20.json
var erc20JSON string

//go:embed abi/multicall3.json
var multicallJSON string

var (
	DiamondABI   = mustABI(diamondJSON)
	ERC20ABI     = mustABI(erc20JSON)
	MulticallABI = mustABI(multicallJSON)
)

// Multicall3Address is the canonical cross-chain Multicall3 deployment.
var Multicall3Address = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")

func mustABI(js string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(js))
	if err != nil {
		panic(err)
	}
	return a
}
