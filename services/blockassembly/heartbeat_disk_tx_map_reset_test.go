package blockassembly

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	prometheusBlockAssemblyDiskTxMapDegraded.Set(0) // isolate from any other test's run of the gauge

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

	// Transient fault: TakeResetRequested is stubbed true only once, so the
	// reset's own post-reload check (recordResetOutcome) sees no new request
	// pending - the fault cleared. It must never reach the degraded state a
	// persistent fault would (see
	// TestBlockAssembler_PersistentDiskTxMapFaultBoundsResetsAndDegrades):
	// give it many heartbeat ticks' worth of time and confirm it stays clear.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, float64(0), testutil.ToFloat64(prometheusBlockAssemblyDiskTxMapDegraded),
		"a transient fault (one reset, then healthy) must not degrade")
}

// A persistent disk fault (every reload attempt still leaves a phantom
// pending) must not loop resets forever: BlockAssembler bounds it at
// maxConsecutiveDiskTxMapResets, backing off between attempts, then
// suspends auto-reset and sets the degraded gauge. This is the loop
// TestScratch_ResetReloadFaultRequestsResetEveryCycle (review scratch)
// demonstrated is otherwise unbounded.
func TestBlockAssembler_PersistentDiskTxMapFaultBoundsResetsAndDegrades(t *testing.T) {
	initPrometheusMetrics()

	items := setupBlockAssemblyTest(t)

	items.blockAssembler.heartbeatInterval = 5 * time.Millisecond
	items.blockAssembler.diskTxMapResetBackoffBase = 20 * time.Millisecond

	var resetCalls atomic.Int32

	mockStp := &subtreeprocessor.MockSubtreeProcessor{}
	mockStp.On("Start", mock.Anything).Return()
	mockStp.On("WaitForPendingBlocks", mock.Anything).Return(nil)
	mockStp.On("Reset", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { resetCalls.Add(1) }).
		Return(subtreeprocessor.ResetResponse{})
	mockStp.On("GetCurrentBlockHeader").Return(model.GenesisBlockHeader)
	mockStp.On("InitCurrentBlockHeader", mock.Anything).Return()
	mockStp.On("FlushDiskTxMapForLoad", mock.Anything, mock.Anything).Return(nil)
	// Persistent: every check, before and after each reset, still finds a
	// pending request - the reload never cures it.
	mockStp.On("TakeResetRequested").Return(true)
	injectMockStp(t, items, mockStp)

	require.NoError(t, items.blockAssembler.Start(t.Context()))

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(prometheusBlockAssemblyDiskTxMapDegraded) == 1
	}, 5*time.Second, 5*time.Millisecond, "a persistent fault must eventually suspend auto-reset and set the degraded gauge")

	require.EqualValues(t, maxConsecutiveDiskTxMapResets, resetCalls.Load(),
		"exactly the retry budget's worth of resets must have run before giving up")

	// Give the (fast) heartbeat plenty of further ticks: once degraded, it
	// must stop polling entirely, not keep retrying past the budget.
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, maxConsecutiveDiskTxMapResets, resetCalls.Load(),
		"no further resets must run once degraded")
}
