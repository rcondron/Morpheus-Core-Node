package storage

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestRenewalPriceRules(t *testing.T) {
	gib := uint64(1 << 30)
	last := &Lease{Price: big.NewInt(1000), Quota: 10 * gib}
	at := func(price int64, quota uint64, provider string) Offer {
		return Offer{Price: big.NewInt(price), Quota: quota, Provider: common.HexToAddress(provider)}
	}
	cases := []struct {
		name string
		o    Offer
		jump bool
	}{
		{"same price", at(1000, 10*gib, "0x1"), false},
		{"25% dearer", at(1250, 10*gib, "0x1"), false},
		{"30% dearer", at(1300, 10*gib, "0x1"), true},
		{"same price for half the quota", at(1000, 5*gib, "0x1"), true},
		{"double price for double the quota", at(2000, 20*gib, "0x1"), false},
	}
	for _, c := range cases {
		if got := jumped(c.o, last); got != c.jump {
			t.Errorf("%s: jumped = %v", c.name, got)
		}
	}
	dear := at(3000, 10*gib, "0x1")
	offers := []Offer{at(1000, 10*gib, "0x1"), at(2000, 10*gib, "0x2"), at(9000, 10*gib, "0x3")}
	if alt, ok := cheaperElsewhere(offers, common.HexToAddress("0x1"), dear); !ok || alt.Provider != common.HexToAddress("0x2") {
		t.Fatalf("cheaper elsewhere: %+v %v", alt, ok)
	}
	if _, ok := cheaperElsewhere(offers[2:], common.HexToAddress("0x1"), dear); ok {
		t.Fatal("found a cheaper offer where there is none")
	}
	if got := (Offer{Price: big.NewInt(1), Quota: gib}).PerGiBMonth(); got.Int64() != 30*24*3600 {
		t.Fatalf("per GiB-month = %s", got)
	}
}
