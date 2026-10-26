package blockvalidation

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
	blockchain_store "github.com/bsv-blockchain/teranode/stores/blockchain"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// overtakeOnSetMined runs fn once, right after the first SetMinedMulti call for blockID
// has been applied. It lets a test move the chain tip while setTxMined is in flight,
// which is the window between setTxMined's onLongestChain check and its completion.
type overtakeOnSetMined struct {
	utxostore.Store
	blockID uint32
	once    sync.Once
	fn      func()
}

func (s *overtakeOnSetMined) SetMinedMulti(ctx context.Context, hashes []*chainhash.Hash, minedBlockInfo utxostore.MinedBlockInfo) (map[chainhash.Hash][]uint32, error) {
	res, err := s.Store.SetMinedMulti(ctx, hashes, minedBlockInfo)
	if err == nil && minedBlockInfo.BlockID == s.blockID {
		s.once.Do(s.fn)
	}

	return res, err
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

// TestSetTxMinedStatus_BlockOvertakenDuringSetMined pins the repair for a tip that is
// overtaken while its setTxMined runs (issue 2051).
//
// setTxMined decides onLongestChain once, before it writes. If block B is the tip at that
// point, the write clears unmined_since on B's txs. If a competing chain then overtakes B
// before block assembly ever moves onto B, block assembly reaches the new tip by a pure
// catch-up and never sees B as a moveBack block, so nothing restores unmined_since on B's
// txs. They stay in block assembly's mempool looking mined, which makes them prunable and
// invisible to loadUnminedTransactions. setTxMined must notice B fell off the chain and
// re-mark the txs that are not mined on the new chain. A tx that B shares with the winning
// chain is mined there and must stay cleared.
func TestSetTxMinedStatus_BlockOvertakenDuringSetMined(t *testing.T) {
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	innerUtxoStore, subtreeValidationClient, _, txStore, subtreeStore, deferFunc := setup(t)
	defer deferFunc()

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

	for _, h := range []*chainhash.Hash{bOnlyTx.TxIDChainHash(), sharedTx.TxIDChainHash()} {
		m, err := innerUtxoStore.Get(ctx, h, fields.UnminedSince)
		require.NoError(t, err)
		require.NotZero(t, m.UnminedSince, "precondition: mempool tx is created unmined")
	}

	// Block B on genesis: coinbase, bOnly, shared.
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

	require.NoError(t, blockchainClient.AddBlock(ctx, blockB, "test"))

	_, metaB, err := blockchainClient.GetBlockHeader(ctx, blockB.Hash())
	require.NoError(t, err)

	onChain, err := blockchainClient.CheckBlockIsInCurrentChain(ctx, []uint32{metaB.ID})
	require.NoError(t, err)
	require.True(t, onChain, "precondition: B is the tip when setTxMined starts")

	// The competing chain B' -> C' on genesis, which overtakes B mid-setTxMined. B' also
	// mines sharedTx, so sharedTx is mined on the winning chain.
	blockB2 := coinbaseOnlyBlock(t, tSettings.ChainCfgParams.GenesisHash, nBits, 1, 0xb2)
	blockC2 := coinbaseOnlyBlock(t, blockB2.Hash(), nBits, 2, 0xc2)

	utxoStore := &overtakeOnSetMined{
		Store:   innerUtxoStore,
		blockID: metaB.ID,
		fn: func() {
			// Header timestamps must not decrease along the chain.
			time.Sleep(10 * time.Millisecond)

			require.NoError(t, blockchainClient.AddBlock(ctx, blockB2, "test"))
			require.NoError(t, blockchainClient.AddBlock(ctx, blockC2, "test"))

			_, metaB2, err := blockchainClient.GetBlockHeader(ctx, blockB2.Hash())
			require.NoError(t, err)

			_, err = innerUtxoStore.SetMinedMulti(ctx, []*chainhash.Hash{sharedTx.TxIDChainHash()}, utxostore.MinedBlockInfo{
				BlockID:        metaB2.ID,
				BlockHeight:    1,
				SubtreeIdx:     0,
				OnLongestChain: true,
			})
			require.NoError(t, err)
		},
	}

	blockValidation := NewBlockValidation(ctx, ulogger.TestLogger{}, tSettings, blockchainClient, subtreeStore, txStore, utxoStore, nil, subtreeValidationClient)

	require.NoError(t, blockValidation.setTxMinedStatus(ctx, blockB.Hash()))

	onChain, err = blockchainClient.CheckBlockIsInCurrentChain(ctx, []uint32{metaB.ID})
	require.NoError(t, err)
	require.False(t, onChain, "precondition: the competing chain overtook B during setTxMined")

	bOnlyMeta, err := innerUtxoStore.Get(ctx, bOnlyTx.TxIDChainHash(), fields.UnminedSince, fields.BlockIDs)
	require.NoError(t, err)
	require.Equal(t, []uint32{metaB.ID}, bOnlyMeta.BlockIDs)
	require.NotZero(t, bOnlyMeta.UnminedSince, "a tx mined only in the overtaken block must be unmined again")

	sharedMeta, err := innerUtxoStore.Get(ctx, sharedTx.TxIDChainHash(), fields.UnminedSince, fields.BlockIDs)
	require.NoError(t, err)
	require.Zero(t, sharedMeta.UnminedSince, "a tx also mined on the winning chain must stay mined")
}

// TestSetTxMinedStatus_BlockStillOnChain_NoRemark is the control: when B is still the tip
// after setTxMined writes, its txs stay mined.
func TestSetTxMinedStatus_BlockStillOnChain_NoRemark(t *testing.T) {
	initPrometheusMetrics()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	utxoStore, subtreeValidationClient, _, txStore, subtreeStore, deferFunc := setup(t)
	defer deferFunc()

	tSettings := test.CreateBaseTestSettings(t)
	blockChainStore, err := blockchain_store.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)
	blockchainClient, err := blockchain.NewLocalClient(ulogger.TestLogger{}, tSettings, blockChainStore, nil, nil)
	require.NoError(t, err)

	coinbaseB, bOnlyTx, _, _ := createCoinbaseAndChildTx(t)

	_, _, err = utxoStore.SpendAndCreate(ctx, bOnlyTx, 1, utxostore.WithCreateOnly())
	require.NoError(t, err)

	subtree, err := subtreepkg.NewTreeByLeafCount(2)
	require.NoError(t, err)
	require.NoError(t, subtree.AddCoinbaseNode())
	require.NoError(t, subtree.AddNode(*bOnlyTx.TxIDChainHash(), uint64(bOnlyTx.Size()), 0)) //nolint:gosec

	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes))

	replicated := subtree.Duplicate()
	replicated.ReplaceRootNode(coinbaseB.TxIDChainHash(), 0, uint64(coinbaseB.Size())) //nolint:gosec

	nBits, err := model.NewNBitFromString("207fffff")
	require.NoError(t, err)

	headerB := mineBlockHeader(t, tSettings.ChainCfgParams.GenesisHash, replicated.RootHash(), nBits)
	blockB, err := model.NewBlock(headerB, coinbaseB, []*chainhash.Hash{subtree.RootHash()}, uint64(subtree.Length()), uint64(coinbaseB.Size()+bOnlyTx.Size()), 1, 0) //nolint:gosec
	require.NoError(t, err)

	require.NoError(t, blockchainClient.AddBlock(ctx, blockB, "test"))

	blockValidation := NewBlockValidation(ctx, ulogger.TestLogger{}, tSettings, blockchainClient, subtreeStore, txStore, utxoStore, nil, subtreeValidationClient)

	require.NoError(t, blockValidation.setTxMinedStatus(ctx, blockB.Hash()))

	bOnlyMeta, err := utxoStore.Get(ctx, bOnlyTx.TxIDChainHash(), fields.UnminedSince)
	require.NoError(t, err)
	require.Zero(t, bOnlyMeta.UnminedSince, "a tx mined in the tip must stay mined")
}
