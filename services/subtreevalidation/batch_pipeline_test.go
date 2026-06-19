package subtreevalidation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/stretchr/testify/require"
)

// runLoadProcessPipeline overlaps the read-only LOAD of each batch with the
// PROCESS of the previous batch while keeping PROCESS strictly serial and
// in-order. These tests pin that contract (it is the consensus-safety property
// of the CheckBlockSubtrees batch pipeline) plus the arena-release guarantee on
// the abort paths.

// TestRunLoadProcessPipeline_ProcessesInOrderSerially asserts process is called
// once per batch, in batch order, and never concurrently with itself — even
// though load runs ahead. The injected per-process sleep means an overlapping
// (buggy) implementation would be caught by the concurrency guard.
func TestRunLoadProcessPipeline_ProcessesInOrderSerially(t *testing.T) {
	const numBatches = 6

	var (
		mu        sync.Mutex
		order     []int
		inProcess bool
		overlap   bool
	)

	loaded := make([]bool, numBatches)

	load := func(_ context.Context, idx int) ([]*bt.Tx, []*bt.Arena, error) {
		loaded[idx] = true
		return nil, nil, nil
	}

	process := func(idx int, _ []*bt.Tx, _ []*bt.Arena) error {
		mu.Lock()
		if inProcess {
			overlap = true
		}
		inProcess = true
		order = append(order, idx)
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		inProcess = false
		mu.Unlock()

		return nil
	}

	err := runLoadProcessPipeline(context.Background(), numBatches, load, process, func([]*bt.Arena) {})
	require.NoError(t, err)

	require.False(t, overlap, "process must never run concurrently with itself")
	require.Equal(t, []int{0, 1, 2, 3, 4, 5}, order, "process must be called in batch order")
}

// TestRunLoadProcessPipeline_ProcessErrorReleasesPendingBatches is the core
// new-risk guard: when process fails on an early batch, every batch the
// producer already loaded ahead must be released exactly once, and the process
// error must propagate. A naive pipeline that drops the load-ahead batches on
// the floor would leak their arenas here.
func TestRunLoadProcessPipeline_ProcessErrorReleasesPendingBatches(t *testing.T) {
	const numBatches = 8

	var (
		mu          sync.Mutex
		loadedCount int
		released    = map[int]int{}
	)

	wantErr := errors.NewProcessingError("process boom")

	load := func(_ context.Context, idx int) ([]*bt.Tx, []*bt.Arena, error) {
		mu.Lock()
		loadedCount++
		mu.Unlock()
		// One sentinel arena per batch so release can be attributed by identity.
		return nil, []*bt.Arena{bt.NewArena(1)}, nil
	}

	// Track which loaded batch each arena belongs to via a side map keyed on
	// pointer identity captured at load time.
	arenaToBatch := map[*bt.Arena]int{}

	loadTracking := func(ctx context.Context, idx int) ([]*bt.Tx, []*bt.Arena, error) {
		txs, arenas, err := load(ctx, idx)
		mu.Lock()
		for _, a := range arenas {
			arenaToBatch[a] = idx
		}
		mu.Unlock()
		return txs, arenas, err
	}

	process := func(idx int, _ []*bt.Tx, _ []*bt.Arena) error {
		if idx == 1 {
			return wantErr
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	}

	release := func(arenas []*bt.Arena) {
		mu.Lock()
		for _, a := range arenas {
			released[arenaToBatch[a]]++
		}
		mu.Unlock()
	}

	err := runLoadProcessPipeline(context.Background(), numBatches, loadTracking, process, release)
	require.ErrorIs(t, err, wantErr)

	mu.Lock()
	defer mu.Unlock()

	// Every batch the producer loaded must have had its arenas released exactly
	// once — no leak, no double release.
	for b := 0; b < loadedCount; b++ {
		require.Equal(t, 1, released[b], "batch %d arenas must be released exactly once (loaded=%d)", b, loadedCount)
	}
	require.Equal(t, loadedCount, len(released), "released set must equal loaded set")
}

// TestRunLoadProcessPipeline_LoadErrorAborts asserts a LOAD failure aborts the
// run, returns that error, and releases any batches already processed/loaded
// without leaking. loadSubtreeBatch releases its own arenas on failure, so the
// failing batch contributes nil arenas (modelled here by returning nil).
func TestRunLoadProcessPipeline_LoadErrorAborts(t *testing.T) {
	const numBatches = 6

	wantErr := errors.NewStorageError("load boom")

	var (
		mu        sync.Mutex
		processed []int
		releasedN int
		loadCalls int
	)

	load := func(_ context.Context, idx int) ([]*bt.Tx, []*bt.Arena, error) {
		mu.Lock()
		loadCalls++
		mu.Unlock()
		if idx == 3 {
			// Mirror loadSubtreeBatch: it releases its own arenas before
			// returning an error, so the failing batch yields nil arenas.
			return nil, nil, wantErr
		}
		return nil, []*bt.Arena{bt.NewArena(1)}, nil
	}

	process := func(idx int, _ []*bt.Tx, _ []*bt.Arena) error {
		mu.Lock()
		processed = append(processed, idx)
		mu.Unlock()
		return nil
	}

	release := func(arenas []*bt.Arena) {
		mu.Lock()
		releasedN += len(arenas)
		mu.Unlock()
	}

	err := runLoadProcessPipeline(context.Background(), numBatches, load, process, release)
	require.ErrorIs(t, err, wantErr)

	mu.Lock()
	defer mu.Unlock()

	// Batches before the failing one are processed in order; the failing batch
	// and everything after it are not processed.
	require.Equal(t, []int{0, 1, 2}, processed)
	// Every non-failing batch that was loaded contributes one arena; all must
	// be released (processed ones after process, the failing batch none).
	require.Equal(t, releasedN, countNonFailingLoaded(loadCalls, 3))
}

// countNonFailingLoaded returns how many loaded batches carried a (releasable)
// arena given the failing batch index — every loaded batch except the failing
// one contributes exactly one arena.
func countNonFailingLoaded(loadCalls, failIdx int) int {
	n := 0
	for i := 0; i < loadCalls; i++ {
		if i != failIdx {
			n++
		}
	}
	return n
}

// TestRunLoadProcessPipeline_OverlapsLoadWithProcess proves the speed-up: with
// a per-batch LOAD delay and PROCESS delay, the pipeline wall-clock must be
// well below the sequential sum (load+process per batch), because LOAD of
// batch N+1 overlaps PROCESS of batch N.
func TestRunLoadProcessPipeline_OverlapsLoadWithProcess(t *testing.T) {
	const (
		numBatches  = 8
		loadDelay   = 8 * time.Millisecond
		processTime = 8 * time.Millisecond
	)

	load := func(_ context.Context, _ int) ([]*bt.Tx, []*bt.Arena, error) {
		time.Sleep(loadDelay)
		return nil, nil, nil
	}
	process := func(_ int, _ []*bt.Tx, _ []*bt.Arena) error {
		time.Sleep(processTime)
		return nil
	}

	start := time.Now()
	err := runLoadProcessPipeline(context.Background(), numBatches, load, process, func([]*bt.Arena) {})
	elapsed := time.Since(start)
	require.NoError(t, err)

	sequential := numBatches * (loadDelay + processTime)
	// Pipeline lower bound ≈ loadDelay (first load) + numBatches*processTime.
	// Require a clear win over sequential (allow scheduling slack).
	require.Less(t, elapsed, sequential*3/4,
		"pipeline (%s) should be well below sequential (%s)", elapsed, sequential)
}
