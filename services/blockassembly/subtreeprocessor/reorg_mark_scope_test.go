package subtreeprocessor

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// scopeMarkRecorder records the hashes of every MarkTransactionsOnLongestChain call, per
// direction, and of every BatchDecorate read, and then applies the call to the real store underneath.
type scopeMarkRecorder struct {
	utxostore.Store

	mu        sync.Mutex
	markFalse map[chainhash.Hash]struct{}
	markTrue  map[chainhash.Hash]struct{}
	gets      map[chainhash.Hash]struct{}
}

func newScopeMarkRecorder(store utxostore.Store) *scopeMarkRecorder {
	return &scopeMarkRecorder{
		Store:     store,
		markFalse: make(map[chainhash.Hash]struct{}),
		markTrue:  make(map[chainhash.Hash]struct{}),
		gets:      make(map[chainhash.Hash]struct{}),
	}
}

func (s *scopeMarkRecorder) BatchDecorate(ctx context.Context, unresolvedMetaDataSlice []*utxostore.UnresolvedMetaData, fields ...fields.FieldName) error {
	s.mu.Lock()
	for _, item := range unresolvedMetaDataSlice {
		s.gets[item.Hash] = struct{}{}
	}
	s.mu.Unlock()

	return s.Store.BatchDecorate(ctx, unresolvedMetaDataSlice, fields...)
}

func (s *scopeMarkRecorder) gotten(hash chainhash.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.gets[hash]

	return ok
}

func (s *scopeMarkRecorder) MarkTransactionsOnLongestChain(ctx context.Context, txHashes []chainhash.Hash, onLongestChain bool, opts ...utxostore.MarkLongestChainOption) error {
	s.mu.Lock()
	target := s.markFalse
	if onLongestChain {
		target = s.markTrue
	}

	for _, hash := range txHashes {
		target[hash] = struct{}{}
	}
	s.mu.Unlock()

	return s.Store.MarkTransactionsOnLongestChain(ctx, txHashes, onLongestChain, opts...)
}

func (s *scopeMarkRecorder) markedFalse(hash chainhash.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.markFalse[hash]

	return ok
}

func (s *scopeMarkRecorder) markFalseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.markFalse)
}

// createScopeTestTx creates a tx spending coinbaseTx output vout, at height 1. With no
// options it is an unmined mempool tx (unmined_since set).
func createScopeTestTx(t *testing.T, ctx context.Context, store utxostore.Store, vout uint32, opts ...utxostore.CreateOption) chainhash.Hash {
	t.Helper()

	tx := bt.NewTx()
	require.NoError(t, tx.From(coinbaseTx.TxIDChainHash().String(), vout,
		coinbaseTx.Outputs[vout].LockingScript.String(), coinbaseTx.Outputs[vout].Satoshis))
	require.NoError(t, tx.AddP2PKHOutputFromAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 100000000))
	tx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})

	_, _, err := store.SpendAndCreate(ctx, tx, 1, append([]utxostore.CreateOption{utxostore.WithCreateOnly()}, opts...)...)
	require.NoError(t, err)

	return *tx.TxIDChainHash()
}

// storeScopeTestSubtree stores a subtree of [coinbase, hashes...] and returns its key.
func storeScopeTestSubtree(t *testing.T, ctx context.Context, blobStore *blob_memory.Memory, hashes ...chainhash.Hash) *chainhash.Hash {
	t.Helper()

	st, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())

	for i, hash := range hashes {
		require.NoError(t, st.AddSubtreeNode(subtreepkg.Node{Hash: hash, Fee: uint64(100 + i), SizeInBytes: 250})) //nolint:gosec
	}

	stBytes, err := st.Serialize()
	require.NoError(t, err)

	metaBytes, err := createSubtreeMeta(t, st).Serialize()
	require.NoError(t, err)

	key := st.RootHash()
	require.NoError(t, blobStore.Set(ctx, key[:], fileformat.FileTypeSubtree, stBytes))
	require.NoError(t, blobStore.Set(ctx, key[:], fileformat.FileTypeSubtreeMeta, metaBytes))

	return key
}

// TestReorgBlocks_MarkFalseScope pins issue 2051: a reorg marks not-on-longest-chain only
// the txs whose chain state it changes (moved back and not moved forward), never the whole
// block assembly mempool.
//
// Scenario, 1 back / 1 forward:
//
//	moveBack block A:    a1, shared   (mined on the old chain)
//	moveForward block B: shared, b1   (validated as a side chain, so b1 is still unmined)
//	mempool before:      m1, m2       (unmined)
func TestReorgBlocks_MarkFalseScope(t *testing.T) {
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)
	sqlStore, err := sql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	store := newScopeMarkRecorder(sqlStore)
	blobStore := blob_memory.New()

	const blockAID = uint32(10)

	a1 := createScopeTestTx(t, ctx, store, 0, utxostore.WithMinedBlockInfo(utxostore.MinedBlockInfo{BlockID: blockAID, BlockHeight: 2, OnLongestChain: true}))
	shared := createScopeTestTx(t, ctx, store, 1, utxostore.WithMinedBlockInfo(utxostore.MinedBlockInfo{BlockID: blockAID, BlockHeight: 2, OnLongestChain: true}))
	b1 := createScopeTestTx(t, ctx, store, 2)
	m1 := createScopeTestTx(t, ctx, store, 3)
	m2 := createScopeTestTx(t, ctx, store, 4)

	mempoolBefore := map[chainhash.Hash]uint32{}
	for _, hash := range []chainhash.Hash{m1, m2} {
		m, err := store.Get(ctx, &hash, fields.UnminedSince)
		require.NoError(t, err)
		require.NotZero(t, m.UnminedSince, "precondition: mempool tx is unmined")
		mempoolBefore[hash] = m.UnminedSince
	}

	_, err = sqlStore.Create(ctx, coinbaseTx2, 2)
	require.NoError(t, err)
	_, err = sqlStore.Create(ctx, coinbaseTx3, 2)
	require.NoError(t, err)

	blockA := &model.Block{
		ID:         blockAID,
		Height:     2,
		CoinbaseTx: coinbaseTx2,
		Subtrees:   []*chainhash.Hash{storeScopeTestSubtree(t, ctx, blobStore, a1, shared)},
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  prevBlockHeader.Hash(),
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      3000000001,
			Bits:           model.NBit{},
			Nonce:          501,
		},
	}

	blockB := &model.Block{
		ID:         blockAID + 1,
		Height:     2,
		CoinbaseTx: coinbaseTx3,
		Subtrees:   []*chainhash.Hash{storeScopeTestSubtree(t, ctx, blobStore, shared, b1)},
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  prevBlockHeader.Hash(),
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      3000000002,
			Bits:           model.NBit{},
			Nonce:          502,
		},
	}

	mockBlockchainClient := &blockchain.Mock{}
	mockBlockchainClient.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{}, nil)
	mockBlockchainClient.On("SetBlockProcessedAt", mock.Anything, mock.AnythingOfType("*chainhash.Hash"), mock.AnythingOfType("[]bool")).Return(nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, mock.Anything).Return(prevBlockHeader, &model.BlockHeaderMeta{}, nil)
	mockBlockchainClient.On("GetBlockIsMined", mock.Anything, mock.Anything).Return(true, nil)

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	t.Cleanup(func() { close(newSubtreeChan) })

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, tSettings, blobStore, mockBlockchainClient, store, newSubtreeChan)
	require.NoError(t, err)
	stp.Start(ctx)

	stp.InitCurrentBlockHeader(blockA.Header)

	stp.AddBatch([]subtreepkg.Node{{Hash: m1, Fee: 300, SizeInBytes: 250}, {Hash: m2, Fee: 301, SizeInBytes: 250}},
		[]*subtreepkg.TxInpoints{{}, {}})

	require.Eventually(t, func() bool {
		txMap := stp.GetCurrentTxMap()
		return txMap.Exists(m1) && txMap.Exists(m2)
	}, 5*time.Second, 10*time.Millisecond, "mempool txs must be in block assembly before the reorg")

	require.NoError(t, stp.Reorg([]*model.Block{blockA}, []*model.Block{blockB}))
	require.Equal(t, blockB.Header.Hash(), stp.GetCurrentBlockHeader().Hash())

	finalTxMap := stp.GetCurrentTxMap()
	require.True(t, finalTxMap.Exists(a1), "a1 is back in the mempool after its block was moved back")
	require.True(t, finalTxMap.Exists(m1))
	require.True(t, finalTxMap.Exists(m2))

	// The mark-false set is exactly what the reorg moved off the chain.
	require.True(t, store.markedFalse(a1), "a moved-back tx not in the new chain must be marked not on the longest chain")
	require.False(t, store.markedFalse(shared), "a tx mined by the moveForward block must not be marked false")
	require.False(t, store.markedFalse(m1), "a pure mempool tx must not be re-marked on a reorg")
	require.False(t, store.markedFalse(m2), "a pure mempool tx must not be re-marked on a reorg")
	require.Equal(t, 1, store.markFalseCount(), "only a1 changed chain state in the not-on-longest-chain direction")

	// End state in the store.
	a1Meta, err := store.Get(ctx, &a1, fields.UnminedSince)
	require.NoError(t, err)
	require.NotZero(t, a1Meta.UnminedSince, "a1 is unmined again")

	b1Meta, err := store.Get(ctx, &b1, fields.UnminedSince)
	require.NoError(t, err)
	require.Zero(t, b1Meta.UnminedSince, "b1 is mined on the new longest chain")

	sharedMeta, err := store.Get(ctx, &shared, fields.UnminedSince)
	require.NoError(t, err)
	require.Zero(t, sharedMeta.UnminedSince, "shared stays mined")

	for hash, before := range mempoolBefore {
		m, err := store.Get(ctx, &hash, fields.UnminedSince)
		require.NoError(t, err)
		require.Equal(t, before, m.UnminedSince, "a pure mempool tx keeps its unmined_since")
	}
}

// TestReorgBlocks_InvalidMoveBackChecksOnlyItsOwnTxs covers the 1 back / 0 forward reorg of
// an invalid block (block 992 on 2026-10-26). The per-tx chain check must run over the
// invalid block's txs only, not every tx in block assembly: on that day it was ~880M
// per-tx store reads, and no block was produced while it ran.
//
//	moveBack block A (invalid): a1 (only in A), dup (also in valid block 5)
//	mempool before:             m1 (unmined)
func TestReorgBlocks_InvalidMoveBackChecksOnlyItsOwnTxs(t *testing.T) {
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)
	if tSettings.UtxoStore.MaxMinedRoutines < 4 {
		tSettings.UtxoStore.MaxMinedRoutines = 4
	}

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)
	sqlStore, err := sql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	store := newScopeMarkRecorder(sqlStore)
	blobStore := blob_memory.New()

	const (
		invalidBlockID = uint32(20)
		validBlockID   = uint32(5)
	)

	a1 := createScopeTestTx(t, ctx, store, 0, utxostore.WithMinedBlockInfo(utxostore.MinedBlockInfo{BlockID: invalidBlockID, BlockHeight: 2, OnLongestChain: true}))
	dup := createScopeTestTx(t, ctx, store, 1, utxostore.WithMinedBlockInfo(
		utxostore.MinedBlockInfo{BlockID: validBlockID, BlockHeight: 1, OnLongestChain: true},
		utxostore.MinedBlockInfo{BlockID: invalidBlockID, BlockHeight: 2, OnLongestChain: true},
	))
	m1 := createScopeTestTx(t, ctx, store, 2)

	_, err = sqlStore.Create(ctx, coinbaseTx2, 2)
	require.NoError(t, err)

	blockA := &model.Block{
		ID:         invalidBlockID,
		Height:     2,
		CoinbaseTx: coinbaseTx2,
		Subtrees:   []*chainhash.Hash{storeScopeTestSubtree(t, ctx, blobStore, a1, dup)},
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  prevBlockHeader.Hash(),
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      3000000011,
			Bits:           model.NBit{},
			Nonce:          511,
		},
	}

	mockBlockchainClient := &blockchain.Mock{}
	mockBlockchainClient.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{}, nil)
	mockBlockchainClient.On("SetBlockProcessedAt", mock.Anything, mock.AnythingOfType("*chainhash.Hash"), mock.AnythingOfType("[]bool")).Return(nil)
	mockBlockchainClient.On("GetBlockHeader", mock.Anything, mock.Anything).Return(prevBlockHeader, &model.BlockHeaderMeta{Invalid: true}, nil)
	mockBlockchainClient.On("GetBlockHeaders", mock.Anything, mock.Anything, mock.Anything).
		Return([]*model.BlockHeader{}, []*model.BlockHeaderMeta{{ID: validBlockID}}, nil)
	// Only reached for a tx in neither the invalid block alone nor a recent block, i.e. a
	// mempool tx that should never have been checked.
	mockBlockchainClient.On("CheckBlockIsInCurrentChain", mock.Anything, mock.Anything).Return(false, nil)
	mockBlockchainClient.On("GetBlock", mock.Anything, mock.Anything).Return(&model.Block{Header: prevBlockHeader, Height: 1}, nil)
	mockBlockchainClient.On("GetBlockIsMined", mock.Anything, mock.Anything).Return(true, nil)

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	t.Cleanup(func() { close(newSubtreeChan) })

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, tSettings, blobStore, mockBlockchainClient, store, newSubtreeChan)
	require.NoError(t, err)
	stp.Start(ctx)

	stp.InitCurrentBlockHeader(blockA.Header)

	stp.AddBatch([]subtreepkg.Node{{Hash: m1, Fee: 300, SizeInBytes: 250}}, []*subtreepkg.TxInpoints{{}})

	require.Eventually(t, func() bool {
		return stp.GetCurrentTxMap().Exists(m1)
	}, 5*time.Second, 10*time.Millisecond, "mempool tx must be in block assembly before the reorg")

	require.NoError(t, stp.Reorg([]*model.Block{blockA}, []*model.Block{}))

	require.True(t, store.gotten(a1), "the invalid block's txs are checked")
	require.True(t, store.gotten(dup), "the invalid block's txs are checked")
	require.False(t, store.gotten(m1), "a pure mempool tx must not be read by the invalid-block check")

	require.True(t, store.markedFalse(a1), "a tx only in the invalid block is not on the longest chain")
	require.False(t, store.markedFalse(dup), "a tx also in a valid block stays on the longest chain")
	require.False(t, store.markedFalse(m1), "a pure mempool tx must not be re-marked")
}
