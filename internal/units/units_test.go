package units

import (
	"math/big"
	"testing"
)

func TestParseFormatRoundTrip(t *testing.T) {
	cases := map[string]string{
		"0":                    "0",
		"1":                    "1000000000000000000",
		"0.5":                  "500000000000000000",
		"12.345":               "12345000000000000000",
		"0.000000000000000001": "1",
		"1_000":                "1000000000000000000000",
		"":                     "0",
	}
	for in, want := range cases {
		got, err := ParseToken(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got.String() != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	for _, bad := range []string{"-1", "1.2.3", "abc", "0.0000000000000000001"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFormatToken(t *testing.T) {
	v, _ := new(big.Int).SetString("1234567890000000000000", 10)
	if got := FormatToken(v, 4); got != "1234.5678" {
		t.Fatalf("got %s", got)
	}
	if got := FormatToken(big.NewInt(1), 4); got != "<0.0001" {
		t.Fatalf("tiny amount rendered %s", got)
	}
	if got := MOR(nil); got != "0 MOR" {
		t.Fatalf("nil rendered %s", got)
	}
}
