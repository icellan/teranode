package txmetacache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/require"
)

// A second Set for a key the native bucket already maps is dropped by the map
// (NativeSplitLockFreeMapUint64 cannot overwrite), so it must not spend ring
// space or count as a new entry either. Under a miss burst nearly every Kafka
// txmeta insert is such a duplicate of the Aerospike write-back.
func TestBucketNative_DuplicateSetDoesNotConsumeRing(t *testing.T) {
	cache, err := New(128*1024*1024, Native)
	require.NoError(t, err)

	key := []byte("0123456789abcdef0123456789abcdef")
	h := xxhash.Sum64(key)
	b := cache.buckets[h%BucketsCount].(*bucketNative)

	require.NoError(t, cache.Set(key, []byte("first")))

	idxAfterFirst := b.idx
	countAfterFirst := b.currentGenCount

	require.NoError(t, cache.Set(key, []byte("second-value")))

	require.Equal(t, idxAfterFirst, b.idx, "duplicate Set must not append to the ring")
	require.Equal(t, countAfterFirst, b.currentGenCount, "duplicate Set must not count as a new entry")

	var dst []byte
	require.NoError(t, cache.Get(&dst, key))
	require.Equal(t, []byte("first"), dst, "the first value stays visible, as before")
}

// recordingBackend records which batch insert BatchDecorate's write-back uses.
type recordingBackend struct {
	cacheBackend
	fanOutCalls     int
	sequentialCalls int
}

func (r *recordingBackend) SetMultiFromBytes(keys, values [][]byte) error {
	r.fanOutCalls++
	return r.cacheBackend.SetMultiFromBytes(keys, values)
}

func (r *recordingBackend) SetMultiSequential(keys, values [][]byte) error {
	r.sequentialCalls++
	return r.cacheBackend.SetMultiSequential(keys, values)
}

// The Aerospike write-back runs on the subtree-validation worker that already
// holds the batch. ImprovedCache.SetMulti's per-bucket goroutine fan-out put
// ~960 goroutines per batch into the scheduler and onto the bucket locks the
// Kafka txmeta handler needs, starving ingestion.
func TestTxMetaCache_BatchDecorateWriteBackDoesNotFanOut(t *testing.T) {
	c := newPerHashCache(t, Native)
	rec := &recordingBackend{cacheBackend: c.cache}
	c.cache = rec

	const n = 1024 // well above smallSetMultiBatchThreshold, so SetMulti would fan out

	items := make([]*utxo.UnresolvedMetaData, n)
	for i := range items {
		items[i] = &utxo.UnresolvedMetaData{Hash: hashFor(1, i), Idx: i}
	}

	require.NoError(t, c.BatchDecorate(context.Background(), items))

	require.Equal(t, 0, rec.fanOutCalls, "write-back must not use the goroutine fan-out path")
	require.Positive(t, rec.sequentialCalls)

	for i := 0; i < n; i++ {
		h := hashFor(1, i)
		d, ok := c.GetMetaCached(context.Background(), h)
		require.True(t, ok, "entry %d must be cached", i)
		require.Equal(t, feeFor(h), d.Fee)
		require.Equal(t, sizeFor(h), d.SizeInBytes)
	}

}

// laneBackend tracks, per write-back lane, how many writes are inside the cache
// at once.
type laneBackend struct {
	cacheBackend
	mu       sync.Mutex
	inFlight map[int]int
	peak     map[int]int
	calls    atomic.Int32
}

func (c *laneBackend) SetMultiSequential(keys, values [][]byte) error {
	lane := writeBackLaneOf(keys[0])
	for _, k := range keys {
		if writeBackLaneOf(k) != lane {
			panic("a write-back batch must stay inside one lane")
		}
	}

	c.calls.Add(1)

	c.mu.Lock()
	c.inFlight[lane]++
	c.peak[lane] = max(c.peak[lane], c.inFlight[lane])
	c.mu.Unlock()

	time.Sleep(time.Millisecond) // widen the window so overlapping writers would show

	err := c.cacheBackend.SetMultiSequential(keys, values)

	c.mu.Lock()
	c.inFlight[lane]--
	c.mu.Unlock()

	return err
}

// Like the Kafka txmeta partitions, each write-back lane owns a contiguous
// bucket range and has at most one writer at a time, which merges whatever the
// concurrent BatchDecorate callers queued for that lane. Thousands of
// concurrent write-backs therefore never become thousands of concurrent
// writers on the bucket locks the Kafka handler needs.
func TestTxMetaCache_BatchDecorateWriteBackOneWriterPerLane(t *testing.T) {
	c := newPerHashCache(t, Native)
	rec := &laneBackend{cacheBackend: c.cache, inFlight: map[int]int{}, peak: map[int]int{}}
	c.cache = rec

	const callers = 64

	var wg sync.WaitGroup

	for g := 0; g < callers; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			items := make([]*utxo.UnresolvedMetaData, 300)
			for i := range items {
				items[i] = &utxo.UnresolvedMetaData{Hash: hashFor(g+10, i), Idx: i}
			}

			require.NoError(t, c.BatchDecorate(context.Background(), items))

			// Populated by the time BatchDecorate returns, whoever wrote it.
			for i := range items {
				_, ok := c.GetMetaCached(context.Background(), hashFor(g+10, i))
				require.True(t, ok)
			}
		}(g)
	}

	wg.Wait()

	require.NotEmpty(t, rec.peak)

	for lane, p := range rec.peak {
		require.Equal(t, 1, p, "lane %d had concurrent writers", lane)
	}

	// Concurrent callers' entries are merged into shared per-lane writes.
	require.Less(t, int(rec.calls.Load()), callers*writeBackLanes)
}
