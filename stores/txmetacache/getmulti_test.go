package txmetacache

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/cespare/xxhash/v2"
	"github.com/stretchr/testify/require"
)

// GetMulti must return exactly what Get returns, key by key, on every bucket
// type: it only changes how often the bucket locks are taken.
func TestImprovedCache_GetMultiMatchesGet(t *testing.T) {
	for _, bt := range []BucketType{Native, Unallocated, Preallocated, Trimmed} {
		cache, err := New(64*1024*1024, bt)
		require.NoError(t, err)

		const n = 5000

		keys := make([][]byte, n)

		for i := range keys {
			keys[i] = make([]byte, 32)
			_, _ = rand.Read(keys[i])

			if i%3 != 0 { // leave every third key missing
				require.NoError(t, cache.Set(keys[i], []byte{byte(i), byte(i >> 8), 7}))
			}
		}

		var dst []byte

		spans := make([][2]int, n)
		cache.GetMulti(keys, &dst, spans)

		for i, k := range keys {
			var want []byte
			err := cache.Get(&want, k)

			if err != nil {
				require.Equal(t, [2]int{-1, -1}, spans[i], "bucket type %v key %d should miss", bt, i)
				continue
			}

			require.NotEqual(t, -1, spans[i][0], "bucket type %v key %d should hit", bt, i)
			require.Equal(t, want, dst[spans[i][0]:spans[i][1]], "bucket type %v key %d", bt, i)
		}
	}
}

// A probe range covers a contiguous, disjoint range of buckets, so a range
// task touches each of its buckets once.
func TestProbeRangeOf_ContiguousBucketRanges(t *testing.T) {
	per := BucketsCount / ProbeRanges

	for i := 0; i < 10000; i++ {
		var h chainhash.Hash
		_, _ = rand.Read(h[:])

		bucket := int(xxhash.Sum64(h[:]) % BucketsCount)
		require.Equal(t, bucket/per, ProbeRangeOf(&h))
	}
}

// The batched lookup must agree with GetMetaCached and count hits and misses
// the same way.
func TestTxMetaCache_GetMetaCachedMultiMatchesGetMetaCached(t *testing.T) {
	c := newPerHashCache(t, Native)

	const n = 600

	inpoints := testInpoints(t)

	hashes := make([]chainhash.Hash, n)
	for i := range hashes {
		hashes[i] = hashFor(5, i)

		if i%4 == 0 {
			continue // missing
		}

		h := hashes[i]
		require.NoError(t, c.SetCache(&h, &meta.Data{Fee: feeFor(h), SizeInBytes: sizeFor(h), TxInpoints: inpoints}))
	}

	hitsBefore, missesBefore := c.metrics.hits.Load(), c.metrics.misses.Load()

	out := make([]*meta.Data, n)
	c.GetMetaCachedMulti(context.Background(), hashes, out)

	require.Equal(t, uint64(n-n/4), c.metrics.hits.Load()-hitsBefore)
	require.Equal(t, uint64(n/4), c.metrics.misses.Load()-missesBefore)

	for i, h := range hashes {
		want, ok := c.GetMetaCached(context.Background(), h)
		if !ok {
			require.Nil(t, out[i], "entry %d should miss", i)
			continue
		}

		require.NotNil(t, out[i], "entry %d should hit", i)
		require.Equal(t, want.Fee, out[i].Fee)
		require.Equal(t, want.SizeInBytes, out[i].SizeInBytes)
		require.Equal(t, want.TxInpoints.ParentTxHashes, out[i].TxInpoints.ParentTxHashes)
	}
}

// testInpoints returns a valid single-parent TxInpoints, so the entry is
// complete enough to be cached and probed.
func testInpoints(t *testing.T) subtree.TxInpoints {
	t.Helper()

	tx := bt.NewTx()
	require.NoError(t, tx.From("0000000000000000000000000000000000000000000000000000000000000001", 0,
		"76a914000000000000000000000000000000000000000088ac", 1000))

	inpoints, err := subtree.NewTxInpointsFromTx(tx)
	require.NoError(t, err)

	return inpoints
}
