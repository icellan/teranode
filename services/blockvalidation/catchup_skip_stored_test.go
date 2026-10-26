package blockvalidation

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/jarcoal/httpmock"
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
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, errors.NewServiceError("blockchain service unavailable"))

	catchupCtx := &CatchupContext{
		blockUpTo:    blocks[2],
		blockHeaders: []*model.BlockHeader{blocks[1].Header, blocks[2].Header},
	}

	err := server.fetchAndValidateBlocks(context.Background(), catchupCtx)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrStorageError), "a local lookup failure is a storage fault, never the peer's")
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
	mockBlockchainClient.On("GetBlockExists", mock.Anything, block.Header.Hash()).Return(false, errors.NewServiceError("blockchain service unavailable"))

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
	require.True(t, errors.Is(result.err, errors.ErrStorageError), "a local lookup failure is a storage fault, never the peer's")
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

// TestFetchAndValidateBlocks_TrimmedPrefixKeepsHeights: blocks are numbered from the common
// ancestor by their position in the header list, so trimming the stored front must move the
// starting height with it. Without that, in the incident case the child gets its parent's height,
// fails the BIP34 coinbase-height check and is stored invalid.
func TestFetchAndValidateBlocks_TrimmedPrefixKeepsHeights(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 4)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, blocks[1].Header.Hash()).Return(true, nil)
	mockBlockchainClient.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, blocks[1].Header.Hash()).Return(blocks[1].Header, &model.BlockHeaderMeta{Height: 101}, nil)
	mockBlockchainClient.ExpectedCalls = filterMockCalls(mockBlockchainClient.ExpectedCalls, "CatchUpBlocks")
	mockBlockchainClient.On("CatchUpBlocks", mock.Anything).Return(errors.NewStateError("stop here"))

	catchupCtx := &CatchupContext{
		blockUpTo:          blocks[3],
		blockHeaders:       []*model.BlockHeader{blocks[1].Header, blocks[2].Header, blocks[3].Header},
		commonAncestorMeta: &model.BlockHeaderMeta{Height: 100},
	}

	require.Error(t, server.fetchAndValidateBlocks(context.Background(), catchupCtx))
	require.Equal(t, uint32(102), catchupFirstBlockHeight(catchupCtx), "the first remaining block sits one above the last stored one")
	require.Equal(t, uint32(100), catchupCtx.commonAncestorMeta.Height, "the common ancestor itself is reported unchanged")
}

// TestCatchupFirstBlockHeight_DefaultsToCommonAncestor: an untrimmed list starts one above the
// common ancestor, as before.
func TestCatchupFirstBlockHeight_DefaultsToCommonAncestor(t *testing.T) {
	catchupCtx := &CatchupContext{commonAncestorMeta: &model.BlockHeaderMeta{Height: 99}}
	require.Equal(t, uint32(100), catchupFirstBlockHeight(catchupCtx))
}

// TestValidateBlocksOnChannel_AlreadyStoredSkipsQuickValidation: a block the worker found stored
// goes to normal validation even when quick validation is on, and normal validation stops at the
// stored block.
func TestValidateBlocksOnChannel_AlreadyStoredSkipsQuickValidation(t *testing.T) {
	server, mockBlockchainClient, _, cleanup := setupTestCatchupServer(t)
	defer cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 2)
	block := blocks[1]
	block.Height = 1
	mockBlockchainClient.On("GetBlockExists", mock.Anything, block.Header.Hash()).Return(true, nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, block.Header.Hash()).Return(block.Header, &model.BlockHeaderMeta{Height: 1}, nil)

	catchupCtx := &CatchupContext{
		blockUpTo:               block,
		useQuickValidation:      true,
		highestCheckpointHeight: 1000,
	}

	validateBlocksChan := make(chan blockForValidation, 1)
	validateBlocksChan <- blockForValidation{block: block, alreadyStored: true}
	close(validateBlocksChan)

	var size atomic.Int64
	size.Store(1)

	quickWrites := make(chan *SubtreeWriteJob, 1)
	require.NoError(t, server.validateBlocksOnChannel(validateBlocksChan, context.Background(), catchupCtx, &size, quickWrites))
	require.Zero(t, size.Load())
	require.Empty(t, quickWrites, "a stored block must not go through quick validation")
}

// TestFetchBlocksConcurrently_NumbersFromFirstBlockHeight: once the stored front was trimmed,
// delivered blocks are numbered from firstBlockHeight, not from the common ancestor.
func TestFetchBlocksConcurrently_NumbersFromFirstBlockHeight(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	suite.MockBlockchain.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, nil).Maybe()
	defer suite.Cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 4)

	httpmock.ActivateNonDefault(util.HTTPClient())
	defer httpmock.DeactivateAndReset()

	// blocks[1] was stored and trimmed: the list starts at blocks[2].
	batch := bytes.Buffer{}
	for i := 3; i >= 2; i-- {
		blockBytes, err := blocks[i].Bytes()
		require.NoError(t, err)
		batch.Write(blockBytes)
	}

	httpmock.RegisterResponder("GET", fmt.Sprintf("http://test-peer/blocks/%s?n=%d", blocks[3].Header.Hash().String(), 2), httpmock.NewBytesResponder(200, batch.Bytes()))

	var size atomic.Int64
	size.Store(2)
	validateBlocksChan := make(chan blockForValidation, 2)

	catchupCtx := &CatchupContext{
		blockUpTo:          blocks[3],
		baseURL:            "http://test-peer",
		blockHeaders:       []*model.BlockHeader{blocks[2].Header, blocks[3].Header},
		commonAncestorMeta: &model.BlockHeaderMeta{Height: 99},
		firstBlockHeight:   101,
	}

	require.NoError(t, suite.Server.fetchBlocksConcurrently(suite.Ctx, catchupCtx, validateBlocksChan, &size))

	for i, want := range []uint32{101, 102} {
		select {
		case item := <-validateBlocksChan:
			require.Equal(t, want, item.block.Height, "block %d", i)
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for block %d", i)
		}
	}
}
