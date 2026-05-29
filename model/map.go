package model

import (
	"sync"

	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/dolthub/swiss"
)

// ParentSpendsMap is the interface for tracking spent inpoints during block validation.
// Both SplitSyncedParentMap (in-memory) and DiskParentSpendsMap (disk-backed) implement this.
type ParentSpendsMap interface {
	// SetIfNotExists records the inpoint. inserted=true means newly recorded;
	// inserted=false means it was already present (a duplicate spend). A non-nil
	// error indicates a storage/capacity failure and MUST halt validation — the
	// caller must not treat an error as either "new" or "duplicate".
	SetIfNotExists(inpoint subtreepkg.Inpoint) (inserted bool, err error)
}

type swissInpointBucket struct {
	mu sync.Mutex
	m  *swiss.Map[subtreepkg.Inpoint, struct{}]
}

type SplitSyncedParentMap struct {
	buckets     []swissInpointBucket
	nrOfBuckets uint16
}

func NewSplitSyncedParentMap(nrOfBuckets uint16, expectedInpoints ...uint64) *SplitSyncedParentMap {
	perBucket := uint32(0)
	if len(expectedInpoints) > 0 && expectedInpoints[0] > 0 {
		perBucket = uint32(expectedInpoints[0] / uint64(nrOfBuckets))
	}
	s := &SplitSyncedParentMap{
		buckets:     make([]swissInpointBucket, nrOfBuckets),
		nrOfBuckets: nrOfBuckets,
	}
	for i := uint16(0); i < nrOfBuckets; i++ {
		s.buckets[i].m = swiss.NewMap[subtreepkg.Inpoint, struct{}](perBucket)
	}
	return s
}

func (s *SplitSyncedParentMap) SetIfNotExists(inpoint subtreepkg.Inpoint) (bool, error) {
	idx := txmap.Bytes2Uint16Buckets(inpoint.Hash, s.nrOfBuckets)
	b := &s.buckets[idx]

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.m.Has(inpoint) {
		return false, nil
	}

	b.m.Put(inpoint, struct{}{})

	return true, nil
}
