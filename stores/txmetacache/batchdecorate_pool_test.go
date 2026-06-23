package txmetacache

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/nullstore"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// perHashDecoratingStore decorates each item with metadata DERIVED FROM ITS HASH
// (fee and size are functions of the hash). This makes the pooled MetaBytes
// buffers in BatchDecorate observable: if a recycled buffer ever leaked one tx's
// bytes into another's cache entry, the round-tripped fee/size would not match
// the hash and the test would fail.
type perHashDecoratingStore struct {
	*nullstore.NullStore

	// A valid, non-empty TxInpoints so the decorated meta is eligible for
	// caching: TxMetaCache.BatchDecorate refuses to cache a non-coinbase tx with
	// empty ParentTxHashes (that would poison the cache for subtree validation).
	inpoints subtree.TxInpoints
}

func feeFor(h chainhash.Hash) uint64  { return binary.LittleEndian.Uint64(h[0:8]) }
func sizeFor(h chainhash.Hash) uint64 { return binary.LittleEndian.Uint64(h[8:16]) }

func (s *perHashDecoratingStore) BatchDecorate(_ context.Context, items []*utxo.UnresolvedMetaData, _ ...fields.FieldName) error {
	for _, it := range items {
		if it == nil {
			continue
		}

		it.Data = &meta.Data{
			Fee:         feeFor(it.Hash),
			SizeInBytes: sizeFor(it.Hash),
			TxInpoints:  s.inpoints,
			BlockIDs:    make([]uint32, 0), // empty => not yet mined => eligible for caching
		}
		it.Err = nil
	}

	return nil
}

func newPerHashCache(t testing.TB, bucketType BucketType) *TxMetaCache {
	t.Helper()

	ns, err := nullstore.NewNullStore()
	require.NoError(t, err)
	require.NoError(t, ns.SetBlockHeight(100))

	// A single-parent tx yields a valid, cacheable TxInpoints.
	tx := bt.NewTx()
	require.NoError(t, tx.From("0000000000000000000000000000000000000000000000000000000000000001", 0,
		"76a914000000000000000000000000000000000000000088ac", 1000))

	inpoints, err := subtree.NewTxInpointsFromTx(tx)
	require.NoError(t, err)

	c, err := NewTxMetaCache(context.Background(), settings.NewSettings(), ulogger.TestLogger{},
		&perHashDecoratingStore{NullStore: ns, inpoints: inpoints}, bucketType)
	require.NoError(t, err)

	return c.(*TxMetaCache)
}

func hashFor(g, i int) chainhash.Hash {
	var h chainhash.Hash
	binary.LittleEndian.PutUint32(h[0:4], uint32(g))
	binary.LittleEndian.PutUint32(h[8:12], uint32(i)) // distinct in both fee and size words
	return h
}

// Test_TxMetaCache_BatchDecorate_PooledBuffers_Correct verifies that, with the
// pooled serialization buffers, every decorated transaction lands in the cache
// with its own correct metadata (no buffer cross-contamination within a call).
func Test_TxMetaCache_BatchDecorate_PooledBuffers_Correct(t *testing.T) {
	for _, bt := range []BucketType{Unallocated, Native} {
		cache := newPerHashCache(t, bt)
		ctx := context.Background()

		const n = 500
		items := make([]*utxo.UnresolvedMetaData, n)
		for i := range items {
			items[i] = &utxo.UnresolvedMetaData{Hash: hashFor(0, i)}
		}

		require.NoError(t, cache.BatchDecorate(ctx, items, fields.Fee, fields.SizeInBytes, fields.TxInpoints))

		for i := 0; i < n; i++ {
			h := hashFor(0, i)
			got, found := cache.GetMetaCached(ctx, h)
			require.True(t, found, "bucket %v: tx %d missing from cache", bt, i)
			require.Equal(t, feeFor(h), got.Fee, "bucket %v: tx %d fee", bt, i)
			require.Equal(t, sizeFor(h), got.SizeInBytes, "bucket %v: tx %d size", bt, i)
		}
	}
}

// Test_TxMetaCache_BatchDecorate_PooledBuffers_Concurrent stresses the buffer
// pool: many goroutines run BatchDecorate over disjoint hash sets at once. Each
// entry must still round-trip to its own hash-derived metadata. Run under -race
// to catch any cross-goroutine reuse of a pooled buffer.
func Test_TxMetaCache_BatchDecorate_PooledBuffers_Concurrent(t *testing.T) {
	cache := newPerHashCache(t, Unallocated)
	ctx := context.Background()

	const (
		goroutines = 8
		perG       = 300
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			items := make([]*utxo.UnresolvedMetaData, perG)
			for i := range items {
				items[i] = &utxo.UnresolvedMetaData{Hash: hashFor(g, i)}
			}

			require.NoError(t, cache.BatchDecorate(ctx, items, fields.Fee, fields.SizeInBytes, fields.TxInpoints))
		}(g)
	}

	wg.Wait()

	for g := 0; g < goroutines; g++ {
		for i := 0; i < perG; i++ {
			h := hashFor(g, i)
			got, found := cache.GetMetaCached(ctx, h)
			require.True(t, found, "g=%d tx %d missing", g, i)
			require.Equal(t, feeFor(h), got.Fee, "g=%d tx %d fee", g, i)
			require.Equal(t, sizeFor(h), got.SizeInBytes, "g=%d tx %d size", g, i)
		}
	}
}
