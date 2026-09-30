package seeder

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

func testImportOptions() importOptions {
	return importOptions{
		workerCount:            256,
		multiRecordWorkerCount: 16,
		channelSize:            16,
		utxoBatchSize:          128,
	}
}

// Workers receive with a plain range over the channel (no select on
// ctx.Done), so cancellation relies on the reader closing the channel. A
// cancelled import must still return promptly with an error, never hang.
func TestImportUTXOSet_CancelledContextReturnsPromptly(t *testing.T) {
	path := writeCompleteSnapshotFile(t, benchWrappers("cancel", 10_000, 50))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)

	go func() {
		done <- importUTXOSet(ctx, ulogger.TestLogger{}, noopCreateStore{}, path, testImportOptions())
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("importUTXOSet did not return after context cancellation")
	}
}

// cancelOnFirstCreateStore waits until the reader has sent everything (the
// channel buffer holds the whole file), then cancels the import and returns
// success, so the remaining buffered records are only ever dropped.
type cancelOnFirstCreateStore struct {
	utxo.Store
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelOnFirstCreateStore) SpendAndCreate(_ context.Context, _ *bt.Tx, _ uint32, _ ...utxo.CreateOption) (*meta.Data, []*utxo.Spend, error) {
	s.once.Do(func() {
		time.Sleep(200 * time.Millisecond) // let the reader finish and close the channel
		s.cancel()
	})

	return nil, nil, nil
}

// A cancellation that arrives after the reader has finished must not be
// reported as success: buffered records were dropped, so the import is
// incomplete and lastProcessed.dat must not be written.
func TestImportUTXOSet_CancelAfterReaderFinishedIsNotSuccess(t *testing.T) {
	path := writeCompleteSnapshotFile(t, benchWrappers("drain", 500, 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := testImportOptions()
	opts.workerCount = 1
	opts.multiRecordWorkerCount = 1
	opts.channelSize = 1000

	err := importUTXOSet(ctx, ulogger.TestLogger{}, &cancelOnFirstCreateStore{cancel: cancel}, path, opts)
	require.ErrorIs(t, err, context.Canceled)
}

// Zero workers would leave the reader blocked forever on a full channel.
func TestImportUTXOSet_RejectsNonPositiveWorkerCounts(t *testing.T) {
	path := writeCompleteSnapshotFile(t, benchWrappers("zero", 10, 0))

	for _, mutate := range []func(*importOptions){
		func(o *importOptions) { o.workerCount = 0 },
		func(o *importOptions) { o.multiRecordWorkerCount = 0 },
	} {
		opts := testImportOptions()
		mutate(&opts)

		require.Error(t, importUTXOSet(context.Background(), ulogger.TestLogger{}, noopCreateStore{}, path, opts))
	}
}

// txidRecordingStore records every created txid and how often it was created.
type txidRecordingStore struct {
	utxo.Store
	mu   sync.Mutex
	seen map[chainhash.Hash]int
}

func (s *txidRecordingStore) SpendAndCreate(_ context.Context, _ *bt.Tx, _ uint32, opts ...utxo.CreateOption) (*meta.Data, []*utxo.Spend, error) {
	o, err := utxo.ParseCreateOptions(opts...)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	s.seen[*o.TxID]++
	s.mu.Unlock()

	return nil, nil, nil
}

// Both passes together must create every record exactly once: single-record
// txs in one pass, multi-record txs in the other, none dropped or doubled.
func TestImportUTXOSet_ProcessesAllRecordsExactlyOnce(t *testing.T) {
	const n = 20_000

	wrappers := benchWrappers("all", n, 50)
	path := writeCompleteSnapshotFile(t, wrappers)

	store := &txidRecordingStore{seen: make(map[chainhash.Hash]int, n)}

	require.NoError(t, importUTXOSet(context.Background(), ulogger.TestLogger{}, store, path, testImportOptions()))

	require.Len(t, store.seen, n)

	for _, w := range wrappers {
		require.Equal(t, 1, store.seen[w.TxID], "tx %s", w.TxID)
	}
}

// gatedStore holds every multi-record create until all single-record creates
// have been written. With one shared worker pool the workers pile up on the
// held multi-record txs and the single-record txs never finish (deadlock);
// with independent passes the single-record pass completes and releases them.
type gatedStore struct {
	utxo.Store
	singleRemaining atomic.Int64
	release         chan struct{}
	releaseOnce     sync.Once
}

func (s *gatedStore) SpendAndCreate(ctx context.Context, tx *bt.Tx, _ uint32, _ ...utxo.CreateOption) (*meta.Data, []*utxo.Spend, error) {
	if len(tx.Outputs) > 128 {
		select {
		case <-s.release:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	if s.singleRemaining.Add(-1) == 0 {
		s.releaseOnce.Do(func() { close(s.release) })
	}

	return nil, nil, nil
}

func TestImportUTXOSet_MultiRecordTxsDoNotStallSingleRecordTxs(t *testing.T) {
	const n = 20_000

	// One tx in 10 is multi-record: 2,000 of them, far more than the 256 workers.
	wrappers := benchWrappers("gated", n, 10)
	path := writeCompleteSnapshotFile(t, wrappers)

	store := &gatedStore{release: make(chan struct{})}

	for _, w := range wrappers {
		if !spansMultipleRecords(w, 128) {
			store.singleRemaining.Add(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, importUTXOSet(ctx, ulogger.TestLogger{}, store, path, testImportOptions()))
}

// failingMultiRecordStore fails every multi-record create.
type failingMultiRecordStore struct {
	utxo.Store
}

func (failingMultiRecordStore) SpendAndCreate(_ context.Context, tx *bt.Tx, _ uint32, _ ...utxo.CreateOption) (*meta.Data, []*utxo.Spend, error) {
	if len(tx.Outputs) > 128 {
		return nil, nil, errors.NewStorageError("simulated multi-record failure")
	}

	return nil, nil, nil
}

// A failure in the background multi-record pass must stop the whole import
// and surface as an error, not be lost behind the other pass succeeding.
func TestImportUTXOSet_MultiRecordFailureStopsImport(t *testing.T) {
	path := writeCompleteSnapshotFile(t, benchWrappers("fail", 20_000, 50))

	err := importUTXOSet(context.Background(), ulogger.TestLogger{}, failingMultiRecordStore{}, path, testImportOptions())
	require.Error(t, err)
	require.Contains(t, err.Error(), "simulated multi-record failure")
}

// The Aerospike store splits a tx into multiple records by its padded output
// count (highest unspent index + 1), not by how many outputs are unspent.
func TestSpansMultipleRecords(t *testing.T) {
	w := func(indices ...uint32) *utxopersister.UTXOWrapper {
		uw := &utxopersister.UTXOWrapper{}
		for _, i := range indices {
			uw.UTXOs = append(uw.UTXOs, &utxopersister.UTXO{Index: i})
		}

		return uw
	}

	require.False(t, spansMultipleRecords(w(0, 1, 2), 128))
	require.False(t, spansMultipleRecords(w(127), 128), "index 127 still fits the first record")
	require.True(t, spansMultipleRecords(w(128), 128), "a single unspent output at index 128 needs a second record")
	require.True(t, spansMultipleRecords(w(5000, 3), 128))
	require.False(t, spansMultipleRecords(w(5000), 0), "threshold 0 disables the check")
	require.False(t, spansMultipleRecords(w(), 128))
}
