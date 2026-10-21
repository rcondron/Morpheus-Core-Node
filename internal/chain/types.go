package chain

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Struct field order must match the ABI tuple order exactly: abi.ConvertType
// copies tuple components positionally. Extra fields go after the tuple fields.

// SD is LibSD.SD: a running mean and sum of squares.
type SD struct {
	Mean  int64
	SqSum int64
}

type Session struct {
	User                    common.Address
	BidId                   [32]byte
	Stake                   *big.Int
	CloseoutReceipt         []byte
	CloseoutType            *big.Int
	ProviderWithdrawnAmount *big.Int
	OpenedAt                *big.Int
	EndsAt                  *big.Int
	ClosedAt                *big.Int
	IsActive                bool
	IsDirectPaymentFromUser bool

	ID common.Hash
}

func (s *Session) EndsAtTime() time.Time   { return time.Unix(s.EndsAt.Int64(), 0) }
func (s *Session) OpenedAtTime() time.Time { return time.Unix(s.OpenedAt.Int64(), 0) }
func (s *Session) Closed() bool            { return s.ClosedAt != nil && s.ClosedAt.Sign() > 0 }
func (s *Session) Exists() bool {
	return s.User != (common.Address{}) || (s.OpenedAt != nil && s.OpenedAt.Sign() > 0)
}

type Bid struct {
	Provider       common.Address
	ModelId        [32]byte
	PricePerSecond *big.Int
	Nonce          *big.Int
	CreatedAt      *big.Int
	DeletedAt      *big.Int

	ID common.Hash
}

func (b *Bid) Active() bool { return b.DeletedAt == nil || b.DeletedAt.Sign() == 0 }

type Provider struct {
	Endpoint          string
	Stake             *big.Int
	CreatedAt         *big.Int
	LimitPeriodEnd    *big.Int
	LimitPeriodEarned *big.Int
	IsDeleted         bool

	Address common.Address
}

type Model struct {
	IpfsCID   [32]byte
	Fee       *big.Int
	Stake     *big.Int
	Owner     common.Address
	Name      string
	Tags      []string
	CreatedAt *big.Int
	IsDeleted bool

	ID common.Hash
}

type ModelStats struct {
	TpsScaled1000 SD
	TtftMs        SD
	TotalDuration SD
	Count         uint32
}

type ProviderModelStats struct {
	TpsScaled1000 SD
	TtftMs        SD
	TotalDuration uint32
	SuccessCount  uint32
	TotalCount    uint32
}
