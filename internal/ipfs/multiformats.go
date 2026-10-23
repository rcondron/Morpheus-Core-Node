package ipfs

import (
	"encoding/binary"
	"errors"
)

var (
	errVarint        = errors.New("truncated or overflowing varint")
	errVarintMinimal = errors.New("varint not minimally encoded")
)

// uvarint decodes an unsigned LEB128 varint (multiformats and protobuf use
// the same encoding) and rejects padded encodings such as 0x80 0x00, which
// would give one value several byte representations.
func uvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, errVarint
	}
	if n > 1 && b[n-1] == 0 {
		return 0, 0, errVarintMinimal
	}
	return v, n, nil
}

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var b58Index = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(b58Alphabet); i++ {
		t[b58Alphabet[i]] = int8(i)
	}
	return t
}()

// base58Encode is base58btc (the Bitcoin alphabet), used by CIDv0. It is
// quadratic, which is fine for 34-byte multihashes.
func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	var digits []byte // base 58, least significant first
	for _, v := range b[zeros:] {
		carry := int(v)
		for i := range digits {
			carry += int(digits[i]) << 8
			digits[i] = byte(carry % 58)
			carry /= 58
		}
		for ; carry > 0; carry /= 58 {
			digits = append(digits, byte(carry%58))
		}
	}
	out := make([]byte, zeros+len(digits))
	for i := 0; i < zeros; i++ {
		out[i] = '1'
	}
	for i, d := range digits {
		out[len(out)-1-i] = b58Alphabet[d]
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	var num []byte // base 256, least significant first
	for i := zeros; i < len(s); i++ {
		d := b58Index[s[i]]
		if d < 0 {
			return nil, errors.New("invalid base58 character")
		}
		carry := int(d)
		for j := range num {
			carry += int(num[j]) * 58
			num[j] = byte(carry)
			carry >>= 8
		}
		for ; carry > 0; carry >>= 8 {
			num = append(num, byte(carry))
		}
	}
	out := make([]byte, zeros+len(num))
	for i, v := range num {
		out[len(out)-1-i] = v
	}
	return out, nil
}
