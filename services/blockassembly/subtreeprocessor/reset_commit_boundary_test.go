package subtreeprocessor

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// reset has no rollback: by the time its postProcess callback runs, reset has
// already committed (header at the target tip, currentTxMap cleared). A disk
// tx map error surfacing during the reload inside postProcess must therefore
// be logged and counted, not fail reset - the binding decision forbids
// returning a post-commit error. This is what BlockAssembler.loadUnminedTransactions
// achieves in production by calling AddNodesDirectlyReportOnly/AddDirectlyReportOnly
// instead of AddNodesDirectly/AddDirectly when isReload is true; this test
// exercises the same contract directly against SubtreeProcessor.reset.
func TestReset_PostProcessReportOnlyMapErrorDoesNotFailReset(t *testing.T) {
	ctx := context.Background()

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), utxoStoreURL)
	require.NoError(t, err)

	blockchainClient := &blockchain.Mock{}

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	t.Cleanup(func() { close(newSubtreeChan) })

	stp, err := NewSubtreeProcessor(ctx, ulogger.NewErrorTestLogger(t), test.CreateBaseTestSettings(t), blob_memory.New(),
		blockchainClient, utxoStore, newSubtreeChan, WithTxMapDirs([]string{t.TempDir()}))
	require.NoError(t, err)
	stp.Start(ctx)
	t.Cleanup(func() { stp.Stop(context.Background()) })

	targetHeader := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      2200000001,
		Bits:           model.NBit{},
		Nonce:          9201,
	}

	boom := errors.NewStorageError("badger write failed")

	before := testutil.ToFloat64(prometheusSubtreeProcessorDiskTxMapErrors.WithLabelValues("AddDirectlyReportOnly"))

	postProcess := func() error {
		// Simulates BlockAssembler.loadUnminedTransactions(ctx, isReload=true):
		// a pending map error observed by the report-only add must not fail
		// this callback (and so must not fail reset).
		stp.diskTxMap.recordErr(boom)

		node := &subtreepkg.Node{Hash: chainhash.HashH([]byte("reset-postprocess-reportonly-tx")), Fee: 1, SizeInBytes: 100}
		inp := &subtreepkg.TxInpoints{}

		return stp.AddDirectlyReportOnly(node, inp, true)
	}

	response := stp.Reset(targetHeader, nil, nil, false, postProcess)
	require.NoError(t, response.Err, "a report-only map error in postProcess must not fail reset")

	require.Equal(t, targetHeader.Hash(), stp.GetCurrentBlockHeader().Hash(), "reset must still land on the target header")

	after := testutil.ToFloat64(prometheusSubtreeProcessorDiskTxMapErrors.WithLabelValues("AddDirectlyReportOnly"))
	require.Equal(t, before+1, after, "the map error must still be logged and counted")
}

// reset's currentTxMap.Clear() + Length()==0 check runs before
// closeChainedSubtrees and before currentSubtree is replaced (F3): a failed
// rotation must leave STP fully intact - still on the pre-reset header,
// chainedSubtrees and currentSubtree - rather than committed to an empty
// template on top of a still-populated, stale map.
func TestReset_FailedTxMapClearLeavesSTPFullyIntact(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), utxoStoreURL)
	require.NoError(t, err)

	blockchainClient := &blockchain.Mock{}

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	t.Cleanup(func() { close(newSubtreeChan) })

	stp, err := NewSubtreeProcessor(ctx, ulogger.NewErrorTestLogger(t), test.CreateBaseTestSettings(t), blob_memory.New(),
		blockchainClient, utxoStore, newSubtreeChan, WithTxMapDirs([]string{dir}))
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, half := range []*DiskTxMap{stp.diskTxMap, stp.diskTxMapShadow} {
			if half != nil {
				_ = half.Close()
			}
		}
	})

	// Not started: reset is called directly (single-goroutine), so reading
	// STP's internal fields afterwards is race-free.
	node := &subtreepkg.Node{Hash: chainhash.HashH([]byte("pre-reset-tx")), Fee: 1, SizeInBytes: 100}
	require.NoError(t, stp.AddDirectly(node, &subtreepkg.TxInpoints{}, true))

	originalHeader := stp.currentBlockHeader.Load()
	originalCurrentSubtree := stp.currentSubtree.Load()
	originalChainedLen := len(stp.chainedSubtrees)

	sealDir(t, dir) // Clear can no longer rotate: its replacement generation can't be created.

	targetHeader := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      2300000001,
		Bits:           model.NBit{},
		Nonce:          9301,
	}

	resetErr := stp.reset(targetHeader, nil, nil, false, nil)
	require.Error(t, resetErr, "a failed tx map rotation must fail reset")
	require.ErrorContains(t, resetErr, "tx map still holds")

	require.Same(t, originalCurrentSubtree, stp.currentSubtree.Load(), "currentSubtree must be untouched: reset must fail before replacing it")
	require.Equal(t, originalChainedLen, len(stp.chainedSubtrees), "chainedSubtrees must be untouched: closeChainedSubtrees must never run")
	require.Equal(t, originalHeader.Hash(), stp.currentBlockHeader.Load().Hash(), "header must be untouched")
}
