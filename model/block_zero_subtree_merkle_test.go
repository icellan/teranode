package model

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/bsv-blockchain/teranode/stores/utxo/nullstore"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// A block with zero subtrees is legitimate — that is what an empty, coinbase-only
// block looks like on the wire, and SubtreeProcessor.moveForwardBlock has an
// explicit branch for it. Its header still covers that body: for an empty block
// the merkle root is the coinbase txid, which CheckMerkleRoot's no-subtree branch
// computes.
//
// These tests cover Valid's handling of that body shape. GetHash covers the header
// alone, so the merkle root is the only value relating a block's body to its
// header, and it has to be evaluated for every body shape rather than only for the
// ones that carry subtrees.

// honestMerkleRootForOneSubtreeBody builds a one-subtree body and returns the
// merkle root a header over it would carry. A zero-subtree body under that header
// does not match it.
func honestMerkleRootForOneSubtreeBody(t *testing.T) *chainhash.Hash {
	t.Helper()

	honestCoinbase, err := bt.NewTxFromString(CoinbaseHex)
	require.NoError(t, err)

	txHash, err := chainhash.NewHashFromStr("0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206")
	require.NoError(t, err)

	st, err := subtreepkg.NewTreeByLeafCount(2)
	require.NoError(t, err)
	require.NoError(t, st.AddCoinbaseNode())
	require.NoError(t, st.AddNode(*txHash, 0, 100))

	root, err := st.RootHashWithReplaceRootNode(honestCoinbase.TxIDChainHash(), 0, uint64(honestCoinbase.Size()))
	require.NoError(t, err)

	return root
}

// coinbaseWithOutputValue parses the canonical test coinbase and rewrites every
// output to satoshisPerOutput.
func coinbaseWithOutputValue(t *testing.T, satoshisPerOutput uint64) *bt.Tx {
	t.Helper()

	tx, err := bt.NewTxFromString(CoinbaseHex)
	require.NoError(t, err)
	require.True(t, tx.IsCoinbase())

	for _, out := range tx.Outputs {
		out.Satoshis = satoshisPerOutput
	}

	return tx
}

// TestBlock_Valid_ZeroSubtreeMerkleRootMismatchRejected: a header whose merkle root
// covers a one-subtree body, paired with a zero-subtree body. The coinbase value is
// deliberately far below the subsidy so checkBlockRewardAndFees cannot be what
// rejects it — only the merkle root comparison can.
func TestBlock_Valid_ZeroSubtreeMerkleRootMismatchRejected(t *testing.T) {
	const checkpointHeight = int32(2000)
	const blockHeight = uint32(1000)

	tSettings := newSkipTestSettings(t, false, checkpointHeight)

	hdr := minedHeader(t, honestMerkleRootForOneSubtreeBody(t))

	block, err := NewBlock(hdr, coinbaseWithOutputValue(t, 1), nil, 1, 123, blockHeight, 0)
	require.NoError(t, err)

	valid, err := block.Valid(
		context.Background(), ulogger.TestLogger{}, nil, &nullstore.NullStore{},
		txmap.NewSyncedMap[chainhash.Hash, []uint32](), []*BlockHeader{}, []uint32{}, tSettings, nil,
	)

	require.Error(t, err, "a zero-subtree body that does not hash to the header merkle root must be rejected")
	require.ErrorContains(t, err, "merkle root does not match")
	require.False(t, valid)
}

// TestBlock_Valid_ZeroSubtreeMerkleRootCheckedWhenFeeSkipEngages: the same mismatch
// below a hardcoded checkpoint with every other fee-skip conjunct satisfied
// (outpoint-only-capable store, confirmed checkpoint ancestor, height at or below
// the checkpoint). checkBlockRewardAndFees returns early in that configuration, so
// the coinbase value is not read there at all and the merkle root is what has to
// reject the block.
func TestBlock_Valid_ZeroSubtreeMerkleRootCheckedWhenFeeSkipEngages(t *testing.T) {
	const checkpointHeight = int32(2000)
	const blockHeight = uint32(1000) // <= checkpoint
	const maxSatoshis = uint64(21_000_000_00_000_000)

	tSettings := newSkipTestSettings(t, true, checkpointHeight)

	hdr := minedHeader(t, honestMerkleRootForOneSubtreeBody(t))

	// Two outputs of MaxSatoshis each: each within the per-transaction money range
	// the validator enforces, well over the subsidy in aggregate.
	coinbase := coinbaseWithOutputValue(t, maxSatoshis)
	coinbase.Outputs = coinbase.Outputs[:2]

	block, err := NewBlock(hdr, coinbase, nil, 1, 123, blockHeight, 0)
	require.NoError(t, err)
	block.SetCheckpointConfirmedAncestor(true)

	valid, err := block.Valid(
		context.Background(), ulogger.TestLogger{}, nil, &panicTxMetaStore{},
		txmap.NewSyncedMap[chainhash.Hash, []uint32](), []*BlockHeader{}, []uint32{}, tSettings, nil,
	)

	require.Error(t, err, "the merkle root must be checked even where the fee check returns early")
	require.ErrorContains(t, err, "merkle root does not match")
	require.False(t, valid)
}

// TestBlock_Valid_HonestEmptyBlockAccepted is the counterweight: a genuine empty
// block, whose header merkle root is its coinbase txid, must still validate. The
// no-subtree merkle check must not reject real empty blocks.
func TestBlock_Valid_HonestEmptyBlockAccepted(t *testing.T) {
	const checkpointHeight = int32(2000)
	const blockHeight = uint32(1000)

	tSettings := newSkipTestSettings(t, false, checkpointHeight)

	coinbase := coinbaseWithOutputValue(t, 1)
	hdr := minedHeader(t, coinbase.TxIDChainHash())

	block, err := NewBlock(hdr, coinbase, nil, 1, 123, blockHeight, 0)
	require.NoError(t, err)

	valid, err := block.Valid(
		context.Background(), ulogger.TestLogger{}, nil, &nullstore.NullStore{},
		txmap.NewSyncedMap[chainhash.Hash, []uint32](), []*BlockHeader{}, []uint32{}, tSettings, nil,
	)

	require.NoError(t, err)
	require.True(t, valid)
}

// bindBlockMerkleRoot makes a test block self-consistent: it sets the header merkle
// root to the value CheckMerkleRoot computes for the block's own body, then re-mines
// (cheap at the regtest 207fffff target the model fixtures use — changing the merkle
// root invalidates the nonce).
//
// Handles the two fixture shapes in this package: a coinbase-only block, whose merkle
// root is the coinbase txid, and a single-subtree block, whose root is the subtree's
// with the coinbase placeholder replaced. Several fixtures paired a real header with
// an unrelated body, which only validated while Valid skipped CheckMerkleRoot for
// them.
//
// Must be called before anything reads block.Hash(), which caches.
func bindBlockMerkleRoot(t *testing.T, block *Block) {
	t.Helper()

	var merkleRoot *chainhash.Hash

	switch len(block.SubtreeSlices) {
	case 0:
		require.Empty(t, block.Subtrees, "a block with no slices must have no subtrees")

		merkleRoot = block.CoinbaseTx.TxIDChainHash()

	case 1:
		var err error

		merkleRoot, err = block.SubtreeSlices[0].RootHashWithReplaceRootNode(
			block.CoinbaseTx.TxIDChainHash(), 0, uint64(block.CoinbaseTx.Size()),
		)
		require.NoError(t, err)

	default:
		t.Fatalf("bindBlockMerkleRoot handles 0 or 1 subtree, got %d", len(block.SubtreeSlices))
	}

	// Copy the header: the fixtures share one *BlockHeader across subtests, and
	// re-mining it in place would change what the others see.
	hdr := *block.Header
	hdr.HashMerkleRoot = merkleRoot
	hdr.Nonce = 0

	// HasMetTargetDifficulty reports a miss as an error, so only the bool is read
	// (as minedHeader does).
	for {
		ok, _, _ := hdr.HasMetTargetDifficulty()
		if ok {
			break
		}

		hdr.Nonce++
	}

	block.Header = &hdr
}
