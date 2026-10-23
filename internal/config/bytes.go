package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var byteUnits = []struct {
	suffix string
	mult   uint64
}{
	{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
	{"tb", 1e12}, {"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3}, {"b", 1},
}

// ParseBytes parses sizes like "4GiB", "500MB" or "1048576" ("" and "0" are 0).
func ParseBytes(s string) (uint64, error) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, " ", "")))
	if s == "" {
		return 0, nil
	}
	mult := uint64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if v*float64(mult) > 1<<62 {
		return 0, errors.New("size is too large")
	}
	return uint64(v * float64(mult)), nil
}

// FormatBytes renders a size with binary units, e.g. "1.5 GiB".
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 10 || v == float64(int64(v)) {
		return fmt.Sprintf("%.0f %ciB", v, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTPE"[exp])
}
