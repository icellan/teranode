package blockvalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A second BlockValidation on the same stores, such as the one the test daemon
// builds next to the service, must leave the mined status of blocks to the
// service. Two setMined workers on one block race: one's set can land after
// the other's unset of an invalidated block, and its txs stay marked mined.

func TestNewBlockValidation_NoSetMinedOption(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	bv := NewBlockValidation(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), nil, nil, nil, nil, nil, nil, NoSetMined{})
	require.True(t, bv.setMinedDisabled)

	bv = NewBlockValidation(ctx, ulogger.TestLogger{}, test.CreateBaseTestSettings(t), nil, nil, nil, nil, nil, nil)
	require.False(t, bv.setMinedDisabled)
}

func TestEnqueueSetMined_NoOpWhenSetMinedDisabled(t *testing.T) {
	bv := newSetMinedTestBV(4)
	bv.setMinedDisabled = true

	hash := chainhash.HashH([]byte("set-mined-disabled"))
	bv.enqueueSetMined(&hash)

	require.Empty(t, bv.setMinedChan)
	require.Empty(t, bv.setMinedOverflow)
}

func TestProcessBlockMinedNotSet_NoOpWhenSetMinedDisabled(t *testing.T) {
	mockBC := &blockchain.Mock{}
	mockBC.On("GetBlocksMinedNotSet", mock.Anything).Return([]*model.Block{{Header: &model.BlockHeader{
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
	}}}, nil)

	bv := newSetMinedTestBV(4)
	bv.blockchainClient = mockBC
	bv.setMinedDisabled = true

	bv.processBlockMinedNotSet(t.Context())

	mockBC.AssertNotCalled(t, "GetBlocksMinedNotSet", mock.Anything)
	require.Empty(t, bv.setMinedChan)
}
