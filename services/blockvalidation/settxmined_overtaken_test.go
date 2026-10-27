package blockvalidation

import (
	"context"
	"io"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	blobopts "github.com/bsv-blockchain/teranode/stores/blob/options"
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	blockchainoptions "github.com/bsv-blockchain/teranode/stores/blockchain/options"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// overtakeTestStore wraps the UTXO store so a test can act at precise points:
// afterSetMined runs once, right after the first SetMinedMulti for blockID,
// failMarkFalse makes the next N MarkTransactionsOnLongestChain(false) calls fail, and
// failAfterSetMined makes SetMinedMulti fail after its write landed: a setTxMined that
// cleared unmined_since and then failed, leaving the block's mined_set unwritten.
type overtakeTestStore struct {
	utxostore.Store

	blockID           uint32
	once              sync.Once
	afterSetMined     func()
	failMarkFalse     atomic.Int32
	failAfterSetMined atomic.Bool

	markedTrueMu sync.Mutex
	markedTrue   []chainhash.Hash // every hash passed to MarkTransactionsOnLongestChain(true)
}

func (s *overtakeTestStore) SetMinedMulti(ctx context.Context, hashes []*chainhash.Hash, minedBlockInfo utxostore.MinedBlockInfo) (map[chainhash.Hash][]uint32, error) {
	res, err := s.Store.SetMinedMulti(ctx, hashes, minedBlockInfo)
	if err == nil && s.failAfterSetMined.Load() {
		return nil, errors.NewStorageError("injected failure after the write")
	}

	if err == nil && s.afterSetMined != nil && minedBlockInfo.BlockID == s.blockID {
		s.once.Do(s.afterSetMined)
	}

	return res, err
}

func (s *overtakeTestStore) MarkTransactionsOnLongestChain(ctx context.Context, txHashes []chainhash.Hash, onLongestChain bool, opts ...utxostore.MarkLongestChainOption) error {
	if onLongestChain {
		s.markedTrueMu.Lock()
		s.markedTrue = append(s.markedTrue, txHashes...)
		s.markedTrueMu.Unlock()
	}

	if !onLongestChain && s.failMarkFalse.Load() > 0 {
		s.failMarkFalse.Add(-1)
		return errors.NewStorageError("injected mark failure")
	}

	return s.Store.MarkTransactionsOnLongestChain(ctx, txHashes, onLongestChain, opts...)
}

// coinbaseOnlyBlock builds a mined block with no subtrees on parent. tag makes the
// coinbase, and so the block, unique.
func coinbaseOnlyBlock(t *testing.T, parent *chainhash.Hash, nBits *model.NBit, height uint32, tag byte) *model.Block {
	t.Helper()

	coinbaseTx := bt.NewTx()
	require.NoError(t, coinbaseTx.From("0000000000000000000000000000000000000000000000000000000000000000", 0xffffffff, "", 0))
	coinbaseTx.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{0x03, byte(height), 0x00, 0x00, tag, '/', 'T', 'e', 's', 't'})
	require.NoError(t, coinbaseTx.AddP2PKHOutputFromAddress("1MUMxUTXcPQ1kAqB7MtJWneeAwVW4cHzzp", 50*100000000))

	header := mineBlockHeader(t, parent, coinbaseTx.TxIDChainHash(), nBits)

	block, err := model.NewBlock(header, coinbaseTx, []*chainhash.Hash{}, 1, uint64(coinbaseTx.Size()), height, 0) //nolint:gosec
	require.NoError(t, err)

	return block
}

// overtakeFixture is a chain with block B on genesis mining two mempool txs:
// bOnly (only ever in B) and shared (also mined by the competing chain).
type overtakeFixture struct {
	ctx                     context.Context
	tSettings               *settings.Settings
	store                   *overtakeTestStore
	blockchainClient        blockchain.ClientI
	subtreeStore            blob.Store
	txStore                 blob.Store
	subtreeValidationClient subtreevalidation.Interface
	nBits                   *model.NBit
	blockB                  *model.Block
	blockBID                uint32
	bOnlyTx                 *bt.Tx
	bOnly                   *chainhash.Hash
	shared                  *chainhash.Hash
}

func newOvertakeFixture(t *testing.T) *overtakeFixture {
	t.Helper()
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	innerUtxoStore, subtreeValidationClient, _, txStore, subtreeStore, deferFunc := setup(t)
	t.Cleanup(deferFunc)

	tSettings := test.CreateBaseTestSettings(t)
	blockChainStore, err := blockchain_store.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)
	blockchainClient, err := blockchain.NewLocalClient(ulogger.TestLogger{}, tSettings, blockChainStore, nil, nil)
	require.NoError(t, err)

	// Two unrelated mempool txs, created unmined. The sql store stamps unmined_since with
	// the create height, so create above 0 to make "unmined" observable.
	coinbaseB, bOnlyTx, _, _ := createCoinbaseAndChildTx(t)
	_, sharedTx, _, _ := createCoinbaseAndChildTx(t)

	_, _, err = innerUtxoStore.SpendAndCreate(ctx, bOnlyTx, 1, utxostore.WithCreateOnly())
	require.NoError(t, err)
	_, _, err = innerUtxoStore.SpendAndCreate(ctx, sharedTx, 1, utxostore.WithCreateOnly())
	require.NoError(t, err)

	subtree, err := subtreepkg.NewTreeByLeafCount(4)
	require.NoError(t, err)
	require.NoError(t, subtree.AddCoinbaseNode())
	require.NoError(t, subtree.AddNode(*bOnlyTx.TxIDChainHash(), uint64(bOnlyTx.Size()), 0))   //nolint:gosec
	require.NoError(t, subtree.AddNode(*sharedTx.TxIDChainHash(), uint64(sharedTx.Size()), 0)) //nolint:gosec

	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes))

	replicated := subtree.Duplicate()
	replicated.ReplaceRootNode(coinbaseB.TxIDChainHash(), 0, uint64(coinbaseB.Size())) //nolint:gosec

	nBits, err := model.NewNBitFromString("207fffff")
	require.NoError(t, err)

	headerB := mineBlockHeader(t, tSettings.ChainCfgParams.GenesisHash, replicated.RootHash(), nBits)
	totalSize := uint64(coinbaseB.Size() + bOnlyTx.Size() + sharedTx.Size())                                                            //nolint:gosec
	blockB, err := model.NewBlock(headerB, coinbaseB, []*chainhash.Hash{subtree.RootHash()}, uint64(subtree.Length()), totalSize, 1, 0) //nolint:gosec
	require.NoError(t, err)

	require.NoError(t, blockchainClient.AddBlock(ctx, blockB, "test", blockchainoptions.WithSubtreesSet(true)))

	_, metaB, err := blockchainClient.GetBlockHeader(ctx, blockB.Hash())
	require.NoError(t, err)

	return &overtakeFixture{
		ctx:                     ctx,
		tSettings:               tSettings,
		store:                   &overtakeTestStore{Store: innerUtxoStore, blockID: metaB.ID},
		blockchainClient:        blockchainClient,
		subtreeStore:            subtreeStore,
		txStore:                 txStore,
		subtreeValidationClient: subtreeValidationClient,
		nBits:                   nBits,
		blockB:                  blockB,
		blockBID:                metaB.ID,
		bOnlyTx:                 bOnlyTx,
		bOnly:                   bOnlyTx.TxIDChainHash(),
		shared:                  sharedTx.TxIDChainHash(),
	}
}

// newBlockValidation returns a BlockValidation whose background workers have stopped, so
// the test drives every repair slice itself (drainTipRepair) and sees them in order.
func (f *overtakeFixture) newBlockValidation(t *testing.T) *BlockValidation {
	t.Helper()

	ctx, stop := context.WithCancel(f.ctx)
	bv := f.newRunningBlockValidation(ctx)
	stop()
	bv.backgroundTasks.Wait()

	return bv
}

// newRunningBlockValidation returns a BlockValidation with its background workers running:
// the setMined worker runs the repair on its own, as in production.
func (f *overtakeFixture) newRunningBlockValidation(ctx context.Context) *BlockValidation {
	return NewBlockValidation(ctx, ulogger.TestLogger{}, f.tSettings, f.blockchainClient, f.subtreeStore, f.txStore, f.store, nil, f.subtreeValidationClient)
}

// drainTipRepair runs repair slices until none is pending, as the setMined worker would
// between blocks, and returns the number of slices. It stops at the first error.
func drainTipRepair(t *testing.T, bv *BlockValidation) (int, error) {
	t.Helper()

	for slices := 1; slices < 1000; slices++ {
		more, err := bv.tipRepairSlice(context.Background(), bv.blockchainClient)
		if err != nil || !more {
			return slices, err
		}
	}

	t.Fatal("repair did not settle")

	return 0, nil
}

// overtakeB adds the competing chain B' -> C' on genesis, which overtakes B, and returns C'.
// B' also mines shared, so shared is mined on the winning chain.
func (f *overtakeFixture) overtakeB(t *testing.T) *model.Block {
	t.Helper()

	blockC2, blockB2ID := f.overtakeBPendingSharedWrite(t)
	f.mineSharedIn(t, blockB2ID)

	return blockC2
}

// mineSharedIn writes shared as mined on the longest chain in blockID: the competing
// block's own setTxMined.
func (f *overtakeFixture) mineSharedIn(t *testing.T, blockID uint32) {
	t.Helper()

	_, err := f.store.Store.SetMinedMulti(f.ctx, []*chainhash.Hash{f.shared}, utxostore.MinedBlockInfo{
		BlockID:        blockID,
		BlockHeight:    1,
		SubtreeIdx:     0,
		OnLongestChain: true,
	})
	require.NoError(t, err)
}

// mineIn writes hash as mined on the longest chain in blockID.
func (f *overtakeFixture) mineIn(t *testing.T, hash *chainhash.Hash, blockID uint32) {
	t.Helper()

	_, err := f.store.Store.SetMinedMulti(f.ctx, []*chainhash.Hash{hash}, utxostore.MinedBlockInfo{
		BlockID:        blockID,
		BlockHeight:    1,
		SubtreeIdx:     0,
		OnLongestChain: true,
	})
	require.NoError(t, err)
}

// overtakeBPendingSharedWrite is overtakeB before B' has written shared as mined. It returns
// C' and B”s id.
func (f *overtakeFixture) overtakeBPendingSharedWrite(t *testing.T) (*model.Block, uint32) {
	t.Helper()

	// Header timestamps must not decrease along the chain.
	time.Sleep(10 * time.Millisecond)

	blockB2 := coinbaseOnlyBlock(t, f.tSettings.ChainCfgParams.GenesisHash, f.nBits, 1, 0xb2)
	blockC2 := coinbaseOnlyBlock(t, blockB2.Hash(), f.nBits, 2, 0xc2)

	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, blockB2, "test", blockchainoptions.WithSubtreesSet(true)))
	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, blockC2, "test", blockchainoptions.WithSubtreesSet(true)))

	_, metaB2, err := f.blockchainClient.GetBlockHeader(f.ctx, blockB2.Hash())
	require.NoError(t, err)

	require.False(t, f.onChain(t, f.blockBID), "precondition: the competing chain overtook B")

	return blockC2, metaB2.ID
}

// extendB makes B's chain the longest again: B -> D2 -> D3 outworks B' -> C'.
func (f *overtakeFixture) extendB(t *testing.T) {
	t.Helper()

	time.Sleep(10 * time.Millisecond)

	blockD2 := coinbaseOnlyBlock(t, f.blockB.Hash(), f.nBits, 2, 0xd2)
	blockD3 := coinbaseOnlyBlock(t, blockD2.Hash(), f.nBits, 3, 0xd3)

	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, blockD2, "test", blockchainoptions.WithSubtreesSet(true)))
	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, blockD3, "test", blockchainoptions.WithSubtreesSet(true)))

	require.True(t, f.onChain(t, f.blockBID), "precondition: B is back on the longest chain")
}

func (f *overtakeFixture) onChain(t *testing.T, blockID uint32) bool {
	t.Helper()

	onChain, err := f.blockchainClient.CheckBlockIsInCurrentChain(f.ctx, []uint32{blockID})
	require.NoError(t, err)

	return onChain
}

func (f *overtakeFixture) unminedSince(t *testing.T, hash *chainhash.Hash) uint32 {
	t.Helper()

	m, err := f.store.Store.Get(f.ctx, hash, fields.UnminedSince)
	require.NoError(t, err)

	return m.UnminedSince
}

// TestRepairOvertakenTips_OvertakenAfterSetMined pins the repair for a tip overtaken after
// its setTxMined finished (issue 2051).
//
// setTxMined clears unmined_since on the txs of the block that is the tip when it runs. If
// a competing chain overtakes that block before block assembly moves onto it, block
// assembly reaches the new tip by a pure catch-up, never sees the block as a moveBack
// block, and nothing in block assembly restores unmined_since. At scale block assembly lags
// by minutes, so this is the common case in a fork storm. The txs would sit in the mempool
// looking mined: prunable, and invisible to loadUnminedTransactions. The repair must
// re-mark the txs not mined on the new chain, and leave a tx the winning chain mined.
func TestRepairOvertakenTips_OvertakenAfterSetMined(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.True(t, f.onChain(t, f.blockBID), "precondition: B is the tip when setTxMined runs")
	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: setTxMined marked B's txs mined")

	_ = f.overtakeB(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)

	require.NotZero(t, f.unminedSince(t, f.bOnly), "a tx mined only in the overtaken block must be unmined again")
	require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
}

// TestRepairOvertakenTips_OvertakenDuringSetMined covers the overtake landing while B's own
// setTxMined is running: the write still uses onLongestChain=true, so B must be tracked.
func TestRepairOvertakenTips_OvertakenDuringSetMined(t *testing.T) {
	f := newOvertakeFixture(t)
	f.store.afterSetMined = func() { _ = f.overtakeB(t) }
	bv := f.newBlockValidation(t)

	require.True(t, f.onChain(t, f.blockBID), "precondition: B is the tip when setTxMined starts")
	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)

	bOnlyMeta, err := f.store.Store.Get(f.ctx, f.bOnly, fields.UnminedSince, fields.BlockIDs)
	require.NoError(t, err)
	require.Equal(t, []uint32{f.blockBID}, bOnlyMeta.BlockIDs)
	require.NotZero(t, bOnlyMeta.UnminedSince, "a tx mined only in the overtaken block must be unmined again")
	require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
}

// TestRepairOvertakenTips_DuplicateSetMinedKeepsTracking: a second setTxMined for B while
// B's write is in flight is turned away as a duplicate. That must not stop B being tracked:
// the write in flight is what clears its txs.
func TestRepairOvertakenTips_DuplicateSetMinedKeepsTracking(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	f.store.afterSetMined = func() {
		require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()), "the duplicate is skipped, not an error")
	}

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "B was still tracked, so it is repaired")
}

// TestRepairOvertakenTips_SharedTxWrittenAfterRepair: the other order for a tx B shares with
// the winning chain. The repair runs before the winning block's setTxMined has written the
// tx, so the tx still reads as only in B and is stamped; that setTxMined then clears it.
// Both run on the setMined worker, so they never overlap.
func TestRepairOvertakenTips_SharedTxWrittenAfterRepair(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_, blockB2ID := f.overtakeBPendingSharedWrite(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.shared), "precondition: shared read as only in B, so it was stamped")

	f.mineSharedIn(t, blockB2ID)

	require.Zero(t, f.unminedSince(t, f.shared), "the winning block's write leaves shared mined")
	require.NotZero(t, f.unminedSince(t, f.bOnly))
}

// TestRepairOvertakenTips_Slices: a block larger than one slice is repaired across several
// slices, each one a separate step of the setMined worker.
func TestRepairOvertakenTips_Slices(t *testing.T) {
	f := newOvertakeFixture(t)
	f.tSettings.BlockValidation.TipRepairSliceSize = 1
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	steps, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	// Slices are over subtree nodes, the coinbase placeholder included: start the job, one
	// step per node (the first also loads the subtree, the last finishes), find nothing left.
	require.Equal(t, 5, steps)

	require.NotZero(t, f.unminedSince(t, f.bOnly))
	require.Zero(t, f.unminedSince(t, f.shared))
}

// TestRepairOvertakenTips_RepairsAgainAfterFlipBack: a block whose repair had nothing to
// re-mark (the winning chain mined all its txs) and that then comes back onto the chain must
// be repaired again when it leaves a second time, for a chain that mines none of them.
func TestRepairOvertakenTips_RepairsAgainAfterFlipBack(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))

	_, blockB2ID := f.overtakeBPendingSharedWrite(t)
	f.mineSharedIn(t, blockB2ID)
	f.mineIn(t, f.bOnly, blockB2ID)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: B' mined every tx of B, so nothing was re-marked")

	f.extendB(t)

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)

	// A third chain, longer than B -> D2 -> D3, that mines neither tx.
	time.Sleep(10 * time.Millisecond)

	parent := f.tSettings.ChainCfgParams.GenesisHash
	for height := uint32(1); height <= 4; height++ {
		block := coinbaseOnlyBlock(t, parent, f.nBits, height, 0xe0+byte(height))
		require.NoError(t, f.blockchainClient.AddBlock(f.ctx, block, "test", blockchainoptions.WithSubtreesSet(true)))
		parent = block.Hash()
	}

	require.False(t, f.onChain(t, f.blockBID), "precondition: B is overtaken again")

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "B is repaired again: no block on the new chain mines bOnly")
}

// TestRepairOvertakenTips_RetriesAfterFailure: a failed repair leaves the block tracked and
// unrepaired, so the next slice retries it. No later setTxMined can do it: B's mined_set is
// already written, and a retried setTxMined would see onLongestChain=false.
func TestRepairOvertakenTips_RetriesAfterFailure(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	f.store.failMarkFalse.Store(1 << 30)

	for i := 0; i < 2*tipRepairMaxAttempts; i++ {
		_, err := drainTipRepair(t, bv)
		require.Error(t, err)
	}

	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: the failed repairs changed nothing")
	_, tracked := bv.minedOnChain.snapshot()[*f.blockB.Hash()]
	require.True(t, tracked, "a transient failure never gives the block up")

	f.store.failMarkFalse.Store(0)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the next attempt repairs the block")
}

// blockGoneClient answers GetBlock with block-not-found.
type blockGoneClient struct {
	blockchain.ClientI
}

func (c *blockGoneClient) GetBlock(_ context.Context, blockHash *chainhash.Hash) (*model.Block, error) {
	return nil, errors.NewBlockNotFoundError(blockHash.String())
}

// extraSubtreeClient returns blocks with an extra, never-stored subtree listed first.
type extraSubtreeClient struct {
	blockchain.ClientI
}

func (c *extraSubtreeClient) GetBlock(ctx context.Context, blockHash *chainhash.Hash) (*model.Block, error) {
	block, err := c.ClientI.GetBlock(ctx, blockHash)
	if err != nil {
		return nil, err
	}

	missing := chainhash.HashH([]byte("never stored"))

	return model.NewBlock(block.Header, block.CoinbaseTx, append([]*chainhash.Hash{&missing}, block.Subtrees...),
		block.TransactionCount, block.SizeInBytes, block.Height, block.ID)
}

// TestRepairOvertakenTips_SkipsMissingSubtree: a subtree that is gone must not stop the
// repair of the block's other subtrees, nor keep the block retrying.
func TestRepairOvertakenTips_SkipsMissingSubtree(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	bv.blockchainClient = &extraSubtreeClient{ClientI: f.blockchainClient}

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the readable subtree is repaired")
	require.True(t, bv.minedOnChain.snapshot()[*f.blockB.Hash()].done, "and the block is done, not retried")
}

// failingGetBlockClient fails GetBlock while fail is set: a blockchain service that is
// briefly unreachable.
type failingGetBlockClient struct {
	blockchain.ClientI
	fail atomic.Bool
}

func (c *failingGetBlockClient) GetBlock(ctx context.Context, blockHash *chainhash.Hash) (*model.Block, error) {
	if c.fail.Load() {
		return nil, errors.NewServiceError("blockchain service unavailable")
	}

	return c.ClientI.GetBlock(ctx, blockHash)
}

// TestRepairOvertakenTips_TransientLoadFailureNeverGivesUp: failing to load the overtaken
// block because a service is briefly unavailable must not count toward giving the block up.
func TestRepairOvertakenTips_TransientLoadFailureNeverGivesUp(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	client := &failingGetBlockClient{ClientI: f.blockchainClient}
	client.fail.Store(true)
	bv.blockchainClient = client

	for i := 0; i < 2*tipRepairMaxAttempts; i++ {
		_, err := drainTipRepair(t, bv)
		require.Error(t, err)
	}

	_, tracked := bv.minedOnChain.snapshot()[*f.blockB.Hash()]
	require.True(t, tracked, "a transient failure never gives the block up")

	client.fail.Store(false)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly))
}

// TestRepairOvertakenTips_GivesUpAfterMaxAttempts: a block that can never be repaired (it is
// gone from the blockchain store) must not be retried, and logged, forever.
func TestRepairOvertakenTips_GivesUpAfterMaxAttempts(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	bv.blockchainClient = &blockGoneClient{ClientI: f.blockchainClient}

	for i := 0; i < tipRepairMaxAttempts; i++ {
		_, err := drainTipRepair(t, bv)
		require.Error(t, err, "attempt %d", i+1)
	}

	_, tracked := bv.minedOnChain.snapshot()[*f.blockB.Hash()]
	require.False(t, tracked, "the block is dropped after the last attempt")

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err, "and no longer retried")
}

// TestRepairOvertakenTips_FlipBackRemines: a repaired block whose chain becomes the longest
// again must have its txs marked mined again.
func TestRepairOvertakenTips_FlipBackRemines(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "precondition: repaired while B is off chain")

	f.extendB(t)

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)
	require.Zero(t, f.unminedSince(t, f.bOnly), "B is on the longest chain again, so its txs are mined again")
	require.Equal(t, []chainhash.Hash{*f.bOnly}, f.store.markedTrue, "only the tx the repair stamped is written back")
}

// TestRepairOvertakenTips_SeedAfterRestart: the tracker is in memory. A block written mined
// before a restart and found off chain after it must still be repaired. The seed runs at the
// first slice after the restarted process marks a block mined, here the new tip C'.
func TestRepairOvertakenTips_SeedAfterRestart(t *testing.T) {
	f := newOvertakeFixture(t)

	require.NoError(t, f.newBlockValidation(t).setTxMinedStatus(f.ctx, f.blockB.Hash()))
	blockC2 := f.overtakeB(t)

	restarted := f.newBlockValidation(t)

	_, err := drainTipRepair(t, restarted)
	require.NoError(t, err)
	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: nothing tracked yet, so no repair has run")

	require.NoError(t, restarted.setTxMinedStatus(f.ctx, blockC2.Hash()))

	_, err = drainTipRepair(t, restarted)
	require.NoError(t, err)

	require.NotZero(t, f.unminedSince(t, f.bOnly), "a block overtaken across a restart must be repaired")
	require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
}

// TestRepairOvertakenTips_PartialWriteSeededBeforeRetry: B's setTxMined clears its txs and
// then fails, so B's mined_set stays unwritten. The node restarts, the competing chain
// overtakes B, and the restarted process seeds the tracker before B's retry. The retry runs
// with onLongestChain=false, which leaves unmined_since alone, so the cleared txs would keep
// looking mined unless the seed tracks B although its mined_set is not written.
func TestRepairOvertakenTips_PartialWriteSeededBeforeRetry(t *testing.T) {
	f := newOvertakeFixture(t)

	f.store.failAfterSetMined.Store(true)
	require.Error(t, f.newBlockValidation(t).setTxMinedStatus(f.ctx, f.blockB.Hash()))
	f.store.failAfterSetMined.Store(false)

	_, metaB, err := f.blockchainClient.GetBlockHeader(f.ctx, f.blockB.Hash())
	require.NoError(t, err)
	require.False(t, metaB.MinedSet, "precondition: B's mined_set is not written")
	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: the failed write cleared B's txs")

	blockC2 := f.overtakeB(t)

	restarted := f.newBlockValidation(t)

	require.NoError(t, restarted.setTxMinedStatus(f.ctx, blockC2.Hash()))
	_, err = drainTipRepair(t, restarted)
	require.NoError(t, err)

	require.NoError(t, restarted.setTxMinedStatus(f.ctx, f.blockB.Hash()), "B's retry, now off the longest chain")
	_, err = drainTipRepair(t, restarted)
	require.NoError(t, err)

	require.NotZero(t, f.unminedSince(t, f.bOnly), "a tx cleared by the failed write must be unmined again")
	require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
}

// TestRepairOvertakenTips_SkipsPrunedTxs: a tx of the overtaken block that is no longer in
// the store (pruned) must not stop the repair of the others.
func TestRepairOvertakenTips_SkipsPrunedTxs(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	require.NoError(t, f.store.Store.Delete(f.ctx, f.shared))

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the remaining txs are repaired")
}

// TestRepairOvertakenTips_StillOnChainNoRemark is the control: when B stays the tip its txs
// stay mined.
func TestRepairOvertakenTips_StillOnChainNoRemark(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)

	require.Zero(t, f.unminedSince(t, f.bOnly), "a tx mined in the tip must stay mined")
}

// TestRepairOvertakenTips_RunsOnTheSetMinedWorker drives the production path end to end, with
// no test call into setTxMined or the repair: the running setMined worker marks B mined,
// the overtaking blocks' notifications ask it for repair slices, and it repairs B.
func TestRepairOvertakenTips_RunsOnTheSetMinedWorker(t *testing.T) {
	f := newOvertakeFixture(t)
	_ = f.newRunningBlockValidation(f.ctx)

	// The overtaking blocks' notifications must reach the worker, so wait for it to subscribe.
	require.Eventually(t, func() bool {
		subscribers, err := f.blockchainClient.GetSubscribers(f.ctx)
		return err == nil && slices.Contains(subscribers, blockchain.SubscriberBlockValidation)
	}, 10*time.Second, 10*time.Millisecond)

	// The worker's mined-not-set pass picks B up and writes it as on the longest chain.
	require.Eventually(t, func() bool {
		_, meta, err := f.blockchainClient.GetBlockHeader(f.ctx, f.blockB.Hash())
		return err == nil && meta.MinedSet
	}, 10*time.Second, 10*time.Millisecond, "the setMined worker marks B mined")
	require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: B's txs are mined")

	_ = f.overtakeB(t)

	require.Eventually(t, func() bool {
		m, err := f.store.Store.Get(f.ctx, f.bOnly, fields.UnminedSince)
		return err == nil && m.UnminedSince != 0
	}, 10*time.Second, 20*time.Millisecond, "the setMined worker repairs the overtaken block")

	require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
}

// grpcLikeBestHeaderClient returns GetBestBlockHeader metas without the block id, as the
// gRPC blockchain client does.
type grpcLikeBestHeaderClient struct {
	blockchain.ClientI
}

func (c *grpcLikeBestHeaderClient) GetBestBlockHeader(ctx context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	header, meta, err := c.ClientI.GetBestBlockHeader(ctx)
	if meta != nil {
		stripped := *meta
		stripped.ID = 0
		meta = &stripped
	}

	return header, meta, err
}

// TestMainChainIDs_ExtensionWithoutBestHeaderID: when the best block only extends the one
// seen last time, its id must still be added to the main-chain set even though
// GetBestBlockHeader over gRPC carries no id. Adding id 0 instead made the new tip look off
// the chain, and the repair stamped its txs unmined.
func TestMainChainIDs_ExtensionWithoutBestHeaderID(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)
	client := &grpcLikeBestHeaderClient{ClientI: f.blockchainClient}

	_, _, err := bv.mainChainIDsLocked(f.ctx, client)
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	child := coinbaseOnlyBlock(t, f.blockB.Hash(), f.nBits, 2, 0xa2)
	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, child, "test", blockchainoptions.WithSubtreesSet(true)))

	_, childMeta, err := f.blockchainClient.GetBlockHeader(f.ctx, child.Hash())
	require.NoError(t, err)

	onMainChain, _, err := bv.mainChainIDsLocked(f.ctx, client)
	require.NoError(t, err)
	require.True(t, onMainChain[childMeta.ID], "the new tip is on the main chain")
	require.True(t, onMainChain[f.blockBID])
}

func TestTipRepairExpired(t *testing.T) {
	// GetBlockHeaderIDs(best, tipRepairWindow) covers best down to best-tipRepairWindow+1.
	require.False(t, tipRepairExpired(1001, 2000), "inside the main chain ID window")
	require.True(t, tipRepairExpired(1000, 2000), "outside it: treating it as off chain would read every tx")
	require.False(t, tipRepairExpired(5, 10), "young chain")
}

func TestMinedOnChainTracker_NewWriteStartsOver(t *testing.T) {
	tracker := newMinedOnChainTracker()
	hash := chainhash.HashH([]byte("b"))

	tracker.track(hash, 1, 1)
	require.True(t, tracker.markWritten(hash))

	gen := tracker.snapshot()[hash].gen
	tracker.update(hash, gen, func(e *minedOnChainBlock) {
		e.stamped = true
		e.done = true
	})

	// setTxMined writes the block as on the longest chain again (its mined_set was cleared).
	tracker.track(hash, 1, 1)

	b := tracker.snapshot()[hash]
	require.NotEqual(t, gen, b.gen, "a new write is a new generation; a repair in progress for the old one is abandoned")
	require.False(t, b.done, "the new write clears the txs again, so the block needs repairing again")
	require.False(t, b.stamped)

	tracker.update(hash, gen, func(e *minedOnChainBlock) { e.done = true })
	require.False(t, tracker.snapshot()[hash].done, "an update for the old generation is ignored")
}

func TestMinedOnChainTracker_Nil(t *testing.T) {
	var tracker *minedOnChainTracker
	hash := chainhash.HashH([]byte("b"))

	tracker.track(hash, 1, 1)
	tracker.seed(hash, 1, 1)
	require.False(t, tracker.markWritten(hash))
	tracker.update(hash, 1, func(e *minedOnChainBlock) { e.done = true })
	tracker.drop(hash)
	require.True(t, tracker.empty())
	require.Nil(t, tracker.snapshot())
}

// TestRepairOvertakenTips_DefaultSliceSize: an unset slice size falls back to the default.
func TestRepairOvertakenTips_DefaultSliceSize(t *testing.T) {
	f := newOvertakeFixture(t)
	f.tSettings.BlockValidation.TipRepairSliceSize = 0
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly))
}

// TestRunTipRepairSlice_RetriesAfterError: a failed slice leaves the retry flag set, so the
// periodic tick asks again; the next good slice clears it.
func TestRunTipRepairSlice_RetriesAfterError(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	client := &failingGetBlockClient{ClientI: f.blockchainClient}
	client.fail.Store(true)

	bv.runTipRepairSlice(f.ctx, client)
	require.True(t, bv.tipRepairRetry.Load())

	client.fail.Store(false)

	for i := 0; i < 10 && bv.tipRepairPending.Swap(false); i++ {
		bv.runTipRepairSlice(f.ctx, client)
	}

	bv.runTipRepairSlice(f.ctx, client)
	require.False(t, bv.tipRepairRetry.Load())
	require.NotZero(t, f.unminedSince(t, f.bOnly))
}

// seedFailClient fails the one blockchain call named by failOn.
type seedFailClient struct {
	blockchain.ClientI
	failOn string
}

func (c *seedFailClient) err(name string) error {
	if c.failOn == name {
		return errors.NewServiceError("injected %s failure", name)
	}

	return nil
}

func (c *seedFailClient) GetBestBlockHeader(ctx context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	if err := c.err("GetBestBlockHeader"); err != nil {
		return nil, nil, err
	}

	return c.ClientI.GetBestBlockHeader(ctx)
}

func (c *seedFailClient) GetBlockHeaders(ctx context.Context, hash *chainhash.Hash, n uint64) ([]*model.BlockHeader, []*model.BlockHeaderMeta, error) {
	if err := c.err("GetBlockHeaders"); err != nil {
		return nil, nil, err
	}

	return c.ClientI.GetBlockHeaders(ctx, hash, n)
}

func (c *seedFailClient) GetLastNBlocks(ctx context.Context, n int64, includeOrphans bool, fromHeight uint32) ([]*model.BlockInfo, error) {
	if err := c.err("GetLastNBlocks"); err != nil {
		return nil, err
	}

	return c.ClientI.GetLastNBlocks(ctx, n, includeOrphans, fromHeight)
}

func (c *seedFailClient) GetBlockHeader(ctx context.Context, hash *chainhash.Hash) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	if err := c.err("GetBlockHeader"); err != nil {
		return nil, nil, err
	}

	return c.ClientI.GetBlockHeader(ctx, hash)
}

func (c *seedFailClient) GetBlockHeaderIDs(ctx context.Context, hash *chainhash.Hash, n uint64) ([]uint32, error) {
	if err := c.err("GetBlockHeaderIDs"); err != nil {
		return nil, err
	}

	return c.ClientI.GetBlockHeaderIDs(ctx, hash, n)
}

// TestRepairOvertakenTips_SeedFailures: a blockchain call failing during the restart seed or
// the main-chain read fails the slice and leaves the seed to run again.
func TestRepairOvertakenTips_SeedFailures(t *testing.T) {
	for _, failOn := range []string{"GetBestBlockHeader", "GetBlockHeaders", "GetLastNBlocks", "GetBlockHeader", "GetBlockHeaderIDs"} {
		t.Run(failOn, func(t *testing.T) {
			f := newOvertakeFixture(t)
			bv := f.newBlockValidation(t)

			require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
			_ = f.overtakeB(t)

			// The seed must run in this slice.
			bv.tipRepairSeeded = false

			_, err := bv.tipRepairSlice(f.ctx, &seedFailClient{ClientI: f.blockchainClient, failOn: failOn})
			require.Error(t, err)

			_, err = drainTipRepair(t, bv)
			require.NoError(t, err)
			require.NotZero(t, f.unminedSince(t, f.bOnly), "the next slice repairs the block")
		})
	}
}

// farBestClient reports the best block as much higher than it is, so tracked blocks look
// older than the repair window.
type farBestClient struct {
	blockchain.ClientI
}

func (c *farBestClient) GetBestBlockHeader(ctx context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	header, meta, err := c.ClientI.GetBestBlockHeader(ctx)
	if meta != nil {
		far := *meta
		far.Height += 2 * tipRepairWindow
		meta = &far
	}

	return header, meta, err
}

// TestRepairOvertakenTips_ExpiredBlockDropped: a block that left the window before its repair
// is dropped, not repaired.
func TestRepairOvertakenTips_ExpiredBlockDropped(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	bv.blockchainClient = &farBestClient{ClientI: f.blockchainClient}

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.True(t, bv.minedOnChain.empty(), "the expired block is dropped")
	require.Zero(t, f.unminedSince(t, f.bOnly), "and not repaired")
}

// TestRepairOvertakenTips_InvalidBlockDropped: an invalidated block belongs to unsetMined,
// so the repair drops it.
func TestRepairOvertakenTips_InvalidBlockDropped(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))

	_, err := f.blockchainClient.InvalidateBlock(f.ctx, f.blockB.Hash())
	require.NoError(t, err)

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)

	_, tracked := bv.minedOnChain.snapshot()[*f.blockB.Hash()]
	require.False(t, tracked, "the invalid block is dropped")
}

// TestRepairOvertakenTips_SubtreeToCheckOnly: setTxMined reads a subtree from its
// .subtreeToCheck file when the .subtree file is not there, so the repair must too, or a
// block marked mined that way is never repaired.
func TestRepairOvertakenTips_SubtreeToCheckOnly(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	subtreeHash := f.blockB.Subtrees[0]
	subtreeBytes, err := f.subtreeStore.Get(f.ctx, subtreeHash[:], fileformat.FileTypeSubtree)
	require.NoError(t, err)
	require.NoError(t, f.subtreeStore.Set(f.ctx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes))
	require.NoError(t, f.subtreeStore.Del(f.ctx, subtreeHash[:], fileformat.FileTypeSubtree))

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the subtree is read from its .subtreeToCheck file")
}

// truncatingSubtreeStore returns a truncated reader for the next truncate reads: a subtree
// read while another service moves the file.
type truncatingSubtreeStore struct {
	blob.Store
	truncate atomic.Int32
}

func (s *truncatingSubtreeStore) GetIoReader(ctx context.Context, key []byte, fileType fileformat.FileType, opts ...blobopts.FileOption) (io.ReadCloser, error) {
	reader, err := s.Store.GetIoReader(ctx, key, fileType, opts...)
	if err != nil || s.truncate.Load() <= 0 {
		return reader, err
	}

	s.truncate.Add(-1)

	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(reader, 10), reader}, nil
}

// TestRepairOvertakenTips_TruncatedSubtreeReadRetried: a truncated subtree read must be
// retried, not taken for a subtree that is gone, which would skip its txs for good.
func TestRepairOvertakenTips_TruncatedSubtreeReadRetried(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	store := &truncatingSubtreeStore{Store: f.subtreeStore}
	store.truncate.Store(1)
	bv.subtreeStore = store

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the subtree is read again and repaired")
}

// TestRepairOvertakenTips_StuckJobYields: a job that keeps failing must not hold up the
// repair of other blocks.
func TestRepairOvertakenTips_StuckJobYields(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	f.store.failMarkFalse.Store(1 << 30)

	// B's job starts first and gets stuck.
	_, err := bv.tipRepairSlice(f.ctx, bv.blockchainClient)
	require.NoError(t, err)
	require.NotNil(t, bv.tipRepairJob)
	require.Equal(t, *f.blockB.Hash(), bv.tipRepairJob.hash)

	// A second overtaken block, written mined while it was on the chain. It has no txs, so
	// its repair finishes in one step.
	time.Sleep(10 * time.Millisecond)

	fork := coinbaseOnlyBlock(t, f.tSettings.ChainCfgParams.GenesisHash, f.nBits, 1, 0xf1)
	require.NoError(t, f.blockchainClient.AddBlock(f.ctx, fork, "test", blockchainoptions.WithSubtreesSet(true)))

	_, forkMeta, err := f.blockchainClient.GetBlockHeader(f.ctx, fork.Hash())
	require.NoError(t, err)
	require.False(t, f.onChain(t, forkMeta.ID))

	bv.minedOnChain.track(*fork.Hash(), forkMeta.ID, forkMeta.Height)
	require.True(t, bv.minedOnChain.markWritten(*fork.Hash()))

	for i := 0; i < 3*tipRepairMaxAttempts; i++ {
		_, _ = bv.tipRepairSlice(f.ctx, bv.blockchainClient)
	}

	require.True(t, bv.minedOnChain.snapshot()[*fork.Hash()].done, "the other block is repaired while B's job keeps failing")
	require.False(t, bv.minedOnChain.snapshot()[*f.blockB.Hash()].done, "B is not done")

	f.store.failMarkFalse.Store(0)

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "B is repaired once the failure clears")
}

// TestRepairOvertakenTips_RemineSkipsTxNotInBlock: a tx of a re-marked block that was pruned
// and created again unmined while the block was off the chain no longer carries the block's
// id. Marking it mined when the block comes back would leave it looking mined in no block.
func TestRepairOvertakenTips_RemineSkipsTxNotInBlock(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	_, err := drainTipRepair(t, bv)
	require.NoError(t, err)
	require.True(t, bv.minedOnChain.snapshot()[*f.blockB.Hash()].stamped, "precondition: B's txs were re-marked")

	require.NoError(t, f.store.Store.Delete(f.ctx, f.bOnly))
	_, _, err = f.store.Store.SpendAndCreate(f.ctx, f.bOnlyTx, 1, utxostore.WithCreateOnly())
	require.NoError(t, err)

	f.extendB(t)

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)

	m, err := f.store.Store.Get(f.ctx, f.bOnly, fields.UnminedSince, fields.BlockIDs)
	require.NoError(t, err)
	require.Empty(t, m.BlockIDs, "precondition: the re-created tx is in no block")
	require.NotZero(t, m.UnminedSince, "a tx in no block must stay unmined")
}

// TestRepairOvertakenTips_PartialWriteRetryKeepsFailing: B's setTxMined clears its txs and
// then fails, and every retry fails too, so B's mined_set is never written. Once the
// competing chain overtakes B, the cleared txs must be re-marked without waiting for a retry
// to succeed, whether the process kept B's tracking or restarted and seeded it.
func TestRepairOvertakenTips_PartialWriteRetryKeepsFailing(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in process", true: "after restart"}[restart], func(t *testing.T) {
			f := newOvertakeFixture(t)
			bv := f.newBlockValidation(t)

			f.store.failAfterSetMined.Store(true)
			require.Error(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
			require.Zero(t, f.unminedSince(t, f.bOnly), "precondition: the failed write cleared B's txs")

			blockC2 := f.overtakeB(t)

			if restart {
				bv = f.newBlockValidation(t)

				f.store.failAfterSetMined.Store(false)
				require.NoError(t, bv.setTxMinedStatus(f.ctx, blockC2.Hash()))
				f.store.failAfterSetMined.Store(true)
			}

			require.Error(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()), "B's retry, off the longest chain, fails again")

			_, err := drainTipRepair(t, bv)
			require.NoError(t, err)

			require.NotZero(t, f.unminedSince(t, f.bOnly), "a tx cleared by the failed write must be unmined again")
			require.Zero(t, f.unminedSince(t, f.shared), "a tx also mined on the winning chain must stay mined")
		})
	}
}

// failingSubtreeStore fails the next fail subtree reads with a storage error: a blob store
// that is briefly unavailable.
type failingSubtreeStore struct {
	blob.Store
	fail atomic.Int32
}

func (s *failingSubtreeStore) GetIoReader(ctx context.Context, key []byte, fileType fileformat.FileType, opts ...blobopts.FileOption) (io.ReadCloser, error) {
	if s.fail.Add(-1) >= 0 {
		return nil, errors.NewStorageError("injected subtree store failure")
	}

	return s.Store.GetIoReader(ctx, key, fileType, opts...)
}

// TestRepairOvertakenTips_TransientSubtreeReadErrorRetried: a subtree store that stays
// unavailable through every read attempt of a step must not get the subtree skipped as gone;
// the step is retried once the store is back.
func TestRepairOvertakenTips_TransientSubtreeReadErrorRetried(t *testing.T) {
	f := newOvertakeFixture(t)
	bv := f.newBlockValidation(t)

	require.NoError(t, bv.setTxMinedStatus(f.ctx, f.blockB.Hash()))
	_ = f.overtakeB(t)

	store := &failingSubtreeStore{Store: f.subtreeStore}
	store.fail.Store(tipRepairSubtreeReadAttempts)
	bv.subtreeStore = store

	_, err := drainTipRepair(t, bv)
	require.Error(t, err, "the step fails while the store is down")
	require.False(t, bv.minedOnChain.snapshot()[*f.blockB.Hash()].done, "the subtree is not skipped")

	_, err = drainTipRepair(t, bv)
	require.NoError(t, err)
	require.NotZero(t, f.unminedSince(t, f.bOnly), "the subtree is repaired once the store is back")
}
