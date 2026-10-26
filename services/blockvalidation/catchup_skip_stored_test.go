package blockvalidation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The scale-3 incident of 2026-10-26: a child went to catchup while its parent was being stored.
// By the time catchup reached its fetch phase the parent was stored, yet catchup moved the FSM to
// CATCHINGBLOCKS and downloaded the parent's 274 GB of subtree data for six minutes before
// ValidateBlock reported "tried to validate existing valid block".

// TestFetchAndValidateBlocks_AllHeadersStored_SkipsCatchingBlocks: when every block catchup was
// about to fetch is already stored as valid, it returns without touching the FSM.
func TestFetchAndValidateBlocks_AllHeadersStored_SkipsCatchingBlocks(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 3)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(true, nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, mock.Anything).Return(blocks[1].Header, &model.BlockHeaderMeta{Height: 1}, nil)

	catchupCtx := &CatchupContext{
		blockUpTo:    blocks[2],
		blockHeaders: []*model.BlockHeader{blocks[1].Header, blocks[2].Header},
	}

	require.NoError(t, server.fetchAndValidateBlocks(context.Background(), catchupCtx))
	mockBlockchainClient.AssertNotCalled(t, "CatchUpBlocks", mock.Anything)
	require.Empty(t, catchupCtx.blockHeaders)
}

// TestFetchAndValidateBlocks_DropsStoredPrefixOnly: stored blocks at the front are dropped, the
// rest are still caught up.
func TestFetchAndValidateBlocks_DropsStoredPrefixOnly(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 4)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, blocks[1].Header.Hash()).Return(true, nil)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, blocks[1].Header.Hash()).Return(blocks[1].Header, &model.BlockHeaderMeta{Height: 1}, nil)

	// Stop at the FSM transition: the test only needs to see what catchup would fetch.
	mockBlockchainClient.ExpectedCalls = filterMockCalls(mockBlockchainClient.ExpectedCalls, "CatchUpBlocks")
	mockBlockchainClient.On("CatchUpBlocks", mock.Anything).Return(errors.NewStateError("stop here"))

	catchupCtx := &CatchupContext{
		blockUpTo:    blocks[3],
		blockHeaders: []*model.BlockHeader{blocks[1].Header, blocks[2].Header, blocks[3].Header},
	}

	require.Error(t, server.fetchAndValidateBlocks(context.Background(), catchupCtx))
	require.Equal(t, []*model.BlockHeader{blocks[2].Header, blocks[3].Header}, catchupCtx.blockHeaders)
}

// TestFetchAndValidateBlocks_StoredInvalidBlockIsKept: a block stored as invalid is not skipped,
// so validation still returns the invalid verdict for it, as before.
func TestFetchAndValidateBlocks_StoredInvalidBlockIsKept(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 3)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(true, nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, mock.Anything).Return(blocks[1].Header, &model.BlockHeaderMeta{Height: 1, Invalid: true}, nil)
	mockBlockchainClient.ExpectedCalls = filterMockCalls(mockBlockchainClient.ExpectedCalls, "CatchUpBlocks")
	mockBlockchainClient.On("CatchUpBlocks", mock.Anything).Return(errors.NewStateError("stop here"))

	catchupCtx := &CatchupContext{
		blockUpTo:    blocks[2],
		blockHeaders: []*model.BlockHeader{blocks[1].Header, blocks[2].Header},
	}

	require.Error(t, server.fetchAndValidateBlocks(context.Background(), catchupCtx))
	require.Len(t, catchupCtx.blockHeaders, 2)
	mockBlockchainClient.AssertCalled(t, "CatchUpBlocks", mock.Anything)
}

// TestFetchAndValidateBlocks_ExistsCheckErrorHalts: a store error while checking for stored blocks
// aborts catchup instead of guessing.
func TestFetchAndValidateBlocks_ExistsCheckErrorHalts(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 3)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, errors.NewStorageError("db down"))

	catchupCtx := &CatchupContext{
		blockUpTo:    blocks[2],
		blockHeaders: []*model.BlockHeader{blocks[1].Header, blocks[2].Header},
	}

	err := server.fetchAndValidateBlocks(context.Background(), catchupCtx)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrStorageError))
	mockBlockchainClient.AssertNotCalled(t, "CatchUpBlocks", mock.Anything)
}

// TestBlockWorker_BlockStoredSinceHeaders_SkipsSubtreeFetch: a block stored after the header phase
// (by the normal path, while catchup was starting) is not prewarmed. It is still delivered, marked
// as stored, so ordered delivery does not stall.
func TestBlockWorker_BlockStoredSinceHeaders_SkipsSubtreeFetch(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 2)
	block := blocks[1]
	mockBlockchainClient.On("GetBlockExists", mock.Anything, block.Header.Hash()).Return(true, nil)

	var fetchCalls atomic.Int32
	server.fetchSubtreeDataForBlockFn = func(context.Context, *model.Block, string, string) (map[string]struct{}, map[chainhash.Hash]map[fileformat.FileType]struct{}, error) {
		fetchCalls.Add(1)
		return nil, nil, nil
	}

	workQueue := make(chan workItem, 1)
	resultQueue := make(chan resultItem, 1)
	workQueue <- workItem{block: block, index: 0}
	close(workQueue)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, server.blockWorker(ctx, 0, workQueue, resultQueue, "peer", "http://peer/", block))
	require.Zero(t, fetchCalls.Load(), "a stored block must not be prewarmed")

	result := <-resultQueue
	require.NoError(t, result.err)
	require.True(t, result.alreadyStored)
}

// TestBlockWorker_ExistsCheckError_FailsBlock: a store error on the exists check fails the block
// instead of fetching on a guess.
func TestBlockWorker_ExistsCheckError_FailsBlock(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 2)
	block := blocks[1]
	mockBlockchainClient.On("GetBlockExists", mock.Anything, block.Header.Hash()).Return(false, errors.NewStorageError("db down"))

	var fetchCalls atomic.Int32
	server.fetchSubtreeDataForBlockFn = func(context.Context, *model.Block, string, string) (map[string]struct{}, map[chainhash.Hash]map[fileformat.FileType]struct{}, error) {
		fetchCalls.Add(1)
		return nil, nil, nil
	}

	workQueue := make(chan workItem, 1)
	resultQueue := make(chan resultItem, 1)
	workQueue <- workItem{block: block, index: 0}
	close(workQueue)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, server.blockWorker(ctx, 0, workQueue, resultQueue, "peer", "http://peer/", block))
	require.Zero(t, fetchCalls.Load())

	result := <-resultQueue
	require.Error(t, result.err)
	require.True(t, errors.Is(result.err, errors.ErrStorageError))
}

// TestOrderedDelivery_CarriesAlreadyStored: the stored mark reaches validation, which skips quick
// validation for it.
func TestOrderedDelivery_CarriesAlreadyStored(t *testing.T) {
	server, _, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 2)
	resultQueue := make(chan resultItem, 1)
	validateBlocksChan := make(chan blockForValidation, 1)
	resultQueue <- resultItem{block: blocks[1], index: 0, alreadyStored: true}

	var size atomic.Int64
	size.Store(1)

	require.NoError(t, server.orderedDelivery(context.Background(), resultQueue, validateBlocksChan, 1, blocks[1], &size))

	item := <-validateBlocksChan
	require.True(t, item.alreadyStored)
}
