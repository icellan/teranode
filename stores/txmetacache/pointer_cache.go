package txmetacache

import (
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/cespare/xxhash/v2"
	"golang.org/x/sync/errgroup"
)

// pointerCacheAvgEntryBytes is the heap-cost estimate per cached entry used to
// translate the configured MB budget into a fixed per-bucket capacity. It must
// account for the metadata-only meta.Data struct, average TxInpoints payload,
// map cell + ring slot overhead, plus the [32]byte key.
//
// With chainhash.Hash keys the per-cell overhead is ~24 bytes higher than the
// previous uint64-keyed layout; the constant is calibrated against that.
// Phase B benchmarks confirmed entries fit at ~280-320 bytes each; 320 leaves
// modest headroom for parent-hash-heavy transactions.
const pointerCacheAvgEntryBytes = 320

// Compile-time check that *PointerCache satisfies the cacheBackend contract.
var _ cacheBackend = (*PointerCache)(nil)

// pointerBucket holds one shard of the PointerCache: a Go map of pointer-valued
// entries plus a fixed FIFO ring of inserted keys for eviction.
//
// Map keys are the full 32-byte transaction hash (chainhash.Hash), not a
// truncated hash. Bucket selection uses xxhash.Sum64(key) % BucketsCount for
// speed; the map lookup itself uses the full hash to eliminate the 2⁻⁶⁴
// collision risk a uint64 key would introduce.
type pointerBucket struct {
	mu      sync.RWMutex
	m       map[chainhash.Hash]*meta.Data
	ring    []chainhash.Hash
	head    int
	full    bool
	added   atomic.Uint64
	evicted atomic.Uint64
}

// PointerCache is a sharded, FIFO-evicting cache that stores *meta.Data
// pointers directly — no serialization on write, no deserialization on read.
// Trade-off: every live entry is a reachable Go heap allocation, scanned by GC.
//
// Sharding mirrors ImprovedCache (BucketsCount buckets, xxhash-based bucket
// selection). Each bucket has its own RWMutex; the ring is per-bucket so
// eviction is strictly per-shard FIFO (not global FIFO).
//
// Set replaces an existing entry in place (no new ring slot); inserting a
// new key when the ring is full evicts the oldest key in that bucket.
//
// Caller contract: the *meta.Data handed out by Get is shared with every
// concurrent reader and with the cache itself — callers must treat it as
// read-only. Set defensively clones a metadata-only snapshot, so passing a
// struct with Tx (or other non-cached fields) populated does not retain
// those fields in the cache.
type PointerCache struct {
	buckets [BucketsCount]*pointerBucket
}

// NewPointerCache constructs a PointerCache sized to fit roughly maxBytes of
// live entries, distributed evenly across BucketsCount shards. The capacity
// per shard is fixed at construction (FIFO ring is preallocated).
func NewPointerCache(maxBytes int) (*PointerCache, error) {
	if maxBytes <= 0 {
		return nil, errors.NewServiceError("maxBytes must be greater than 0; got %d", maxBytes)
	}

	perBucketEntries := maxBytes / BucketsCount / pointerCacheAvgEntryBytes
	if perBucketEntries < 1 {
		perBucketEntries = 1
	}

	var c PointerCache
	for i := range c.buckets {
		c.buckets[i] = &pointerBucket{
			m:    make(map[chainhash.Hash]*meta.Data, perBucketEntries),
			ring: make([]chainhash.Hash, perBucketEntries),
		}
	}

	return &c, nil
}

func (c *PointerCache) bucketFor(hash chainhash.Hash) *pointerBucket {
	return c.buckets[xxhash.Sum64(hash[:])%BucketsCount]
}

// insert is the shared insertion path used by Set (pointer-native) and
// SetFromBytes (byte adapter). Caller has already prepared the stripped
// metadata-only *meta.Data.
//
// Stale ring slots: Del removes from the map but leaves the slot's old key
// in the ring. When the ring is full we walk past stale entries (keys no
// longer in the map) until we find a live one to evict, or until we've
// walked the whole ring. This keeps Del cheap while preventing stale slots
// from silently reducing effective capacity through Set→Del→Set cycles.
func (c *PointerCache) insert(hash chainhash.Hash, data *meta.Data) {
	b := c.bucketFor(hash)

	b.mu.Lock()

	if _, exists := b.m[hash]; exists {
		b.m[hash] = data
		b.mu.Unlock()
		return
	}

	if b.full {
		ringSize := len(b.ring)
		for skipped := 0; skipped < ringSize; skipped++ {
			evict := b.ring[b.head]
			if _, exists := b.m[evict]; exists {
				delete(b.m, evict)
				b.evicted.Add(1)
				break
			}

			b.head++
			if b.head >= ringSize {
				b.head = 0
			}
		}
		// If every ring slot was stale, no eviction happened — fall through
		// and write below; the new key fills the slot at b.head naturally.
	}

	b.ring[b.head] = hash
	b.head++

	if b.head >= len(b.ring) {
		b.head = 0
		b.full = true
	}

	b.m[hash] = data
	b.added.Add(1)
	b.mu.Unlock()
}

// metadataOnly returns a fresh *meta.Data populated with only the fields the
// byte backend would have round-tripped via MetaBytes — explicitly excluding
// Tx, BlockIDs, BlockHeights, SubtreeIdxs, ConflictingChildren, SpendingDatas,
// and timestamps. Without this strip, pointer mode would retain full
// transactions in the cache and diverge in semantics from the byte path.
//
// TxInpoints is deep-cloned via Serialize + NewTxInpointsFromBytes — a
// struct copy alone would alias the caller's ParentTxHashes / voutIdxs
// backing arrays, which the cache hands out concurrently to every reader.
// A caller doing `append(ParentTxHashes, …)` could otherwise corrupt the
// cached entry observed by other readers. The round-trip is the only public
// path through subtree's API that produces fresh backing slices (voutIdxs
// is unexported).
//
// On the rare serialize/deserialize failure the original TxInpoints is
// reused as a shallow copy — preferable to dropping the entry entirely.
func metadataOnly(data *meta.Data) *meta.Data {
	cloned := &meta.Data{
		Fee:         data.Fee,
		SizeInBytes: data.SizeInBytes,
		IsCoinbase:  data.IsCoinbase,
		Frozen:      data.Frozen,
		Conflicting: data.Conflicting,
		Locked:      data.Locked,
		TxInpoints:  data.TxInpoints,
	}

	if len(data.TxInpoints.ParentTxHashes) == 0 {
		return cloned
	}

	bts, err := data.TxInpoints.Serialize()
	if err != nil {
		return cloned
	}

	fresh, err := subtree.NewTxInpointsFromBytes(bts)
	if err != nil {
		return cloned
	}

	cloned.TxInpoints = fresh

	return cloned
}

// Set is the pointer-native insert. The caller's *meta.Data may carry fields
// the byte backend strips (Tx, BlockIDs, ...); Set clones into a
// metadata-only snapshot before storing so the cache contract is uniform
// across backends and full transactions are never retained.
// Implements cacheBackend.
func (c *PointerCache) Set(hash *chainhash.Hash, data *meta.Data) error {
	c.insert(*hash, metadataOnly(data))
	return nil
}

// Get returns the cached *meta.Data for hash, or (nil, false) if absent.
// The returned pointer is shared with every other concurrent reader and the
// cache itself — callers must treat it as read-only.
// Implements cacheBackend.
func (c *PointerCache) Get(hash chainhash.Hash) (*meta.Data, bool) {
	b := c.bucketFor(hash)

	b.mu.RLock()
	d, ok := b.m[hash]
	b.mu.RUnlock()

	return d, ok
}

// SetFromBytes is the byte-oriented bridge: deserialises value into a fresh
// *meta.Data and inserts it. Implements cacheBackend.
//
// The deserialiser already produces a metadata-only struct (NewMetaDataFrom-
// Bytes only writes the fields the wire format carries), so no extra
// stripping is required.
func (c *PointerCache) SetFromBytes(key, value []byte) error {
	hash, err := chainhash.NewHash(key)
	if err != nil {
		return errors.NewProcessingError("invalid cache key", err)
	}

	data := &meta.Data{}
	if err := meta.NewMetaDataFromBytes(value, data); err != nil {
		return errors.NewProcessingError("failed to unmarshal txMeta bytes for pointer cache", err)
	}

	c.insert(*hash, data)

	return nil
}

// SetMultiFromBytes applies SetFromBytes to each (key, value) pair, fanning
// out across shards so a single Kafka batch doesn't serialise on one
// goroutine. Mirrors ImprovedCache.SetMulti's pattern: bucket the inputs by
// xxhash, then run one goroutine per non-empty bucket.
// Implements cacheBackend.
func (c *PointerCache) SetMultiFromBytes(keys, values [][]byte) error {
	if len(keys) != len(values) {
		return errors.NewProcessingError("keys and values length mismatch; got %d keys and %d values", len(keys), len(values))
	}

	if len(keys) == 0 {
		return nil
	}

	// Small batches: skip the bucketing overhead.
	if len(keys) <= smallSetMultiBatchThreshold {
		for i := range keys {
			if err := c.SetFromBytes(keys[i], values[i]); err != nil {
				return err
			}
		}

		return nil
	}

	batchedKeys := make([][][]byte, BucketsCount)
	batchedValues := make([][][]byte, BucketsCount)

	for i, key := range keys {
		idx := xxhash.Sum64(key) % BucketsCount
		batchedKeys[idx] = append(batchedKeys[idx], key)
		batchedValues[idx] = append(batchedValues[idx], values[i])
	}

	g := errgroup.Group{}

	for idx := range batchedKeys {
		if len(batchedKeys[idx]) == 0 {
			continue
		}

		idx := idx

		g.Go(func() error {
			for i, k := range batchedKeys[idx] {
				if err := c.SetFromBytes(k, batchedValues[idx][i]); err != nil {
					return err
				}
			}

			return nil
		})
	}

	return g.Wait()
}

// SetMultiSequential is identical to SetMultiFromBytes for the pointer
// backend: there is no per-bucket goroutine fan-out to skip, so the
// "sequential" and "default" forms share an implementation.
// Implements cacheBackend.
func (c *PointerCache) SetMultiSequential(keys, values [][]byte) error {
	return c.SetMultiFromBytes(keys, values)
}

// SetMultiSequentialWithHashes ignores the supplied xxhash values — the
// pointer backend keys by the full chainhash.Hash, not xxhash, so a
// caller-supplied uint64 hash carries no information the backend can use.
// Falls through to SetMultiFromBytes.
// Implements cacheBackend.
func (c *PointerCache) SetMultiSequentialWithHashes(keys, values [][]byte, _ []uint64) error {
	return c.SetMultiFromBytes(keys, values)
}

// Del removes the entry under key. The ring slot is not reclaimed eagerly —
// it will be passed over on its next eviction turn (bounded by ring size).
// Implements cacheBackend.
func (c *PointerCache) Del(key []byte) {
	hash, err := chainhash.NewHash(key)
	if err != nil {
		return
	}

	b := c.bucketFor(*hash)

	b.mu.Lock()
	delete(b.m, *hash)
	b.mu.Unlock()
}

// Reset clears every bucket and resets the FIFO state.
func (c *PointerCache) Reset() {
	for _, b := range c.buckets {
		b.mu.Lock()

		ringLen := len(b.ring)
		b.m = make(map[chainhash.Hash]*meta.Data, ringLen)

		for i := range b.ring {
			b.ring[i] = chainhash.Hash{}
		}

		b.head = 0
		b.full = false
		b.mu.Unlock()
	}
}

// UpdateStats populates s with current PointerCache totals so it can be
// reported through the existing txmetacache Prometheus pipeline. Fields that
// do not map cleanly onto FIFO-ring semantics (TrimCount, PreviousGenEntries)
// are left untouched.
// Implements cacheBackend.
func (c *PointerCache) UpdateStats(s *Stats) {
	var entries, totalAdded uint64

	for _, b := range c.buckets {
		b.mu.RLock()
		entries += uint64(len(b.m))
		b.mu.RUnlock()
		totalAdded += b.added.Load()
	}

	s.ValidEntriesCount += entries
	s.TotalMapSize += entries
	s.TotalElementsAdded += totalAdded
	s.CurrentGenEntries += entries
}
