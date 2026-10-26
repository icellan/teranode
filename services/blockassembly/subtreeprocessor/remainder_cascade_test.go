package subtreeprocessor

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/stretchr/testify/require"
)

// A subtree-resident descendant of a tx the block made conflicting must not
// survive the remainder pass. The pass used to filter by the block's tx map
// and immediate losers only, so a child drained into the subtrees before a
// catch-up (issue 2050) kept spending a conflicting parent. The cascade is
// transitive: a grandchild goes with its child.
func TestProcessRemainderTxHashes_DropsDescendantsOfConflictingTxs(t *testing.T) {
	for name, threshold := range map[string]int{"sequential_insert": 1 << 20, "parallel_insert": 1} {
		t.Run(name, func(t *testing.T) {
			stp := newTestProcessorNoStart(t)
			stp.settings.BlockAssembly.ParallelSetIfNotExistsThreshold = threshold

			parent := chainhash.HashH([]byte("cascade-conflicting-parent"))
			child := chainhash.HashH([]byte("cascade-child"))
			grandchild := chainhash.HashH([]byte("cascade-grandchild"))
			other := chainhash.HashH([]byte("cascade-unrelated"))

			st, err := subtreepkg.NewTreeByLeafCount(8)
			require.NoError(t, err)
			require.NoError(t, st.AddCoinbaseNode())

			sourceTxMap := NewSplitTxInpointsMap(16)
			for _, n := range []struct{ hash, parent chainhash.Hash }{
				{parent, chainhash.HashH([]byte("cascade-grandparent"))},
				{child, parent},
				{grandchild, child},
				{other, chainhash.HashH([]byte("cascade-unrelated-parent"))},
			} {
				require.NoError(t, st.AddSubtreeNode(subtreepkg.Node{Hash: n.hash, Fee: 1, SizeInBytes: 250}))
				sourceTxMap.Set(n.hash, &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{n.parent}})
			}

			transactionMap := NewSortedHashSet(16)
			require.NoError(t, transactionMap.Freeze(1))

			conflictingHashes := map[chainhash.Hash]struct{}{parent: {}}

			require.NoError(t, stp.processRemainderTxHashes(t.Context(), []*subtreepkg.Subtree{st}, transactionMap, nil, conflictingHashes, sourceTxMap, true))

			hashes := collectSubtreeHashes(stp)
			require.NotContains(t, hashes, parent, "a conflicting tx must not stay in the subtrees")
			require.NotContains(t, hashes, child, "a child of a conflicting tx must not stay in the subtrees")
			require.NotContains(t, hashes, grandchild, "a grandchild of a conflicting tx must not stay in the subtrees")
			require.Contains(t, hashes, other, "an unrelated tx must stay")

			require.Contains(t, conflictingHashes, child, "a dropped descendant must join the set, so the drain that follows rejects its queued children")
			require.Contains(t, conflictingHashes, grandchild, "a dropped descendant must join the set, so the drain that follows rejects its queued children")
		})
	}
}

// BenchmarkDropConflictingDescendants measures the cascade pass the remainder
// pass adds when a block has conflicts: one inpoints lookup per remainder
// node, then the sequential walk. Nothing matches, so every node is kept.
func BenchmarkDropConflictingDescendants(b *testing.B) {
	const (
		subtrees   = 8
		perSubtree = 1 << 20
	)

	currentTxMap := NewSplitTxInpointsMap(256)
	remainder := make([][]subtreepkg.Node, subtrees)

	var h chainhash.Hash

	for s := range remainder {
		remainder[s] = make([]subtreepkg.Node, perSubtree)

		for i := range remainder[s] {
			binary.LittleEndian.PutUint64(h[0:8], uint64(s)<<32|uint64(i))
			remainder[s][i] = subtreepkg.Node{Hash: h, Fee: 1, SizeInBytes: 1}
			currentTxMap.Set(h, &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{chainhash.HashH(h[:8])}})
		}
	}

	conflictingHashes := map[chainhash.Hash]struct{}{chainhash.HashH([]byte("bench-conflicting")): {}}
	workers := remainderWorkers(0)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// the pass compacts in place; nothing is dropped, so the input is unchanged
		require.NoError(b, dropConflictingDescendants(context.Background(), remainder, conflictingHashes, currentTxMap, workers))
	}

	b.ReportMetric(float64(subtrees*perSubtree)*float64(b.N)/b.Elapsed().Seconds(), "nodes/s")
}
