package blockvalidation

import (
	"testing"

	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Quick validation runs for blocks at or below a hash-verified checkpoint, which
// certifies the body transitively: the pinned hash covers the header, the header
// covers the merkle root, the merkle root covers the body. That last step has to
// be evaluated for it to mean anything.
//
// For a zero-subtree block the merkle root is the coinbase txid, so the check is
// one hash. Without it nothing on this path reads the coinbase, even though its
// outputs become UTXOs once the block commits
// (SubtreeProcessor.moveForwardBlock -> processCoinbaseUtxos).

// TestQuickValidateBlock_ZeroSubtreeMerkleRootMismatchRejected drives a block whose
// header merkle root does not match its coinbase and asserts quick validation
// refuses it before committing.
func TestQuickValidateBlock_ZeroSubtreeMerkleRootMismatchRejected(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	suite.MockBlockchain.On("AssignBlockID", mock.Anything, mock.Anything).Return(uint64(1), nil).Maybe()
	suite.MockBlockchain.On("AddBlock", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	suite.MockBlockchain.On("SetBlockSubtreesSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	// Keep the header and put a different coinbase under it, so the block no longer
	// hashes to its own merkle root.
	block := testhelpers.CreateTestBlocks(t, 1)[0]
	block.CoinbaseTx = testhelpers.CreateSimpleCoinbaseTx(9999)

	require.False(t, block.Header.HashMerkleRoot.IsEqual(block.CoinbaseTx.TxIDChainHash()),
		"test precondition: the coinbase must not hash to the header merkle root")

	err := suite.Server.blockValidation.quickValidateBlock(suite.Ctx, block, "test", "")

	require.Error(t, err, "quick validation must reject a zero-subtree body that does not match the header merkle root")
	require.ErrorContains(t, err, "merkle root")
	suite.MockBlockchain.AssertNotCalled(t, "AddBlock", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestQuickValidateBlock_HonestEmptyBlockAccepted is the counterweight: a genuine
// coinbase-only block, whose merkle root is its coinbase txid, must still commit.
func TestQuickValidateBlock_HonestEmptyBlockAccepted(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	suite.MockBlockchain.On("AssignBlockID", mock.Anything, mock.Anything).Return(uint64(1), nil).Once()
	suite.MockBlockchain.On("AddBlock", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	suite.MockBlockchain.On("SetBlockSubtreesSet", mock.Anything, mock.Anything).Return(nil).Maybe()

	block := testhelpers.CreateTestBlocks(t, 1)[0]

	err := suite.Server.blockValidation.quickValidateBlock(suite.Ctx, block, "test", "")

	require.NoError(t, err, "a genuine empty block must still quick-validate")
	suite.MockBlockchain.AssertCalled(t, "AddBlock", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
