package subtreeprocessor

import (
	"context"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// deferredDrainBlockHex is a block with one subtree (fd61a797...), whose
// transactions include 6affcabb...; see TestMoveForwardBlock_LeftInQueue.
const deferredDrainBlockHex = "000000206a21d13c3d2656557493b4652f67a763f835b86bf90107a60f412c290000000083ba48026c405d5a4b4d5aa3f10cee9de605a012e9a25f72a19aa9fe123380c689505c67c874461cc6dda18002fde501016b104579e34c5c12fad8899035be27f7605f8ff95db814ba02fbc49397a761fd01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff1903af32190000000000205f7c477c327c437c5f200001000000ffffffff01e50b5402000000001976a9147a112f6a373b80b4ebb2b02acef97f35aef7494488ac00000000feaf321900"

// newDeferredDrainProcessor returns a processor that has not been started, so
// nothing but handleMoveForwardRequest drains its queue, together with the
// foreign block it will be moved forward with and the unbuffered subtree
// announcement channel it reports completed subtrees on.
//
// wrap, when set, wraps the UTXO store the processor uses.
func newDeferredDrainProcessor(t *testing.T, wrap func(utxostore.Store) utxostore.Store) (*SubtreeProcessor, *model.Block, chan NewSubtreeRequest) {
	t.Helper()

	ctx := context.Background()
	logger := ulogger.NewErrorTestLogger(t)

	subtreeStore := blob_memory.New()

	subtreeHash, err := chainhash.NewHashFromStr("fd61a79793c4fb02ba14b85df98f5f60f727be359089d8fa125c4ce37945106b")
	require.NoError(t, err)

	subtreeBytes, err := os.ReadFile("./testdata/fd61a79793c4fb02ba14b85df98f5f60f727be359089d8fa125c4ce37945106b.subtree")
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, subtreeHash.CloneBytes(), fileformat.FileTypeSubtree, subtreeBytes))

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockAssembly.DoubleSpendWindow = 0
	tSettings.BlockAssembly.InitialMerkleItemsPerSubtree = 4
	tSettings.BlockAssembly.TxMapDirs = nil
	tSettings.BlockAssembly.SubtreeMmapDir = ""

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	sqlStore, err := sql.New(ctx, logger, tSettings, utxoStoreURL)
	require.NoError(t, err)

	var utxoStore utxostore.Store = sqlStore
	if wrap != nil {
		utxoStore = wrap(sqlStore)
	}

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("SetBlockProcessedAt", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	newSubtreeChan := make(chan NewSubtreeRequest)

	stp, err := NewSubtreeProcessor(ctx, logger, tSettings, subtreeStore, blockchainClient, utxoStore, newSubtreeChan)
	require.NoError(t, err)

	stp.currentBlockHeader.Store(model.GenesisBlockHeader)

	blockBytes, err := hex.DecodeString(deferredDrainBlockHex)
	require.NoError(t, err)

	block, err := model.NewBlockFromBytes(blockBytes)
	require.NoError(t, err)

	block.Header.HashPrevBlock = model.GenesisBlockHeader.Hash()

	return stp, block, newSubtreeChan
}

// subtreeHashes returns every tx hash in the processor's chained and current
// subtrees.
func subtreeHashes(stp *SubtreeProcessor) map[chainhash.Hash]struct{} {
	hashes := make(map[chainhash.Hash]struct{})

	for _, st := range stp.chainedSubtrees {
		for _, n := range st.Nodes {
			hashes[n.Hash] = struct{}{}
		}
	}

	for _, n := range stp.currentSubtree.Load().Nodes {
		hashes[n.Hash] = struct{}{}
	}

	return hashes
}

// TestHandleMoveForwardRequest_RespondsBeforeQueueDrain pins that the caller
// of MoveForwardBlock gets its result as soon as the block is applied, and the
// queue that built up while the block was being applied is drained afterwards.
// Block assembly only reports the new tip, and serves mining candidates with
// transactions again, once MoveForwardBlock returns, so the drain must not
// hold that up.
//
// The drain here completes a subtree, and the announcement of that subtree
// blocks on the unbuffered channel until the test reads it. The response must
// arrive while the drain is still blocked there.
func TestHandleMoveForwardRequest_RespondsBeforeQueueDrain(t *testing.T) {
	stp, block, newSubtreeChan := newDeferredDrainProcessor(t, nil)

	inBlock, err := chainhash.NewHashFromStr("6affcabb2013261e764a5d4286b463b11127f4fd1de05368351530ddb3f19942")
	require.NoError(t, err)

	// One tx that is in the block, and enough that are not to complete a
	// 4-leaf subtree (the coinbase placeholder takes the first leaf).
	nodes := []subtreepkg.Node{{Hash: *inBlock, Fee: 1, SizeInBytes: 250}}
	notInBlock := make([]chainhash.Hash, 0, 5)

	for i := 0; i < 5; i++ {
		h := chainhash.HashH([]byte{byte(i), 0xde, 0xfe, 0x22})
		notInBlock = append(notInBlock, h)
		nodes = append(nodes, subtreepkg.Node{Hash: h, Fee: 1, SizeInBytes: 250})
	}

	inpoints := make([]*subtreepkg.TxInpoints, len(nodes))
	for i := range inpoints {
		inpoints[i] = &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{chainhash.HashH([]byte{byte(i), 0x01})}}
	}

	stp.AddBatch(nodes, inpoints)

	// The drain only takes batches enqueued before it starts.
	time.Sleep(10 * time.Millisecond)

	errChan := make(chan error, 1)

	var handlerDone sync.WaitGroup

	handlerDone.Go(func() {
		stp.handleMoveForwardRequest(context.Background(), moveBlockRequest{block: block, errChan: errChan})
	})

	select {
	case err := <-errChan:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("moveForwardBlock result was held back by the queue drain")
	}

	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()

	handlerDone.Wait()

	hashes := subtreeHashes(stp)

	_, found := hashes[*inBlock]
	require.False(t, found, "a queued tx that is in the block must not be re-added")

	for _, h := range notInBlock {
		_, found = hashes[h]
		require.True(t, found, "queued tx %s is not in the block and must be added", h)
	}

	require.Zero(t, stp.queue.length(), "the drain must empty the queue")
	require.Zero(t, stp.currentTxMapShadow.Length(), "the retired tx map half must be cleared after the drain")

	leaves := len(stp.currentSubtree.Load().Nodes)
	for _, st := range stp.chainedSubtrees {
		leaves += len(st.Nodes)
	}

	require.Equal(t, uint64(leaves), stp.TxCount(), "the tx count must include the txs the drain added") //nolint:gosec // small test count
}

// TestHandleMoveForwardRequest_FailedBlockLeavesQueueUntouched pins the #852
// fix on the dispatcher path: the queue is drained only after the block is
// applied, so a block that fails after the leftover pass (here: creating the
// coinbase UTXOs) leaves every queued batch in the queue, instead of having
// drained batches that the rollback then cannot put back.
func TestHandleMoveForwardRequest_FailedBlockLeavesQueueUntouched(t *testing.T) {
	boom := errors.NewStorageError("create failed")

	stp, block, _ := newDeferredDrainProcessor(t, func(s utxostore.Store) utxostore.Store {
		return &errOnCreateUtxoStore{Store: s, err: boom}
	})

	queued := chainhash.HashH([]byte("queued-before-failed-block"))
	stp.AddBatch([]subtreepkg.Node{{Hash: queued, Fee: 1, SizeInBytes: 250}}, []*subtreepkg.TxInpoints{{}})

	// The drain only takes batches enqueued before it starts.
	time.Sleep(10 * time.Millisecond)

	errChan := make(chan error, 1)
	stp.handleMoveForwardRequest(context.Background(), moveBlockRequest{block: block, errChan: errChan})

	require.ErrorIs(t, <-errChan, boom)
	require.Equal(t, int64(1), stp.queue.length(), "a failed block must not drain the queue")

	_, found := subtreeHashes(stp)[queued]
	require.False(t, found, "the queued tx must still be waiting in the queue, not in a subtree")
}
