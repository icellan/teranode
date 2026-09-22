package subtreeprocessor

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// countMmapNodeFiles counts the mmap backing files go-subtree creates under dir.
// mmapNodeStore.Close() unlinks its file, so this is the observable that tells a
// retired-but-live subtree apart from a closed one without dereferencing Nodes
// (reading an unmapped region is a SIGSEGV, not something a test can assert on).
func countMmapNodeFiles(t *testing.T, dir string) int {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "subtree-nodes-*"))
	require.NoError(t, err)

	return len(matches)
}

func newMmapProcessor(t *testing.T, dir string) *SubtreeProcessor {
	t.Helper()

	settings := test.CreateBaseTestSettings(t)
	settings.BlockAssembly.SplitMapBuckets = 16

	stp, err := NewSubtreeProcessor(
		context.Background(), ulogger.TestLogger{}, settings,
		nil, nil, nil, make(chan NewSubtreeRequest, 16),
		WithMmapDir(dir),
	)
	require.NoError(t, err)

	return stp
}

// TestResetSubtreeState_DoesNotCloseSubtreesStillRead pins the P0 from review:
// moveForwardBlock captures currentSubtree and the chained subtrees, then calls
// resetSubtreeState, then passes those same pointers into
// processRemainderTransactionsAndDequeue, which reads their Nodes. Closing an
// mmap-backed subtree munmaps and unlinks the region Nodes points into, so the
// reset must hand the subtrees back to the caller rather than destroying them.
func TestResetSubtreeState_DoesNotCloseSubtreesStillRead(t *testing.T) {
	dir := t.TempDir()
	stp := newMmapProcessor(t, dir)

	require.True(t, stp.currentSubtree.Load().IsMmapBacked(), "mmapDir must produce an mmap-backed currentSubtree")

	// Park one completed subtree in the chain alongside the current one, so the
	// reset has both kinds of retired subtree to deal with.
	chained, err := stp.newSubtree(4)
	require.NoError(t, err)
	require.NoError(t, chained.AddCoinbaseNode())
	stp.chainedSubtrees = append(stp.chainedSubtrees, chained)

	capturedCurrent := stp.currentSubtree.Load()
	capturedChained := stp.chainedSubtrees

	before := countMmapNodeFiles(t, dir)
	require.Equal(t, 2, before, "one current + one chained mmap subtree")

	retired, err := stp.resetSubtreeState(true)
	require.NoError(t, err)

	// The reset must have installed a fresh current subtree and emptied the chain.
	require.NotSame(t, capturedCurrent, stp.currentSubtree.Load(), "reset must install a new currentSubtree")
	require.Empty(t, stp.chainedSubtrees, "reset must detach the chained subtrees")

	// ...and handed the old ones back, still mapped.
	require.Contains(t, retired, capturedCurrent, "previous currentSubtree must be retired, not closed")
	require.Contains(t, retired, capturedChained[0], "previous chained subtrees must be retired, not closed")

	require.Equal(t, before+1, countMmapNodeFiles(t, dir),
		"retired subtrees must still be mapped after reset (only the new currentSubtree is added)")

	// Nodes must still be readable — this is the deref that used to segfault.
	require.NotEmpty(t, capturedCurrent.Nodes)
	require.NotEmpty(t, capturedChained[0].Nodes)

	// The commit point releases them.
	closeRetiredSubtrees(retired)
	require.Equal(t, 1, countMmapNodeFiles(t, dir), "commit must release every retired subtree")
}

// TestResetSubtreeState_RetiresNothingWhenNewSubtreeFails asserts the reset does
// not destroy the old state when it cannot build the replacement: the caller
// rolls back onto exactly those subtrees.
func TestResetSubtreeState_RetiresNothingWhenNewSubtreeFails(t *testing.T) {
	dir := t.TempDir()
	stp := newMmapProcessor(t, dir)

	capturedCurrent := stp.currentSubtree.Load()

	// A non-power-of-two leaf count makes subtreepkg.NewTreeByLeafCount (and the
	// mmap variant) return ErrNotPowerOfTwo.
	stp.currentItemsPerFile.Store(3)

	retired, err := stp.resetSubtreeState(true)
	require.Error(t, err)
	require.Empty(t, retired, "a failed reset must retire nothing")
	require.Same(t, capturedCurrent, stp.currentSubtree.Load(), "a failed reset must leave currentSubtree in place")
	require.Equal(t, 1, countMmapNodeFiles(t, dir), "a failed reset must not unmap the live subtree")
}

func newDiskTxMapProcessor(t *testing.T, dirs []string) *SubtreeProcessor {
	t.Helper()

	settings := test.CreateBaseTestSettings(t)
	settings.BlockAssembly.SplitMapBuckets = 16

	stp, err := NewSubtreeProcessor(
		context.Background(), ulogger.TestLogger{}, settings,
		nil, nil, nil, make(chan NewSubtreeRequest, 16),
		WithTxMapDirs(dirs),
	)
	require.NoError(t, err)

	require.NotNil(t, stp.diskMap(), "txMapDirs must install a DiskTxMap")

	return stp
}

// TestResetSubtreeState_DiskTxMapKeepsPreResetGeneration pins the third review
// finding. processRemainderTxHashes reads the pre-block TxInpoints out of the
// captured currentTxMap while writing the surviving ones back into the active
// map. On the disk path both were the same object, so the in-place Clear() in
// resetSubtreeState destroyed the read source and every remainder lookup failed
// with "node %s not found in currentTxMap". The reset must rotate to a fresh
// generation and keep the old one readable.
func TestResetSubtreeState_DiskTxMapKeepsPreResetGeneration(t *testing.T) {
	stp := newDiskTxMapProcessor(t, []string{t.TempDir()})
	defer func() { _ = stp.diskMap().Close() }()

	hash := makeHash(0x11)
	preReset := stp.diskMap()

	_, wasSet := preReset.SetIfNotExists(hash, &subtreepkg.TxInpoints{})
	require.True(t, wasSet)
	require.NoError(t, preReset.Flush())

	_, err := stp.resetSubtreeState(true)
	require.NoError(t, err)

	require.NotSame(t, preReset, stp.diskMap(), "reset must rotate to a fresh generation")
	require.Equal(t, 0, stp.currentTxMap.Length(), "the freshly-current generation must be empty")

	_, found := preReset.Get(hash)
	require.True(t, found, "the retired generation must still serve the pre-reset TxInpoints")

	// Commit releases the retired generation.
	stp.clearCurrentTxMapShadow()
	require.Empty(t, stp.retiredTxMaps, "commit must release every retired generation")
}

// TestSwapCurrentTxMapBack_DiskTxMapRestoresGeneration covers the rollback half:
// a failed moveForwardBlock must put the pre-reset generation back, not leave
// the caller holding the empty one the reset just rotated in.
func TestSwapCurrentTxMapBack_DiskTxMapRestoresGeneration(t *testing.T) {
	stp := newDiskTxMapProcessor(t, []string{t.TempDir()})

	hash := makeHash(0x22)
	preReset := stp.diskMap()

	_, wasSet := preReset.SetIfNotExists(hash, &subtreepkg.TxInpoints{})
	require.True(t, wasSet)
	require.NoError(t, preReset.Flush())

	_, err := stp.resetSubtreeState(true)
	require.NoError(t, err)
	require.NotSame(t, preReset, stp.diskMap())

	stp.swapCurrentTxMapBack()

	require.Same(t, preReset, stp.diskMap(), "the disk-map accessor must follow the reinstated generation")
	require.Empty(t, stp.retiredTxMaps, "rollback must consume the retired generation")

	_, found := stp.currentTxMap.Get(hash)
	require.True(t, found, "rollback must preserve the pre-reset content")

	_ = stp.diskMap().Close()
}

// TestReleaseRetiredTxMaps_RemovesBadgerDirectories asserts a released
// generation actually gives its disk back — tempstore.Close() removes the
// directory, so a leak here would accumulate one Badger tree per block.
func TestReleaseRetiredTxMaps_RemovesBadgerDirectories(t *testing.T) {
	dir := t.TempDir()
	stp := newDiskTxMapProcessor(t, []string{dir})

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one live generation on disk")

	_, err = stp.resetSubtreeState(true)
	require.NoError(t, err)

	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "retired + fresh generation both on disk until commit")

	stp.clearCurrentTxMapShadow()

	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "commit must delete the retired generation's Badger directory")

	_ = stp.diskMap().Close()
}

// TestMoveForwardBlock_OffHeapPathsEndToEnd drives a real MoveForwardBlock with
// both off-heap settings enabled, which is the combination the settings wiring
// in this change makes reachable for the first time. It covers the two review
// findings together, end to end:
//
//   - blockassembly_subtreeMmapDir: resetSubtreeState used to Close() the
//     currentSubtree and the chained subtrees that moveForwardBlock then read
//     through, so this call took SIGSEGV inside processOwnBlockSubtreeNodes.
//   - blockassembly_txMapDirs: resetSubtreeState used to Clear() the DiskTxMap
//     in place, so the remainder pass could no longer read the TxInpoints of
//     the transactions that survive the block and the call failed with
//     "not found in currentTxMap".
//
// Both are success-path failures, not error-path ones: the block here is valid
// and half its subtrees stay in assembly as remainder.
func TestMoveForwardBlock_OffHeapPathsEndToEnd(t *testing.T) {
	const n = 18

	// Unbuffered, with a WaitGroup for the four subtrees the 18 transactions
	// complete: the same synchronisation TestMoveForwardBlock uses. A buffered
	// channel would let the assertions run while the processor is still
	// rotating subtrees, and GetCurrentLength() transiently equals 2 on the way
	// to every subtree boundary.
	newSubtreeChan := make(chan NewSubtreeRequest)

	var subtreesSeen sync.WaitGroup

	subtreesSeen.Add(4)

	go func() {
		seen := 0

		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}

			if seen < 4 {
				seen++

				subtreesSeen.Done()
			}
		}
	}()

	logger := ulogger.NewErrorTestLogger(t)
	subtreeStore := blob_memory.New()

	ctx := t.Context()

	settings := test.CreateBaseTestSettings(t)
	settings.BlockAssembly.InitialMerkleItemsPerSubtree = 4

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, logger, settings, utxoStoreURL)
	require.NoError(t, err)

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("SetBlockProcessedAt", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	mmapDir := t.TempDir()
	txMapDir := t.TempDir()

	stp, err := NewSubtreeProcessor(ctx, logger, settings, subtreeStore, blockchainClient, utxoStore, newSubtreeChan,
		WithMmapDir(mmapDir), WithTxMapDirs([]string{txMapDir}))
	require.NoError(t, err)

	require.True(t, stp.currentSubtree.Load().IsMmapBacked(), "the mmap path must actually be engaged")
	require.NotNil(t, stp.diskMap(), "the disk tx map path must actually be engaged")

	stp.Start(ctx)

	for i := 0; i < n; i++ {
		txid, err := generateTxID()
		require.NoError(t, err)

		hash, err := chainhash.NewHashFromStr(txid)
		require.NoError(t, err)

		if i == 0 {
			stp.currentSubtree.Load().ReplaceRootNode(hash, 0, 0)
		} else {
			stp.AddBatch([]subtreepkg.Node{{Hash: *hash, Fee: 1}}, []*subtreepkg.TxInpoints{{ParentTxHashes: []chainhash.Hash{*hash}}})
		}
	}

	subtreesSeen.Wait()

	require.Eventually(t, func() bool {
		return len(stp.chainedSubtrees) == 4 && stp.GetCurrentLength() == 2
	}, 5*time.Second, 10*time.Millisecond, "expected 4 chained subtrees and currentSubtree length 2 after processing all %d transactions", n)

	stp.currentItemsPerFile.Store(2)
	require.NoError(t, stp.utxoStore.SetBlockHeight(1))
	require.NoError(t, stp.utxoStore.SetMedianBlockTime(uint32(time.Now().Unix()))) //nolint:gosec

	// Local headers, not the package-level prevBlockHeader/blockHeader: other
	// tests in this package reassign those vars, which makes any test that reads
	// them order-dependent.
	ownPrevHeader := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      1234567890,
		Bits:           model.NBit{},
		Nonce:          4321,
	}

	ownBlockHeader := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  ownPrevHeader.Hash(),
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      1234567890,
		Bits:           model.NBit{},
		Nonce:          4321,
	}

	stp.InitCurrentBlockHeader(ownPrevHeader)

	// The block claims the first two chained subtrees; the rest of assembly is
	// remainder that has to be rebuilt from the retired subtrees and the retired
	// tx map generation.
	require.NoError(t, stp.MoveForwardBlock(&model.Block{
		Header: ownBlockHeader,
		Subtrees: []*chainhash.Hash{
			stp.chainedSubtrees[0].RootHash(),
			stp.chainedSubtrees[1].RootHash(),
		},
		CoinbaseTx: coinbaseTx,
	}))

	// Same assertions the in-memory TestMoveForwardBlock makes: the remainder
	// survived the move intact.
	require.Len(t, stp.chainedSubtrees, 5)
	require.Equal(t, 2, stp.chainedSubtrees[0].Size())
	require.Equal(t, 1, stp.GetCurrentLength())
	require.Equal(t, int(stp.TxCount()), stp.currentTxMap.Length()+1) //nolint:gosec

	// The commit point released the retired generation and every retired subtree,
	// leaving only what is live now.
	require.Empty(t, stp.retiredTxMaps, "commit must release the retired tx map generation")

	entries, err := os.ReadDir(txMapDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the live DiskTxMap generation may remain on disk")

	live := len(stp.chainedSubtrees) + 1 // chained + currentSubtree
	require.Equal(t, live, countMmapNodeFiles(t, mmapDir), "no mmap subtree may be leaked or closed early")
}
