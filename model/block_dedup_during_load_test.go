package model

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// dedupLoadFixture builds a two-subtree block served from a rawSubtreeStore.
// When dup is non-nil it is the last leaf of both subtrees.
func dedupLoadFixture(t *testing.T, dup *chainhash.Hash) (*Block, *subtreepkg.Subtree, *subtreepkg.Subtree, SubtreeStore) {
	t.Helper()

	blockHeaderBytes, err := hex.DecodeString(block1Header)
	require.NoError(t, err)
	blockHeader, err := NewBlockHeaderFromBytes(blockHeaderBytes)
	require.NoError(t, err)

	coinbase, err := bt.NewTxFromString(CoinbaseHex)
	require.NoError(t, err)

	st0, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, st0.AddCoinbaseNode())

	for i := 0; i < 2; i++ {
		require.NoError(t, st0.AddNode(randCorruptTestHash(t), 1, 0))
	}

	if dup != nil {
		require.NoError(t, st0.AddNode(*dup, 1, 0))
	} else {
		require.NoError(t, st0.AddNode(randCorruptTestHash(t), 1, 0))
	}

	st1, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoError(t, st1.AddNode(randCorruptTestHash(t), 1, 0))
	}

	if dup != nil {
		require.NoError(t, st1.AddNode(*dup, 1, 0))
	} else {
		require.NoError(t, st1.AddNode(randCorruptTestHash(t), 1, 0))
	}

	store := &rawSubtreeStore{data: make(map[chainhash.Hash][]byte)}
	store.put(t, st0)
	store.put(t, st1)

	b, err := NewBlock(blockHeader, coinbase, []*chainhash.Hash{st0.RootHash(), st1.RootHash()}, 8, 123, 0, 0)
	require.NoError(t, err)

	return b, st0, st1, store
}

// TestGetAndValidateSubtreesWithDedup_FillsTxMap pins that the duplicate check
// is done while the subtrees load: the txMap comes back filled with every
// non-coinbase leaf at the same index checkDuplicateTransactions would give it,
// sized from the body as len(Subtrees) x first subtree length.
func TestGetAndValidateSubtreesWithDedup_FillsTxMap(t *testing.T) {
	b, st0, st1, store := dedupLoadFixture(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	defer b.releaseTxMap()

	deduped, err := b.getAndValidateSubtreesWithDedup(ctx, ulogger.TestLogger{}, store, 2)
	require.NoError(t, err)
	require.True(t, deduped)

	require.NotNil(t, b.txMap)
	require.Equal(t, uint64(2*st0.Length()), b.txMapCount)
	require.Equal(t, 7, b.txMap.Length(), "every leaf but the coinbase placeholder")

	subtreeSize := st0.Size()

	idx, ok := b.txMap.Get(st0.Nodes[1].Hash)
	require.True(t, ok)
	require.Equal(t, uint64(1), idx)

	idx, ok = b.txMap.Get(st1.Nodes[2].Hash)
	require.True(t, ok)
	require.Equal(t, uint64(subtreeSize+2), idx)

	_, ok = b.txMap.Get(st0.Nodes[0].Hash)
	require.False(t, ok, "the coinbase placeholder is not inserted")
}

// TestGetAndValidateSubtreesWithDedup_DuplicateAcrossSubtrees pins that a
// transaction repeated in a later subtree fails the load with the same
// corrupt (re-download, never poison) classification the separate pass gives.
func TestGetAndValidateSubtreesWithDedup_DuplicateAcrossSubtrees(t *testing.T) {
	dup := randCorruptTestHash(t)

	b, _, _, store := dedupLoadFixture(t, &dup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	defer b.releaseTxMap()

	_, err := b.getAndValidateSubtreesWithDedup(ctx, ulogger.TestLogger{}, store, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate transaction")
	require.True(t, errors.IsBlockCorrupt(err), "a duplicated body is corrupt, got: %v", err)
	require.False(t, errors.Is(err, errors.ErrBlockInvalid), "must not poison")
}

// TestGetAndValidateSubtreesWithDedup_AlreadyLoaded pins the fallback signal:
// when the subtrees are already in memory nothing is fetched, so no dedup ran
// and the caller must do the separate pass.
func TestGetAndValidateSubtreesWithDedup_AlreadyLoaded(t *testing.T) {
	b, st0, st1, store := dedupLoadFixture(t, nil)
	b.SubtreeSlices = []*subtreepkg.Subtree{st0, st1}

	defer b.releaseTxMap()

	deduped, err := b.getAndValidateSubtreesWithDedup(context.Background(), ulogger.TestLogger{}, store, 2)
	require.NoError(t, err)
	require.False(t, deduped)
	require.Nil(t, b.txMap)
}
