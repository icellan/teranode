package subtreevalidation

import (
	"context"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/stretchr/testify/require"
)

// recordingParentStore embeds MockUtxostore and records every key passed to
// BatchDecorate so a test can assert how many distinct parents were actually
// read from the store.
type recordingParentStore struct {
	*utxostore.MockUtxostore

	mu        sync.Mutex
	requested []chainhash.Hash
	resolve   map[chainhash.Hash]*meta.Data
}

func (s *recordingParentStore) BatchDecorate(_ context.Context, items []*utxostore.UnresolvedMetaData, _ ...fields.FieldName) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, it := range items {
		s.requested = append(s.requested, it.Hash)
		if d, ok := s.resolve[it.Hash]; ok {
			it.Data = d
		}
	}

	return nil
}

// Test_prefetchLevelParents_DedupsSharedParent proves the Phase-1 win: when a
// level has many transactions spending the same parent (the fan-out pattern that
// dominates the scaling test — one funding tx, many children), the bulk reader
// asks the store for that parent ONCE, not once per child. Without dedup the
// store sees N reads of the same record; with it, one.
func Test_prefetchLevelParents_DedupsSharedParent(t *testing.T) {
	ctx := context.Background()

	server, cleanup := setupTestServer(t)
	defer cleanup()

	lockingScript := "76a914000000000000000000000000000000000000000088ac"
	sharedParentHex := "0000000000000000000000000000000000000000000000000000000000000001"
	otherParentHex := "0000000000000000000000000000000000000000000000000000000000000002"

	mkTx := func(parentHex string, vout uint32) *bt.Tx {
		tx := bt.NewTx()
		require.NoError(t, tx.From(parentHex, vout, lockingScript, 1000))
		return tx
	}

	// 5 children of the shared parent + 1 tx spending a different parent.
	levelTxs := make([]missingTx, 0, 6)
	for i := 0; i < 5; i++ {
		levelTxs = append(levelTxs, missingTx{tx: mkTx(sharedParentHex, uint32(i)), idx: i})
	}
	levelTxs = append(levelTxs, missingTx{tx: mkTx(otherParentHex, 0), idx: 5})

	sharedParent := *levelTxs[0].tx.Inputs[0].PreviousTxIDChainHash()
	otherParent := *levelTxs[5].tx.Inputs[0].PreviousTxIDChainHash()

	store := &recordingParentStore{
		MockUtxostore: &utxostore.MockUtxostore{},
		resolve: map[chainhash.Hash]*meta.Data{
			sharedParent: {BlockHeights: []uint32{}},         // in-block / unconfirmed
			otherParent:  {BlockHeights: []uint32{100, 101}}, // earlier block
		},
	}
	server.utxoStore = store

	prefetched, err := server.prefetchLevelParents(ctx, levelTxs)
	require.NoError(t, err)

	// Both distinct parents resolved.
	require.Len(t, prefetched, 2)
	require.NotNil(t, prefetched[sharedParent])
	require.NotNil(t, prefetched[otherParent])

	// The dedup contract: each distinct parent read exactly once (2 total),
	// not once per child (which would be 6).
	require.Len(t, store.requested, 2,
		"shared parent must be read once per level, not once per child")
}
