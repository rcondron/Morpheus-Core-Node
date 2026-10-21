// Package units converts between 18-decimal token amounts (wei) and decimal strings.
package units

import (
	"fmt"
	"math/big"
	"strings"
)

const decimals = 18

var weiPerToken = new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)

// ParseToken parses a decimal token amount such as "12.5" into wei.
func ParseToken(s string) (*big.Int, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "_", ""))
	if s == "" {
		return new(big.Int), nil
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		return nil, fmt.Errorf("negative amount %q", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > decimals {
		return nil, fmt.Errorf("amount %q has more than %d decimals", s, decimals)
	}
	frac += strings.Repeat("0", decimals-len(frac))
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount %q", s)
	}
	return v, nil
}

// FormatToken renders wei as a decimal with at most maxFrac fractional digits
// (trailing zeros trimmed).
func FormatToken(wei *big.Int, maxFrac int) string {
	if wei == nil {
		return "0"
	}
	sign := ""
	v := new(big.Int).Set(wei)
	if v.Sign() < 0 {
		sign = "-"
		v.Neg(v)
	}
	whole, rem := new(big.Int).QuoRem(v, weiPerToken, new(big.Int))
	if maxFrac <= 0 || rem.Sign() == 0 {
		return sign + whole.String()
	}
	frac := fmt.Sprintf("%018s", rem.String())
	if maxFrac < decimals {
		frac = frac[:maxFrac]
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		if whole.Sign() == 0 && rem.Sign() > 0 {
			return sign + "<0." + strings.Repeat("0", maxFrac-1) + "1"
		}
		return sign + whole.String()
	}
	return sign + whole.String() + "." + frac
}

// MOR formats wei as a human MOR amount.
func MOR(wei *big.Int) string { return FormatToken(wei, 4) + " MOR" }

// ETH formats wei as a human ETH amount.
func ETH(wei *big.Int) string { return FormatToken(wei, 6) + " ETH" }
