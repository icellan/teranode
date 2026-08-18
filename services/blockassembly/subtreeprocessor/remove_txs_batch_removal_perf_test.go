package subtreeprocessor

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// setupBatchRemovalProcessor builds a SubtreeProcessor with subtreeSize-sized
// subtrees, optionally backed by DiskTxMap (which is what maintains the
// currentTxMap.SubtreeIndex bookkeeping that locateTxInSubtrees uses for its
// O(1) shortcut).
func setupBatchRemovalProcessor(tb testing.TB, subtreeSize int, useDiskMap bool) *SubtreeProcessor {
	tb.Helper()

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(tb, err)

	utxoStore, err := sql.New(context.Background(), ulogger.TestLogger{}, test.CreateBaseTestSettings(tb), utxoStoreURL)
	require.NoError(tb, err)

	blobStore := memory.New()

	settings := test.CreateBaseTestSettings(tb)
	settings.BlockAssembly.InitialMerkleItemsPerSubtree = subtreeSize

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	tb.Cleanup(func() {
		close(newSubtreeChan)
	})

	var opts []Options
	if useDiskMap {
		opts = append(opts, WithTxMapDirs([]string{tb.TempDir()}))
	}

	stp, err := NewSubtreeProcessor(context.Background(), ulogger.TestLogger{}, settings, blobStore, nil, utxoStore, newSubtreeChan, opts...)
	require.NoError(tb, err)

	return stp
}

// populateChainedSubtrees adds numSubtrees*subtreeSize transactions directly,
// producing numSubtrees complete chained subtrees (plus a fresh, empty current
// subtree), and returns every hash in insertion order.
func populateChainedSubtrees(tb testing.TB, stp *SubtreeProcessor, numSubtrees, subtreeSize int) []chainhash.Hash {
	tb.Helper()

	hashes := make([]chainhash.Hash, 0, numSubtrees*subtreeSize)

	// The first subtree already carries the coinbase placeholder at index 0,
	// so it only has subtreeSize-1 free slots.
	for st := 0; st < numSubtrees; st++ {
		free := subtreeSize
		if st == 0 {
			free = subtreeSize - 1
		}

		for i := 0; i < free; i++ {
			h := chainhash.HashH([]byte(fmt.Sprintf("batchremoval_%d_%d", st, i)))
			hashes = append(hashes, h)

			node := &subtreepkg.Node{Hash: h, Fee: 1000, SizeInBytes: 250}
			require.NoError(tb, stp.AddDirectly(node, &subtreepkg.TxInpoints{}, true))
		}
	}

	require.Equal(tb, numSubtrees, len(stp.chainedSubtrees), "expected exactly numSubtrees complete chained subtrees")

	return hashes
}

// BenchmarkRemoveTxsFromSubtrees_ManySubtrees exercises removeTxsFromSubtrees
// under worst-case deep-reorg conditions: many chained subtrees and many
// transactions to remove in a single batch call, with the hashes scattered
// across subtrees (including the last one), forcing a linear scan over every
// chained subtree per hash unless the currentTxMap shortcut is used.
func BenchmarkRemoveTxsFromSubtrees_ManySubtrees(b *testing.B) {
	if testing.Short() {
		b.Skip("heavy benchmark; skipped in -short (CI passes -short)")
	}

	const (
		numSubtrees  = 200
		subtreeSize  = 16
		removeStride = 4 // remove every 4th tx: ~25% of all transactions
	)

	b.Run("DiskTxMap_shortcut", func(b *testing.B) {
		benchmarkRemoveTxsFromSubtrees(b, numSubtrees, subtreeSize, removeStride, true)
	})

	b.Run("InMemoryMap_linear_scan", func(b *testing.B) {
		benchmarkRemoveTxsFromSubtrees(b, numSubtrees, subtreeSize, removeStride, false)
	})
}

func benchmarkRemoveTxsFromSubtrees(b *testing.B, numSubtrees, subtreeSize, removeStride int, useDiskMap bool) {
	b.ReportAllocs()

	ctx := context.Background()

	for i := 0; i < b.N; i++ {
		b.StopTimer()

		stp := setupBatchRemovalProcessor(b, subtreeSize, useDiskMap)
		allHashes := populateChainedSubtrees(b, stp, numSubtrees, subtreeSize)

		toRemove := make([]chainhash.Hash, 0, len(allHashes)/removeStride+1)
		for idx := 0; idx < len(allHashes); idx += removeStride {
			toRemove = append(toRemove, allHashes[idx])
		}

		b.StartTimer()

		if err := stp.removeTxsFromSubtrees(ctx, toRemove); err != nil {
			b.Fatalf("removeTxsFromSubtrees failed: %v", err)
		}
	}
}

// TestRemoveTxsFromSubtrees_DiskTxMapShortcutMatchesLinearScan proves that the
// currentTxMap-based shortcut used by removeTxsFromSubtrees (when DiskTxMap is
// active) produces exactly the same result as the linear-scan fallback (when it
// is not): the same set of transactions removed, and the same subtree structure
// left behind. This is a pure performance optimization, so the two code paths
// must be behaviorally identical.
func TestRemoveTxsFromSubtrees_DiskTxMapShortcutMatchesLinearScan(t *testing.T) {
	ctx := context.Background()

	const (
		numSubtrees  = 12
		subtreeSize  = 8
		removeStride = 3
	)

	buildAndRemove := func(useDiskMap bool) (*SubtreeProcessor, []chainhash.Hash, []chainhash.Hash) {
		stp := setupBatchRemovalProcessor(t, subtreeSize, useDiskMap)
		allHashes := populateChainedSubtrees(t, stp, numSubtrees, subtreeSize)

		var toRemove, expectedRemaining []chainhash.Hash
		for idx, h := range allHashes {
			if idx%removeStride == 0 {
				toRemove = append(toRemove, h)
			} else {
				expectedRemaining = append(expectedRemaining, h)
			}
		}

		require.NoError(t, stp.removeTxsFromSubtrees(ctx, toRemove))

		return stp, toRemove, expectedRemaining
	}

	stpShortcut, removedShortcut, remainingShortcut := buildAndRemove(true)
	stpScan, removedScan, remainingScan := buildAndRemove(false)

	occurrences := func(stp *SubtreeProcessor, h chainhash.Hash) int {
		count := 0
		if stp.currentSubtree.Load().NodeIndex(h) >= 0 {
			count++
		}

		for _, cs := range stp.chainedSubtrees {
			if cs.NodeIndex(h) >= 0 {
				count++
			}
		}

		return count
	}

	// Same removed set produces the same absence, on both code paths.
	for _, h := range removedShortcut {
		require.Equal(t, 0, occurrences(stpShortcut, h), "removed hash must be absent (shortcut path)")
	}

	for _, h := range removedScan {
		require.Equal(t, 0, occurrences(stpScan, h), "removed hash must be absent (linear-scan path)")
	}

	_, existsShortcut := stpShortcut.currentTxMap.Get(removedShortcut[0])
	require.False(t, existsShortcut, "removed hash must be gone from currentTxMap (shortcut path)")

	// Same surviving set, present exactly once, on both code paths.
	require.Equal(t, len(remainingScan), len(remainingShortcut))

	for i, h := range remainingShortcut {
		require.Equal(t, 1, occurrences(stpShortcut, h), "surviving hash must remain exactly once (shortcut path)")
		require.Equal(t, 1, occurrences(stpScan, remainingScan[i]), "surviving hash must remain exactly once (linear-scan path)")
	}

	// Identical resulting subtree structure: same chained-subtree count, and
	// every chained subtree fully compacted (no holes) on both paths.
	require.Equal(t, len(stpScan.chainedSubtrees), len(stpShortcut.chainedSubtrees), "chained subtree count must match")

	for i := range stpShortcut.chainedSubtrees {
		require.True(t, stpShortcut.chainedSubtrees[i].IsComplete(), "chained subtree %d must be complete (shortcut path)", i)
		require.True(t, stpScan.chainedSubtrees[i].IsComplete(), "chained subtree %d must be complete (linear-scan path)", i)
		require.Equal(t, stpScan.chainedSubtrees[i].Length(), stpShortcut.chainedSubtrees[i].Length(), "chained subtree %d length must match", i)
	}

	require.Equal(t, stpScan.currentSubtree.Load().Length(), stpShortcut.currentSubtree.Load().Length(), "current subtree length must match")

	requireCoinbasePlaceholderIntact(t, stpShortcut)
	requireCoinbasePlaceholderIntact(t, stpScan)
}
