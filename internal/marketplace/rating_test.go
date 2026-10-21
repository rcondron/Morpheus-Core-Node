package marketplace

import (
	"math/big"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
)

func TestSessionStakeMatchesReference(t *testing.T) {
	// ceil(supply * price * (duration + 1) / budget)
	price := big.NewInt(1_000_000_000_000_000) // 0.001 MOR/s
	supply := big.NewInt(3_000)
	budget := big.NewInt(7)
	got := SessionStake(price, 3600, supply, budget)
	num := new(big.Int).Mul(supply, new(big.Int).Mul(price, big.NewInt(3601)))
	want := new(big.Int).Div(new(big.Int).Add(num, big.NewInt(6)), budget)
	if got.Cmp(want) != 0 {
		t.Fatalf("got %s want %s", got, want)
	}
	// The contract must grant at least the requested duration for that stake.
	if d := SessionDuration(got, price, supply, budget); d < 3600 {
		t.Fatalf("stake buys only %ds", d)
	}
	if SessionStake(price, 60, supply, big.NewInt(0)) != nil {
		t.Fatal("zero budget must not price a session")
	}
}

func TestScorePrefersCheaperAndReliable(t *testing.T) {
	minStake := big.NewInt(200)
	base := ScoreInput{
		ProviderModel: &chain.ProviderModelStats{SuccessCount: 9, TotalCount: 10},
		Model:         &chain.ModelStats{},
		ProviderStake: big.NewInt(200), MinStake: minStake,
		PricePerSecond: big.NewInt(1e15),
	}
	cheap := base
	cheap.PricePerSecond = big.NewInt(5e14)
	if Score(DefaultWeights, cheap) <= Score(DefaultWeights, base) {
		t.Fatal("cheaper bid did not score higher")
	}
	flaky := base
	flaky.ProviderModel = &chain.ProviderModelStats{SuccessCount: 1, TotalCount: 10}
	if Score(DefaultWeights, flaky) >= Score(DefaultWeights, base) {
		t.Fatal("unreliable provider did not score lower")
	}
	staked := base
	staked.ProviderStake = big.NewInt(2000)
	if Score(DefaultWeights, staked) <= Score(DefaultWeights, base) {
		t.Fatal("higher stake did not score higher")
	}
	// Weights sum to one, like the reference rating config requires.
	w := DefaultWeights
	if s := w.TPS + w.TTFT + w.Duration + w.Success + w.Stake; s < 0.999 || s > 1.001 {
		t.Fatalf("weights sum to %f", s)
	}
}

func TestAssignAliases(t *testing.T) {
	models := []Model{
		{Name: "llama", Bids: 1, ID: [32]byte{1}},
		{Name: "Llama", Bids: 5, ID: [32]byte{2}},
		{Name: "qwen", Bids: 2, ID: [32]byte{3}},
	}
	assignAliases(models)
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m.Alias] {
			t.Fatalf("duplicate alias %s", m.Alias)
		}
		seen[m.Alias] = true
	}
	for _, m := range models {
		if m.ID == ([32]byte{2}) && m.Alias != "Llama" {
			t.Fatalf("best supplied duplicate should keep the bare name, got %s", m.Alias)
		}
	}
}

func TestStakeScoreWithRealisticAmounts(t *testing.T) {
	mor := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	at := func(stake int64) float64 {
		// Only the stake term counts; a price of 1 MOR/s leaves it unscaled.
		return Score(Weights{Stake: 1}, ScoreInput{ProviderStake: mor(stake), MinStake: mor(10), PricePerSecond: mor(1)})
	}
	if got := at(10); got != 0 {
		t.Fatalf("stake at the minimum scored %v, want 0", got)
	}
	if got := at(55); got < 0.4999 || got > 0.5001 {
		t.Fatalf("stake at 5.5x the minimum scored %v, want 0.5", got)
	}
	if got := at(1000); got != 1 {
		t.Fatalf("stake far above 10x the minimum scored %v, want 1", got)
	}
}
