package config

import (
	"fmt"
	"sort"
)

// Network describes a chain the Morpheus marketplace is deployed on.
type Network struct {
	Name            string
	Label           string
	ChainID         int64
	DiamondAddress  string
	MORTokenAddress string
	ExplorerURL     string
	PublicRPCs      []string
}

// Canonical deployments, from the Morpheus-Lumerin-Node release notes
// (https://nodedocs.mor.org/get-started/networks-and-tokens).
var networks = map[string]Network{
	"mainnet": {
		Name:            "mainnet",
		Label:           "BASE Mainnet",
		ChainID:         8453,
		DiamondAddress:  "0x6aBE1d282f72B474E54527D93b979A4f64d3030a",
		MORTokenAddress: "0x7431aDa8a591C955a994a21710752EF9b882b8e3",
		ExplorerURL:     "https://base.blockscout.com",
		PublicRPCs: []string{
			"https://mainnet.base.org",
			"https://base-rpc.publicnode.com",
			"https://base.drpc.org",
			"https://1rpc.io/base",
		},
	},
	"testnet": {
		Name:            "testnet",
		Label:           "BASE Sepolia (testnet)",
		ChainID:         84532,
		DiamondAddress:  "0xA328196f2438DADA5ab729E39388D86896c27c85",
		MORTokenAddress: "0x5C80Ddd187054E1E4aBBfFCD750498e81d34FfA3",
		ExplorerURL:     "https://base-sepolia.blockscout.com",
		PublicRPCs: []string{
			"https://sepolia.base.org",
			"https://base-sepolia-rpc.publicnode.com",
			"https://base-sepolia.drpc.org",
		},
	},
}

func init() {
	// Local simulated marketplace started by `morcore-devnet` (development only).
	networks["local"] = Network{
		Name:            "local",
		Label:           "Local devnet",
		ChainID:         31337,
		DiamondAddress:  "0x6aBE1d282f72B474E54527D93b979A4f64d3030a",
		MORTokenAddress: "0x7431aDa8a591C955a994a21710752EF9b882b8e3",
		PublicRPCs:      []string{"http://127.0.0.1:8545"},
	}
}

// LookupNetwork returns the named network.
func LookupNetwork(name string) (Network, error) {
	n, ok := networks[name]
	if !ok {
		return Network{}, fmt.Errorf("unknown network %q (want one of %v)", name, NetworkNames())
	}
	return n, nil
}

// NetworkNames lists the known network names in a stable order.
func NetworkNames() []string {
	names := make([]string, 0, len(networks))
	for k := range networks {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
