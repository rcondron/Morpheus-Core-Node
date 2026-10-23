package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// peer is a storage provider as the client reaches it.
type peer struct {
	provider common.Address
	endpoint string
}

func (l *Lease) peer() peer { return peer{l.Provider, l.Endpoint} }
func (c *Copy) peer() peer  { return peer{c.Provider, c.Endpoint} }

// call sends a signed storage request and decodes the provider's signed
// result into out, checking the signer may act for the provider.
func (s *Store) call(ctx context.Context, p peer, method string, req morrpc.StorageReq, out any) error {
	return s.callWait(ctx, p, method, req, 0, out)
}

// callWait is call with a longer wait for the answer (0: the client default).
func (s *Store) callWait(ctx context.Context, p peer, method string, req morrpc.StorageReq, wait time.Duration, out any) error {
	msg, err := morrpc.StorageRequest(randomHex(8), method, req, s.w)
	if err != nil {
		return err
	}
	if wait == 0 {
		wait = s.rpc.ReplyTimeout
	}
	var res *morrpc.RPCResponse
	err = s.rpc.Stream(ctx, p.endpoint, msg, wait, wait, func(r *morrpc.RPCResponse) (bool, error) {
		res = r
		return true, nil
	})
	if err == nil && res == nil {
		err = errors.New("provider closed the connection without replying")
	}
	if err != nil {
		return err
	}
	return morrpc.VerifyStorageResponse(res, func(signer common.Address) error {
		return s.leaser.Authorized(ctx, p.provider, signer)
	}, out)
}

// readRange reads stored bytes [off, off+n) of a file from a provider.
func (s *Store) readRange(ctx context.Context, p peer, cid string, off, n uint64) ([]byte, error) {
	var d morrpc.StorageData
	if err := s.call(ctx, p, morrpc.MethodStorageGet, morrpc.StorageReq{CID: cid, Offset: off, Length: n}, &d); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(d.Data)
	if err != nil || d.CID != cid || d.Offset != off || len(data) == 0 || uint64(len(data)) > n {
		return nil, errors.New("provider returned an invalid range")
	}
	return data, nil
}

// Offer is a storage product on the marketplace: Quota bytes per lease for
// Price MOR (wei) per second, sold by Provider under a listing.
type Offer struct {
	Listing  marketplace.Model `json:"listing"`
	Bid      common.Hash       `json:"bid"`
	Provider common.Address    `json:"provider"`
	Endpoint string            `json:"endpoint"`
	Quota    uint64            `json:"quotaBytes"`
	Price    *big.Int          `json:"pricePerSecond"`
}

// PerGiBMonth is what 30 days of 1 GiB cost at this offer's rate. Leases are
// bought whole: the full quota is paid for however much of it is used.
func (o Offer) PerGiBMonth() *big.Int {
	v := new(big.Int).Mul(o.Price, big.NewInt(30*24*3600))
	v.Mul(v, big.NewInt(1<<30))
	return v.Div(v, new(big.Int).SetUint64(o.Quota))
}

// LeaseCost is what one lease of d costs at this offer (paid to the provider
// from the session stake; the rest of the stake comes back).
func (o Offer) LeaseCost(d time.Duration) *big.Int {
	return new(big.Int).Mul(o.Price, big.NewInt(int64(d/time.Second)))
}

// cheaper orders offers by price per byte.
func cheaper(a, b Offer) bool {
	x := new(big.Int).Mul(a.Price, new(big.Int).SetUint64(b.Quota))
	y := new(big.Int).Mul(b.Price, new(big.Int).SetUint64(a.Quota))
	return x.Cmp(y) < 0
}

// ListOffers reads the storage offers on the marketplace, cheapest per byte
// first, leaving out the wallet's own bids.
func ListOffers(ctx context.Context, market *marketplace.Market, self common.Address) ([]Offer, error) {
	models, err := market.Models(ctx)
	if err != nil {
		return nil, err
	}
	var out []Offer
	for _, m := range models {
		quota := marketplace.StorageQuota(m.Tags)
		if quota == 0 || m.Bids == 0 {
			continue
		}
		bids, err := market.RatedBids(ctx, m.ID)
		if err != nil {
			continue
		}
		for _, b := range bids {
			if b.Bid.Provider == self || b.Provider == nil || b.Bid.PricePerSecond == nil {
				continue
			}
			out = append(out, Offer{
				Listing: m, Bid: b.Bid.ID, Provider: b.Bid.Provider, Endpoint: morrpc.NormalizeEndpoint(b.Provider.Endpoint),
				Quota: quota, Price: b.Bid.PricePerSecond,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return cheaper(out[i], out[j]) })
	return out, nil
}

// Offers lists the storage offers this store may use.
func (s *Store) Offers(ctx context.Context) ([]Offer, error) {
	all, err := ListOffers(ctx, s.market, s.w.Address())
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, o := range all {
		if s.allowed(o.Provider) {
			out = append(out, o)
		}
	}
	return out, nil
}

// Info asks a provider for its storage terms.
func (s *Store) Info(ctx context.Context, provider common.Address, endpoint string) (*morrpc.StorageInfo, error) {
	nonce := randomHex(16)
	var info morrpc.StorageInfo
	if err := s.call(ctx, peer{provider, morrpc.NormalizeEndpoint(endpoint)}, morrpc.MethodStorageInfo, morrpc.StorageReq{Nonce: nonce}, &info); err != nil {
		return nil, err
	}
	if info.Nonce != nonce {
		return nil, fmt.Errorf("provider answered a different request")
	}
	return &info, nil
}
