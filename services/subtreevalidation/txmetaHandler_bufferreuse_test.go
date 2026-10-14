package subtreevalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/stores/txmetacache"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/stretchr/testify/require"
)

// TestServer_txmetaHandler_CacheDoesNotRetainMessageBuffer pins the contract
// that lets the handler pass sub-slices of msg.Value to the cache without
// copying them first: Kafka may recycle msg.Value once the handler returns, so
// every backend must have copied the key and value by then.
func TestServer_txmetaHandler_CacheDoesNotRetainMessageBuffer(t *testing.T) {
	for _, bucket := range []txmetacache.BucketType{txmetacache.Unallocated, txmetacache.Pointer, txmetacache.Lockfree} {
		t.Run(bucketName(bucket), func(t *testing.T) {
			testCacheBucket = bucket

			t.Cleanup(func() { testCacheBucket = txmetacache.Unallocated })

			cache := setupTestCache(t)
			server := &Server{logger: ulogger.TestLogger{}, utxoStore: cache}

			hash := chainhash.HashH([]byte("buffer-reuse"))
			want := &meta.Data{
				Fee:         1234,
				SizeInBytes: 321,
				TxInpoints:  subtree.NewTxInpointsFromPacked([]chainhash.Hash{chainhash.HashH([]byte("parent"))}, []uint32{1, 7}),
			}

			payload, err := want.MetaBytes()
			require.NoError(t, err)

			msg := &kafka.KafkaMessage{Value: buildV1TxmetaMessage(t, 1, hash, payload)}
			require.NoError(t, server.txmetaHandler(context.Background(), msg))

			// Kafka reuses the buffer.
			for i := range msg.Value {
				msg.Value[i] = 0xAA
			}

			got, ok := cache.GetMetaCached(context.Background(), hash)
			require.True(t, ok, "the entry must have been cached under the original key")
			require.Equal(t, want.Fee, got.Fee)
			require.Equal(t, want.SizeInBytes, got.SizeInBytes)
			require.Equal(t, want.TxInpoints.ParentTxHashes, got.TxInpoints.ParentTxHashes)
		})
	}
}

func bucketName(b txmetacache.BucketType) string {
	switch b {
	case txmetacache.Pointer:
		return "pointer"
	case txmetacache.Lockfree:
		return "lockfree"
	default:
		return "unallocated"
	}
}
