package marketplace

import (
	"math"
	"math/big"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
)

// Weights for the default bid scorer. They mirror the reference proxy-router's
// rating config so this node ranks providers the same way the network does.
type Weights struct {
	TPS      float64
	TTFT     float64
	Duration float64
	Success  float64
	Stake    float64
}

// DefaultWeights is the reference rating-config default.
var DefaultWeights = Weights{TPS: 0.24, TTFT: 0.08, Duration: 0.24, Success: 0.32, Stake: 0.12}

// ScoreInput is everything the scorer looks at for one bid.
type ScoreInput struct {
	ProviderModel  *chain.ProviderModelStats
	Model          *chain.ModelStats
	ProviderStake  *big.Int
	PricePerSecond *big.Int
	MinStake       *big.Int
}

// Score rates a bid: quality (z-scores of throughput, latency and uptime versus
// other providers of the model, success ratio and stake) divided by price.
func Score(w Weights, in ScoreInput) float64 {
	pm, m := in.ProviderModel, in.Model
	if pm == nil {
		pm = &chain.ProviderModelStats{}
	}
	if m == nil {
		m = &chain.ModelStats{}
	}
	count := int64(m.Count)
	tps := normRange(normZ(pm.TpsScaled1000.Mean, m.TpsScaled1000, count), 3)
	ttft := normRange(-normZ(pm.TtftMs.Mean, m.TtftMs, count), 3)
	dur := normRange(normZ(int64(pm.TotalDuration), m.TotalDuration, count), 3)
	success := math.Pow(ratio(pm.SuccessCount, pm.TotalCount), 2)
	stake := 0.0
	if in.ProviderStake != nil && in.MinStake != nil {
		stake = normMinMax(in.ProviderStake.Int64(), in.MinStake.Int64(), 10*in.MinStake.Int64())
	}
	total := w.TPS*tps + w.TTFT*ttft + w.Duration*dur + w.Success*success + w.Stake*stake

	price, _ := new(big.Float).SetInt(in.PricePerSecond).Float64()
	price /= 1e18
	if price <= 0 {
		return math.Inf(1)
	}
	return total / price
}

func ratio(num, den uint32) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func normZ(mean int64, sd chain.SD, n int64) float64 {
	if n <= 1 {
		return 0
	}
	v := float64(sd.SqSum) / float64(n-1)
	if v <= 0 {
		return 0
	}
	return float64(mean-sd.Mean) / math.Sqrt(v)
}

func normRange(x, r float64) float64 {
	v := (x + r) / (2 * r)
	return math.Max(0, math.Min(1, v))
}

func normMinMax(v, lo, hi int64) float64 {
	if hi == lo || v < lo {
		return 0
	}
	if v > hi {
		return 1
	}
	return float64(v-lo) / float64(hi-lo)
}

// SessionStake is the MOR (wei) to escrow for a session of durationSec at
// pricePerSecond: ceil(supply × price × (duration + 1) / budget), the
// reference node's formula (one second of headroom, rounded up so the
// contract's own rounding never yields a shorter session).
func SessionStake(pricePerSecond *big.Int, durationSec int64, supply, budget *big.Int) *big.Int {
	if budget == nil || budget.Sign() <= 0 || supply == nil || pricePerSecond == nil {
		return nil
	}
	cost := new(big.Int).Mul(pricePerSecond, big.NewInt(durationSec+1))
	amt := new(big.Int).Mul(supply, cost)
	amt.Add(amt, new(big.Int).Sub(budget, big.NewInt(1)))
	return amt.Div(amt, budget)
}

// SessionDuration is the inverse: how long stake buys at pricePerSecond.
func SessionDuration(stake, pricePerSecond, supply, budget *big.Int) int64 {
	if supply == nil || supply.Sign() <= 0 || pricePerSecond == nil || pricePerSecond.Sign() <= 0 || budget == nil {
		return 0
	}
	stipend := new(big.Int).Mul(stake, budget)
	stipend.Div(stipend, supply)
	return new(big.Int).Div(stipend, pricePerSecond).Int64()
}
