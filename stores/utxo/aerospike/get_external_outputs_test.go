package aerospike

import (
	"context"
	"io"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// failingReadStore wraps a blob store and replaces the GetIoReader result for one
// file type, so a test can distinguish "the object is not there" from "the store
// failed".
type failingReadStore struct {
	blob.Store

	failFileType fileformat.FileType
	failWith     error
}

func (f *failingReadStore) GetIoReader(ctx context.Context, key []byte, fileType fileformat.FileType, opts ...options.FileOption) (io.ReadCloser, error) {
	if fileType == f.failFileType {
		return nil, f.failWith
	}

	return f.Store.GetIoReader(ctx, key, fileType, opts...)
}

// newOutputsOnlyStore builds a store whose external blob store holds only the
// .outputs (UTXO-set) representation of a transaction: the shape written by
// create.go for an input-less transaction, which is what cmd/seeder produces when
// bootstrapping from a snapshot. liveIndexes are the output indexes that were
// still unspent when the snapshot was taken.
func newOutputsOnlyStore(t *testing.T, liveIndexes ...uint32) (*Store, blob.Store, *bt.Tx) {
	t.Helper()

	chainParams := chaincfg.RegressionNetParams
	tSettings := &settings.Settings{}
	tSettings.ChainCfgParams = &chainParams

	mem := memory.New()

	s := &Store{
		ctx:           context.Background(),
		externalStore: mem,
		settings:      tSettings,
		logger:        ulogger.TestLogger{},
	}

	// A transaction with three outputs, of which only liveIndexes survive.
	tx := &bt.Tx{
		Version: 1,
		Inputs:  []*bt.Input{{PreviousTxOutIndex: 0}},
		Outputs: []*bt.Output{
			{Satoshis: 1000, LockingScript: bscript.NewFromBytes([]byte{0x51})},
			{Satoshis: 2000, LockingScript: bscript.NewFromBytes([]byte{0x52})},
			{Satoshis: 3000, LockingScript: bscript.NewFromBytes([]byte{0x53})},
		},
	}

	txHash := *tx.TxIDChainHash()

	wrapper := utxopersister.UTXOWrapper{
		TxID:   txHash,
		Height: 1,
	}

	for _, idx := range liveIndexes {
		wrapper.UTXOs = append(wrapper.UTXOs, &utxopersister.UTXO{
			Index:  idx,
			Value:  tx.Outputs[idx].Satoshis,
			Script: *tx.Outputs[idx].LockingScript,
		})
	}

	require.NoError(t, mem.Set(context.Background(), txHash[:], fileformat.FileTypeOutputs, wrapper.Bytes()))

	return s, mem, tx
}

// TestGetExternalOutputs_LeavesNilHoles documents the shape at the root of issue
// 1306. The .outputs blob retains only the outputs that were live at snapshot
// time — no inputs, no version, no locktime, and no record that a spent output
// ever existed — so the reconstruction cannot be the transaction. It is still
// exactly what the outpoint-decoration callers need, which is why the fallback
// exists.
func TestGetExternalOutputs_LeavesNilHoles(t *testing.T) {
	// Output 0 was already spent at snapshot time; 1 and 2 survived, so the hole
	// at index 0 stays in bounds.
	s, _, tx := newOutputsOnlyStore(t, 1, 2)

	got, err := s.getExternalOutputs(context.Background(), *tx.TxIDChainHash())
	require.NoError(t, err)

	require.Len(t, got.Outputs, 3)
	require.Nil(t, got.Outputs[0], "a spent index must stay nil — that is the store's encoding for 'not a live UTXO'")
	require.NotNil(t, got.Outputs[1])
	require.NotNil(t, got.Outputs[2])

	// The reconstruction is not the transaction and must never be mistaken for it.
	require.Empty(t, got.Inputs, "the .outputs blob retains no inputs")
	require.Zero(t, got.Version, "the .outputs blob retains no version")

	// It cannot even be identified: computing its txid would mean serializing it,
	// and serialization dereferences the nil hole — go-bt's Output.Size() and
	// appendTo() both fault, and TxIDChainHash() reaches them. That crash is the
	// reason meta.Data.TxIsSerializable exists, so assert the predicate the callers
	// actually gate on rather than the panic itself: a future go-bt that returned an
	// error instead of faulting would break a require.Panics here for a good reason,
	// while this assertion would keep meaning what it says.
	require.False(t, (&meta.Data{Tx: got}).TxIsSerializable(),
		"the reconstruction must fail the gate that every serializing caller checks")

	// Sanity: the source transaction is serializable, so the predicate is
	// discriminating rather than always-false.
	require.True(t, (&meta.Data{Tx: tx}).TxIsSerializable())
	require.NotNil(t, tx.TxIDChainHash())
}

// TestGetExternalTransaction_ReadsOnlyTheTxBlob pins the contract of the .tx
// reader itself: it reports what the .tx blob holds, and reaching for the
// UTXO-set representation is the composing function's job, not its own.
//
// Note what is deliberately NOT asserted here: that the public
// GetTxFromExternalStore refuses the reconstruction. It does not, and must not —
// services/validator/Validator.go reads a parent's outputs through it to extend a
// child, and a snapshot-seeded parent has no .tx blob at all. What keeps the
// reconstruction away from the callers that cannot survive it is
// meta.Data.TxIsSerializable, which they check. See
// TestExternalOutputsOnlyParentIsReadableForExtension.
func TestGetExternalTransaction_ReadsOnlyTheTxBlob(t *testing.T) {
	t.Run("an absent .tx blob is not-found", func(t *testing.T) {
		s, _, tx := newOutputsOnlyStore(t, 1, 2)

		got, err := s.getExternalTransaction(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err, "this reader speaks only for the .tx blob, which is absent")
		require.Nil(t, got)
		require.True(t, errors.Is(err, errors.ErrTxNotFound),
			"an absent .tx blob is not-found, got %v", err)
	})

	t.Run("a transient .tx failure is a storage error, not not-found", func(t *testing.T) {
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)

		s.externalStore = &failingReadStore{
			Store:        mem,
			failFileType: fileformat.FileTypeTx,
			failWith:     errors.NewServiceUnavailableError("read permits exhausted"),
		}

		got, err := s.getExternalTransaction(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err)
		require.Nil(t, got)
		require.False(t, errors.Is(err, errors.ErrTxNotFound),
			"a failure to read the tx blob must not masquerade as the tx being absent")
		require.True(t, errors.Is(err, errors.ErrServiceUnavailable),
			"the original failure must be preserved, got %v", err)
	})
}

// TestGetTxInpointsFromExternalStore_MissIsNotAStoreFailure covers the third
// external read. There is no fallback to offer — inpoints are inputs and the
// .outputs blob retains none — but the miss/failure distinction still has to hold,
// and on a snapshot-seeded node an .outputs-only parent makes the miss routine.
func TestGetTxInpointsFromExternalStore_MissIsNotAStoreFailure(t *testing.T) {
	t.Run("absent .tx reports not-found", func(t *testing.T) {
		s, _, tx := newOutputsOnlyStore(t, 1, 2)

		_, err := s.GetTxInpointsFromExternalStore(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrTxNotFound),
			"an absent .tx blob is not-found, not a store failure, got %v", err)
	})

	t.Run("a transient failure stays a store failure", func(t *testing.T) {
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)

		s.externalStore = &failingReadStore{
			Store:        mem,
			failFileType: fileformat.FileTypeTx,
			failWith:     errors.NewServiceUnavailableError("read permits exhausted"),
		}

		_, err := s.GetTxInpointsFromExternalStore(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err)
		require.False(t, errors.Is(err, errors.ErrTxNotFound),
			"a store failure must not masquerade as the tx being absent")
		require.True(t, errors.Is(err, errors.ErrServiceUnavailable),
			"the original failure must be preserved, got %v", err)
	})
}

// TestGetExternalTransactionOrOutputs_FallsBackOnlyOnNotFound is the contract this
// change establishes for the outpoint path. The fallback to the UTXO-set blob is
// correct when the full transaction is genuinely absent, and wrong for any other
// failure: silently serving a snapshot-derived reconstruction in place of a
// transaction the store merely failed to read turns a transient outage into a
// wrong answer.
func TestGetExternalTransactionOrOutputs_FallsBackOnlyOnNotFound(t *testing.T) {
	t.Run("missing .tx falls back to .outputs", func(t *testing.T) {
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)

		// memory.New() reports a genuine miss for the absent .tx blob.
		s.externalStore = mem

		got, err := s.getExternalTransactionOrOutputs(context.Background(), *tx.TxIDChainHash())
		require.NoError(t, err)
		require.Len(t, got.Outputs, 3)
	})

	t.Run("the public reader falls back too", func(t *testing.T) {
		// The regression this guards: routing GetTxFromExternalStore at the .tx
		// reader alone left the validator's parent-extension read with no way to
		// reach a snapshot-seeded parent, since such a parent has only .outputs.
		// TestExternalOutputsOnlyParentIsReadableForExtension covers the same
		// contract through a real store; this keeps it pinned without a container.
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)
		s.externalStore = mem

		got, err := s.GetTxFromExternalStore(context.Background(), *tx.TxIDChainHash())
		require.NoError(t, err, "a seeded parent must stay readable through the public entry point")
		require.Len(t, got.Outputs, 3)
		require.NotNil(t, got.Outputs[1], "the live indexes are what the extension path reads")
	})

	t.Run("transient .tx read failure is surfaced, not masked", func(t *testing.T) {
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)

		injected := errors.NewServiceUnavailableError("read permits exhausted")
		s.externalStore = &failingReadStore{
			Store:        mem,
			failFileType: fileformat.FileTypeTx,
			failWith:     injected,
		}

		got, err := s.getExternalTransactionOrOutputs(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err, "a store failure must not silently degrade to the UTXO-set reconstruction")
		require.Nil(t, got)
		require.True(t, errors.Is(err, errors.ErrServiceUnavailable),
			"the original failure must be preserved so the caller can tell it apart from a miss, got %v", err)
	})

	t.Run("transient .outputs read failure is surfaced, not reported as not-found", func(t *testing.T) {
		s, mem, tx := newOutputsOnlyStore(t, 1, 2)

		// .tx is genuinely absent, so the fallback is taken; the fallback read
		// itself then fails. That is a store failure, not "this node does not have
		// the transaction", and the caller must be able to tell them apart.
		s.externalStore = &failingReadStore{
			Store:        mem,
			failFileType: fileformat.FileTypeOutputs,
			failWith:     errors.NewServiceUnavailableError("read permits exhausted"),
		}

		got, err := s.getExternalTransactionOrOutputs(context.Background(), *tx.TxIDChainHash())

		require.Error(t, err)
		require.Nil(t, got)
		require.False(t, errors.Is(err, errors.ErrTxNotFound),
			"a failure to read the outputs blob must not masquerade as the tx being absent")
		require.True(t, errors.Is(err, errors.ErrServiceUnavailable),
			"the original failure must be preserved, got %v", err)
	})

	t.Run("missing .tx and missing .outputs is both not-found and our fault", func(t *testing.T) {
		// An external record with neither blob is this node's storage contradicting
		// itself, not a transaction it legitimately lacks, so the error carries both
		// classifications. ErrTxNotFound is what netsync's DAH'd-parent recovery and
		// subtreevalidation's continue-the-level branch key on; ErrStorageError is
		// what errors.IsTransientLocalError keys on, and legacy/peer_server.go's
		// shouldDisconnectOnBlockErr is its negation — so losing it would disconnect
		// a peer for our missing blob.
		s, _, tx := newOutputsOnlyStore(t)

		// newOutputsOnlyStore with no live indexes still writes an .outputs blob, so
		// remove both representations to reach the neither-stored state.
		require.NoError(t, s.externalStore.Del(context.Background(), tx.TxIDChainHash()[:], fileformat.FileTypeOutputs))

		got, err := s.getExternalTransactionOrOutputs(context.Background(), *tx.TxIDChainHash())
		require.Error(t, err)
		require.Nil(t, got)

		require.True(t, errors.Is(err, errors.ErrTxNotFound),
			"what is missing must stay visible to callers that branch on it, got %v", err)
		require.True(t, errors.Is(err, errors.ErrStorageError),
			"whose fault it is must stay visible: this classifies as a local storage fault, got %v", err)
		require.True(t, errors.IsTransientLocalError(err),
			"a missing blob of ours must not be attributed to the peer that sent the block, got %v", err)
	})

	t.Run("missing .tx and missing .outputs reports not found", func(t *testing.T) {
		chainParams := chaincfg.RegressionNetParams
		tSettings := &settings.Settings{}
		tSettings.ChainCfgParams = &chainParams

		s := &Store{
			ctx:           context.Background(),
			externalStore: memory.New(),
			settings:      tSettings,
			logger:        ulogger.TestLogger{},
		}

		tx := &bt.Tx{Version: 1, Inputs: []*bt.Input{{}}}

		got, err := s.getExternalTransactionOrOutputs(context.Background(), *tx.TxIDChainHash())
		require.Error(t, err)
		require.Nil(t, got)
		require.True(t, errors.Is(err, errors.ErrTxNotFound),
			"with neither representation stored the answer is not-found, got %v", err)
	})
}
