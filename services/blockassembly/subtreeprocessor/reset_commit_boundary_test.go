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

	require.True(t, stp.TakeResetRequested(), "a post-commit map error surfacing during reset's own reload must request a further reset - reset has no rollback of its own, so this is the only way the phantom it may have left gets cured")
}

// A disk tx map error recorded before reset ever started - or Clear's own
// pre-rotation flushAllDisks - belongs to the generation Clear just rotated
// away. Reset's Clear gives every disk a fresh Badger generation, so nothing
// about that stale error is still true of the map's current content: it must
// not survive to request a (redundant) reset once reset itself succeeds.
// Without this, a stale pending error - or a prior reset's own unconsumed
// request - makes every subsequent clean reset immediately request another
// one, looping (finding P1-1/P2-3).
func TestReset_StaleErrorBeforeResetDoesNotRequestReset(t *testing.T) {
	stp := newSubtreeProcessorWithTxMapDirs(t, []string{t.TempDir()})

	stp.diskTxMap.recordErr(errors.NewStorageError("old generation write failed"))

	response := stp.reset(&model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      2200000002,
		Bits:           model.NBit{},
		Nonce:          9202,
	}, nil, nil, false, func() error { return nil })
	require.NoError(t, response, "a clean reset (no reload error) must succeed")

	require.False(t, stp.TakeResetRequested(), "a stale pre-reset error must not survive the rotation to request a redundant reset")
	require.NoError(t, stp.diskTxMapErr(), "the stale error must have been drained, not left pending")
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

	before := testutil.ToFloat64(prometheusSubtreeProcessorDiskTxMapErrors.WithLabelValues("reset_rotation_failed"))

	resetErr := stp.reset(targetHeader, nil, nil, false, nil)
	require.Error(t, resetErr, "a failed tx map rotation must fail reset")
	require.ErrorContains(t, resetErr, "tx map still holds")

	// A rotation failure dead-ends the usual post-commit escalation (reset
	// fails outright, so reportOrJoinDiskTxMapErr("reset") only joins the map
	// error into that failure instead of requesting a reset): it must still
	// be visible through its own distinct counter, since the generic
	// disk_tx_map_errors_total{where="reset"} label is never incremented on
	// this path.
	after := testutil.ToFloat64(prometheusSubtreeProcessorDiskTxMapErrors.WithLabelValues("reset_rotation_failed"))
	require.Equal(t, before+1, after, "a rotation failure must be counted distinctly from the generic post-commit path")

	require.Same(t, originalCurrentSubtree, stp.currentSubtree.Load(), "currentSubtree must be untouched: reset must fail before replacing it")
	require.Equal(t, originalChainedLen, len(stp.chainedSubtrees), "chainedSubtrees must be untouched: closeChainedSubtrees must never run")
	require.Equal(t, originalHeader.Hash(), stp.currentBlockHeader.Load().Hash(), "header must be untouched")
}

func resetTestHeader(nonce uint32) *model.BlockHeader {
	return &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      2200000003,
		Bits:           model.NBit{},
		Nonce:          nonce,
	}
}

// LastResetStorageFailed reports whether the most recent reset itself hit a
// disk tx map storage error (its rotation failed, or its reload raised a new
// reset request). BlockAssembler uses it to decide whether a storage-triggered
// reset cured the map, without consuming the request.
func TestReset_LastResetStorageFailed(t *testing.T) {
	t.Run("clean reset", func(t *testing.T) {
		stp := newSubtreeProcessorWithTxMapDirs(t, []string{t.TempDir()})

		require.NoError(t, stp.reset(resetTestHeader(9301), nil, nil, false, func() error { return nil }))
		require.False(t, stp.LastResetStorageFailed())
	})

	t.Run("reload raises a new request", func(t *testing.T) {
		stp := newSubtreeProcessorWithTxMapDirs(t, []string{t.TempDir()})

		require.NoError(t, stp.reset(resetTestHeader(9302), nil, nil, false, func() error {
			stp.requestReset("test_reload")
			return nil
		}))
		require.True(t, stp.LastResetStorageFailed())
		require.True(t, stp.TakeResetRequested(), "the reload's request stays pending for the heartbeat")

		// The next clean reset clears it.
		require.NoError(t, stp.reset(resetTestHeader(9303), nil, nil, false, func() error { return nil }))
		require.False(t, stp.LastResetStorageFailed())
	})

	t.Run("a request raised before the rotation does not count", func(t *testing.T) {
		stp := newSubtreeProcessorWithTxMapDirs(t, []string{t.TempDir()})
		stp.requestReset("before_reset")

		require.NoError(t, stp.reset(resetTestHeader(9304), nil, nil, false, func() error { return nil }))
		require.False(t, stp.LastResetStorageFailed())
	})
}
