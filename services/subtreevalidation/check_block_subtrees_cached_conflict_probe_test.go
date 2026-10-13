package subtreevalidation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	utxometa "github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/stretchr/testify/require"
)

// storeCachedSubtreeOf stores a blessed subtree holding the coinbase placeholder
// followed by txHashes, and returns its root hash.
func storeCachedSubtreeOf(t *testing.T, server *Server, txHashes []chainhash.Hash) chainhash.Hash {
	t.Helper()

	st, err := subtreepkg.NewIncompleteTreeByLeafCount(len(txHashes) + 1)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())

	for _, h := range txHashes {
		require.NoError(t, st.AddNode(h, 1, 1))
	}

	serialized, err := st.Serialize()
	require.NoError(t, err)

	rootHash := st.RootHash()
	require.NoError(t, server.subtreeStore.Set(context.Background(), rootHash[:], fileformat.FileTypeSubtree, serialized))

	return *rootHash
}

// TestCachedSubtreeConflictingNodes_RetriesTransientStoreError pins that the
// conflicting probe goes through the same transient-failure retry as every other
// decorate read in this service. The probe now runs on every cached subtree of
// every block, so without the retry one store timeout fails CheckBlockSubtrees.
func TestCachedSubtreeConflictingNodes_RetriesTransientStoreError(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	server.settings.BlockValidation.ProcessTxMetaUsingStoreRetries = 3

	tx := chainhash.HashH([]byte("probe-retry-tx"))
	rootHash := storeCachedSubtreeOf(t, server, []chainhash.Hash{tx})

	store := &flakyBatchDecorateStore{
		failures: 1,
		err:      errors.NewStorageError("error in aerospike map store batch records", errors.NewError("ResultCode: TIMEOUT")),
	}
	server.utxoStore = store

	conflicting, err := server.cachedSubtreeConflictingNodes(context.Background(), []chainhash.Hash{rootHash})
	require.NoError(t, err, "a transient store failure must be retried, not fail the block")
	require.Empty(t, conflicting)
	require.Equal(t, 2, store.callCount(), "expected one failed attempt followed by a successful one")
}

// barrierDecorateStore holds every BatchDecorate call until `want` calls are in
// flight at once, and fails the call if that never happens. It also flags the
// transactions in `conflicting`.
type barrierDecorateStore struct {
	utxo.Store

	want        int
	conflicting map[chainhash.Hash]bool

	mu       sync.Mutex
	inFlight int
	reached  chan struct{}
}

func (s *barrierDecorateStore) BatchDecorate(ctx context.Context, items []*utxo.UnresolvedMetaData, _ ...fields.FieldName) error {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight == s.want {
		close(s.reached)
	}
	s.mu.Unlock()

	select {
	case <-s.reached:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.NewProcessingError("batches of one subtree were never in flight together")
	}

	for _, item := range items {
		item.Data = &utxometa.Data{Conflicting: s.conflicting[item.Hash]}
	}

	return nil
}

// TestCachedSubtreeConflictingNodes_BatchesOfOneSubtreeRunConcurrently pins that
// a large cached subtree's batches go to the store in parallel. Parallelism only
// across subtrees leaves a block of a few 1M-leaf subtrees doing ~1000 serial
// round trips each.
func TestCachedSubtreeConflictingNodes_BatchesOfOneSubtreeRunConcurrently(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	server.settings.BlockValidation.ProcessTxMetaUsingStoreBatchSize = 2
	server.settings.BlockValidation.ProcessTxMetaUsingStoreConcurrency = 4

	// Coinbase + 7 txs = 4 batches of 2 leaves.
	txs := make([]chainhash.Hash, 7)
	for i := range txs {
		txs[i] = chainhash.HashH([]byte{byte(i)})
	}

	rootHash := storeCachedSubtreeOf(t, server, txs)

	server.utxoStore = &barrierDecorateStore{
		want:        2,
		conflicting: map[chainhash.Hash]bool{txs[0]: true, txs[6]: true},
		reached:     make(chan struct{}),
	}

	conflicting, err := server.cachedSubtreeConflictingNodes(context.Background(), []chainhash.Hash{rootHash})
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{txs[0], txs[6]}, conflicting, "flagged txs must come back in subtree order")
}

// TestCachedSubtreeConflictingNodes_EmptySubtreeWithUnsetBatchSize guards the
// batch-count arithmetic: an unset batch size falls back to the leaf count, which
// is zero for an empty subtree.
func TestCachedSubtreeConflictingNodes_EmptySubtreeWithUnsetBatchSize(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	server.settings.BlockValidation.ProcessTxMetaUsingStoreBatchSize = 0

	// Hand-built, since an empty Subtree cannot Serialize (it has no root hash):
	// root hash, fees, size, leaf count 0, conflicting count 0.
	serialized := make([]byte, chainhash.HashSize+8+8+8+8)

	rootHash := chainhash.HashH([]byte("empty-subtree"))
	require.NoError(t, server.subtreeStore.Set(context.Background(), rootHash[:], fileformat.FileTypeSubtree, serialized))

	conflicting, err := server.cachedSubtreeConflictingNodes(context.Background(), []chainhash.Hash{rootHash})
	require.NoError(t, err)
	require.Empty(t, conflicting)
}

// TestCachedSubtreeConflictingNodes_CancelledContextIsNotAStorageFault pins that a
// store failure seen while the context is cancelled — shutdown, or a sibling
// subtree failing the errgroup — is reported as cancellation. blockvalidation
// treats a storage fault as recoverable and redelivers the block for it.
func TestCachedSubtreeConflictingNodes_CancelledContextIsNotAStorageFault(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	server.settings.BlockValidation.ProcessTxMetaUsingStoreRetries = 3

	tx := chainhash.HashH([]byte("probe-cancel-tx"))
	rootHash := storeCachedSubtreeOf(t, server, []chainhash.Hash{tx})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.utxoStore = &flakyBatchDecorateStore{
		failures: 100,
		err:      errors.NewStorageError("error in aerospike map store batch records", errors.NewError("ResultCode: TIMEOUT")),
		onCall:   func(int) { cancel() },
	}

	_, err := server.cachedSubtreeConflictingNodes(ctx, []chainhash.Hash{rootHash})
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrContextCanceled), "expected a context-cancelled error, got: %v", err)
	require.False(t, errors.Is(err, errors.ErrStorageError), "a cancellation must not surface as a storage fault, got: %v", err)
}
