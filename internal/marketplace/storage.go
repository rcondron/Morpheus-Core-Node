package marketplace

import (
	"strings"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
)

// Storage listings are marketplace models that sell IPFS storage instead of
// inference: tagged "storage", with a "quota:<size>" tag giving the bytes one
// session covers (e.g. "quota:10GiB"). Bids price a quota per second.
const (
	StorageTag     = "storage"
	quotaTagPrefix = "quota:"
)

// StorageQuota returns the per-session quota of a storage listing, or 0 when
// the tags do not describe one.
func StorageQuota(tags []string) uint64 {
	storage := false
	var quota uint64
	for _, t := range tags {
		t = strings.TrimSpace(t)
		switch {
		case strings.EqualFold(t, StorageTag):
			storage = true
		case len(t) > len(quotaTagPrefix) && strings.EqualFold(t[:len(quotaTagPrefix)], quotaTagPrefix):
			quota, _ = config.ParseBytes(t[len(quotaTagPrefix):])
		}
	}
	if !storage {
		return 0
	}
	return quota
}

// IsStorage reports whether the model is a storage listing.
func (m *Model) IsStorage() bool { return IsStorageTags(m.Tags) }

// IsStorageTags reports whether model tags mark a storage listing.
func IsStorageTags(tags []string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), StorageTag) {
			return true
		}
	}
	return false
}

// StorageTags are the tags for a new storage listing of quota bytes.
func StorageTags(quota uint64) []string {
	return []string{StorageTag, "ipfs", quotaTagPrefix + strings.ReplaceAll(config.FormatBytes(quota), " ", "")}
}
