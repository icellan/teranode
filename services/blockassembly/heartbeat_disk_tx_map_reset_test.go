package blockassembly

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A post-commit disk tx map storage error cannot fail the operation that
// observed it (moveForwardBlock, reorgBlocks, dequeue, removeTx, Stop, or
// reset's own reload - the write is already applied), so the subtree
// processor instead records that a reset should happen and BlockAssembler's
// main loop is the only reader (see the heartbeatTicker.C case in Start).
// This pins that wiring end to end: a subtree processor reporting a pending
// reset request must make BlockAssembler actually reset, with no other
// trigger (no block announcement, no explicit Reset call) involved.
func TestBlockAssembler_HeartbeatPollsAndActsOnDiskTxMapResetRequest(t *testing.T) {
	initPrometheusMetrics()

	items := setupBlockAssemblyTest(t)

	// Shrink the tick so this runs in milliseconds instead of seconds (same
	// technique as TestLivenessDoesNotRestartAnIdleNode).
	items.blockAssembler.heartbeatInterval = 10 * time.Millisecond

	resetCalled := make(chan struct{})

	mockStp := &subtreeprocessor.MockSubtreeProcessor{}
	mockStp.On("Start", mock.Anything).Return()
	mockStp.On("WaitForPendingBlocks", mock.Anything).Return(nil)
	mockStp.On("Reset", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			select {
			case <-resetCalled:
			default:
				close(resetCalled)
			}
		}).
		Return(subtreeprocessor.ResetResponse{})
	mockStp.On("GetCurrentBlockHeader").Return(model.GenesisBlockHeader)
	mockStp.On("InitCurrentBlockHeader", mock.Anything).Return()
	mockStp.On("FlushDiskTxMapForLoad", mock.Anything, mock.Anything).Return(nil)
	// True exactly once: TakeResetRequested is take-once in the real
	// implementation, so a fresh mock call after the first reset has been
	// picked up must not requeue another one.
	mockStp.On("TakeResetRequested").Return(true).Once()
	mockStp.On("TakeResetRequested").Return(false)
	injectMockStp(t, items, mockStp)

	require.NoError(t, items.blockAssembler.Start(t.Context()))

	select {
	case <-resetCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("a pending disk tx map reset request must make BlockAssembler reset, with no block announcement or explicit Reset call")
	}
}
