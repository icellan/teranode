package blockassembly

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// resetTestMock wires a mock subtree processor for the heartbeat reset tests.
// onReset runs inside every Reset call, where the real subtree processor's
// reset would run its reload; it can raise a new request or mark the reset as
// having hit a storage error, exactly as the reload would.
func resetTestMock(onReset func(m *subtreeprocessor.MockSubtreeProcessor)) (*subtreeprocessor.MockSubtreeProcessor, *atomic.Int32) {
	resets := &atomic.Int32{}
	m := &subtreeprocessor.MockSubtreeProcessor{}

	m.On("Start", mock.Anything).Return()
	m.On("WaitForPendingBlocks", mock.Anything).Return(nil)
	m.On("Reset", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			m.ResetStorageFailed.Store(false)
			resets.Add(1)

			if onReset != nil {
				onReset(m)
			}
		}).
		Return(subtreeprocessor.ResetResponse{})
	m.On("GetCurrentBlockHeader").Return(model.GenesisBlockHeader)
	m.On("InitCurrentBlockHeader", mock.Anything).Return()
	m.On("FlushDiskTxMapForLoad", mock.Anything, mock.Anything).Return(nil)

	return m, resets
}

func startWithMock(t *testing.T, m *subtreeprocessor.MockSubtreeProcessor) *baTestItems {
	t.Helper()

	initPrometheusMetrics()
	prometheusBlockAssemblyDiskTxMapDegraded.Set(0)
	t.Cleanup(func() { prometheusBlockAssemblyDiskTxMapDegraded.Set(0) })

	items := setupBlockAssemblyTest(t)
	items.blockAssembler.heartbeatInterval = 10 * time.Millisecond
	injectMockStp(t, items, m)

	require.NoError(t, items.blockAssembler.Start(t.Context()))

	return items
}

func degradedGauge() float64 {
	return testutil.ToFloat64(prometheusBlockAssemblyDiskTxMapDegraded)
}

// A pending post-commit storage request makes BlockAssembler reset, with no
// other trigger involved; a reset that completes clean leaves the node healthy.
func TestBlockAssembler_HeartbeatActsOnDiskTxMapResetRequest(t *testing.T) {
	m, resets := resetTestMock(nil)
	m.ResetRequested.Store(true)

	startWithMock(t, m)

	require.Eventually(t, func() bool { return resets.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return resets.Load() > 1 }, 200*time.Millisecond, 10*time.Millisecond,
		"the request is take-once: one reset per request")
	require.Zero(t, degradedGauge())
}

// A persistent fault: the storage-triggered reset's own reload hits a storage
// error again. There must be exactly one such reset, after which the node is
// degraded and stops auto-resetting instead of looping full reloads.
func TestBlockAssembler_PersistentDiskTxMapFault_OneResetThenDegraded(t *testing.T) {
	m, resets := resetTestMock(func(m *subtreeprocessor.MockSubtreeProcessor) {
		m.ResetStorageFailed.Store(true)
		m.ResetRequested.Store(true) // the reload left a phantom again
	})
	m.ResetRequested.Store(true)

	startWithMock(t, m)

	require.Eventually(t, func() bool { return degradedGauge() == 1 }, 2*time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return resets.Load() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"no further storage-triggered resets while degraded")
	require.False(t, m.ResetRequested.Load(), "requests raised while degraded are consumed, not left to pile up")
}

// A phantom left by a manual reset's reload is still escalated: the request it
// raises is picked up by the heartbeat and causes a storage-triggered reset.
func TestBlockAssembler_PhantomFromManualResetIsEscalated(t *testing.T) {
	var call atomic.Int32

	m, resets := resetTestMock(func(m *subtreeprocessor.MockSubtreeProcessor) {
		if call.Add(1) == 1 {
			m.ResetStorageFailed.Store(true)
			m.ResetRequested.Store(true)
		}
	})

	items := startWithMock(t, m)
	items.blockAssembler.Reset(false)

	require.Eventually(t, func() bool { return resets.Load() == 2 }, 2*time.Second, 5*time.Millisecond,
		"manual reset, then the storage-triggered one it requested")
	require.Never(t, func() bool { return resets.Load() > 2 }, 200*time.Millisecond, 10*time.Millisecond)
	require.Zero(t, degradedGauge(), "the storage-triggered reset completed clean")
}

// onResetDone decides the degraded state from how a reset ended; every reset
// completion path (the resetCh handler and the reorg fallbacks) goes through it.
func TestBlockAssembler_OnResetDone(t *testing.T) {
	initPrometheusMetrics()
	t.Cleanup(func() { prometheusBlockAssemblyDiskTxMapDegraded.Set(0) })

	storageErr := errors.NewStorageError("rotation failed")
	otherErr := errors.NewServiceError("blockchain unavailable")

	for _, tc := range []struct {
		name             string
		storageTriggered bool
		resetErr         error
		storageFailed    bool
		degradedBefore   bool
		degradedAfter    bool
	}{
		{name: "storage-triggered reset fails on storage", storageTriggered: true, storageFailed: true, degradedAfter: true},
		{name: "storage-triggered rotation failure", storageTriggered: true, resetErr: storageErr, storageFailed: true, degradedAfter: true},
		{name: "storage-triggered reset clean", storageTriggered: true},
		{name: "clean storage-triggered reset clears degraded", storageTriggered: true, degradedBefore: true},
		{name: "clean manual or reorg reset clears degraded", degradedBefore: true},
		{name: "manual reset leaving a phantom does not degrade", storageFailed: true},
		{name: "non-storage failure does not degrade", storageTriggered: true, resetErr: otherErr},
		{name: "non-storage failure keeps degraded", resetErr: otherErr, degradedBefore: true, degradedAfter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &subtreeprocessor.MockSubtreeProcessor{}
			m.On("Stop", mock.Anything).Return().Maybe() // the test setup's cleanup stops it
			m.ResetStorageFailed.Store(tc.storageFailed)

			items := setupBlockAssemblyTest(t)
			b := items.blockAssembler
			b.subtreeProcessor = m
			b.diskTxMapDegraded = tc.degradedBefore
			prometheusBlockAssemblyDiskTxMapDegraded.Set(map[bool]float64{false: 0, true: 1}[tc.degradedBefore])

			b.onResetDone(tc.storageTriggered, tc.resetErr)

			require.Equal(t, tc.degradedAfter, b.diskTxMapDegraded)
			require.Equal(t, map[bool]float64{false: 0, true: 1}[tc.degradedAfter], degradedGauge())
		})
	}
}
