package blockassembly

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	utxofields "github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// blockchainMembershipMock returns a blockchain client mock that reports every
// block as on/off the longest chain per onChain. notFound makes GetBlockHeader
// report the block as unknown (treated as off-chain).
func blockchainMembershipMock(onChain, notFound bool) *blockchain.Mock {
	m := &blockchain.Mock{}
	if notFound {
		m.On("GetBlockHeader", mock.Anything, mock.Anything).
			Return((*model.BlockHeader)(nil), (*model.BlockHeaderMeta)(nil), errors.NewBlockNotFoundError("unknown block"))
		return m
	}

	m.On("GetBlockHeader", mock.Anything, mock.Anything).
		Return(&model.BlockHeader{}, &model.BlockHeaderMeta{ID: 7}, nil)
	m.On("CheckBlockIsInCurrentChain", mock.Anything, mock.Anything).Return(onChain, nil)

	return m
}

// replaySpyStore embeds the utxo mock and records WAL lifecycle calls so the
// BlockAssembler replay path can be asserted without a full store backend.
type replaySpyStore struct {
	*utxo.MockUtxostore

	pending    []utxo.ConflictIntent
	pendingErr error
	completed  []chainhash.Hash
}

func (s *replaySpyStore) PendingConflictIntents(_ context.Context) ([]utxo.ConflictIntent, error) {
	return s.pending, s.pendingErr
}

func (s *replaySpyStore) BeginConflictIntent(_ context.Context, _ utxo.ConflictIntent) error {
	return nil
}

func (s *replaySpyStore) CompleteConflictIntent(_ context.Context, intentID chainhash.Hash) error {
	s.completed = append(s.completed, intentID)
	return nil
}

// newReplayTestAssembler builds the minimal BlockAssembler needed to drive
// replayPendingConflictIntents — it touches utxoStore, blockchainClient (for
// the chain-membership gate) and logger.
func newReplayTestAssembler(store utxo.Store, bc blockchain.ClientI) *BlockAssembler {
	// Production initialises metrics in Server.New; do the same so the replay
	// path's gauge/counter are non-nil.
	initPrometheusMetrics()

	return &BlockAssembler{
		logger:           ulogger.TestLogger{},
		utxoStore:        store,
		blockchainClient: bc,
	}
}

// TestReplayPendingConflictIntents_NoPending is a no-op when the WAL is empty.
func TestReplayPendingConflictIntents_NoPending(t *testing.T) {
	spy := &replaySpyStore{MockUtxostore: &utxo.MockUtxostore{}}
	b := newReplayTestAssembler(spy, blockchainMembershipMock(false, true))

	b.replayPendingConflictIntents(context.Background())

	require.Empty(t, spy.completed, "nothing to complete when no intents are pending")
}

// TestReplayPendingConflictIntents_LoadErrorIsNotFatal verifies a failure to
// read the WAL is logged/counted but does not panic or block startup.
func TestReplayPendingConflictIntents_LoadErrorIsNotFatal(t *testing.T) {
	spy := &replaySpyStore{
		MockUtxostore: &utxo.MockUtxostore{},
		pendingErr:    errors.NewStorageError("wal read failed"),
	}
	b := newReplayTestAssembler(spy, blockchainMembershipMock(false, true))

	require.NotPanics(t, func() {
		b.replayPendingConflictIntents(context.Background())
	})
	require.Empty(t, spy.completed)
}

// TestReplayPendingConflictIntents_ReverseReplayCompletes drives a reverse
// intent through replay. The demoted tx resolves to a record with a nil body,
// so ReverseProcessConflicting treats it as already-resolved (a no-op success)
// and the intent is cleared from the WAL.
func TestReplayPendingConflictIntents_ReverseReplayCompletes(t *testing.T) {
	demoted := chainhash.HashH([]byte("replay-demoted"))

	intent := utxo.ConflictIntent{
		Kind:        utxo.ConflictIntentReverse,
		BlockHeight: 500,
		TxHashes:    []chainhash.Hash{demoted},
		StartedAt:   1,
	}

	mockStore := &utxo.MockUtxostore{}
	// Demoted tx resolves with a nil body → ReverseProcessConflicting skips it
	// (nothing to restore) and returns success.
	mockStore.On("Get", mock.Anything, &demoted, mock.Anything).
		Return(&meta.Data{}, nil)

	spy := &replaySpyStore{MockUtxostore: mockStore, pending: []utxo.ConflictIntent{intent}}
	// Block is unknown/off-chain → reverse intent is NOT stale → it replays.
	b := newReplayTestAssembler(spy, blockchainMembershipMock(false, true))

	b.replayPendingConflictIntents(context.Background())

	require.Contains(t, spy.completed, intent.IntentID(), "successful replay must clear the intent from the WAL")
}

// TestReplayPendingConflictIntents_StaleForwardDiscarded covers the corruption
// the human reviewer flagged: a forward intent whose block is no longer on the
// longest chain (a later reverse superseded it) must be DISCARDED, not replayed
// — replaying would re-promote the loser and undo the reverse. The bare mock
// store has no Get expectation, so if ProcessConflicting were invoked the test
// would panic; passing proves the gate discarded the intent before re-running.
func TestReplayPendingConflictIntents_StaleForwardDiscarded(t *testing.T) {
	intent := utxo.ConflictIntent{
		Kind:        utxo.ConflictIntentForward,
		BlockHeight: 300,
		BlockHash:   chainhash.HashH([]byte("reorged-out-block")),
		TxHashes:    []chainhash.Hash{chainhash.HashH([]byte("stale-winner"))},
		StartedAt:   1,
	}

	spy := &replaySpyStore{MockUtxostore: &utxo.MockUtxostore{}, pending: []utxo.ConflictIntent{intent}}
	// Block exists but is NOT on the longest chain → forward intent is stale.
	b := newReplayTestAssembler(spy, blockchainMembershipMock(false, false))

	b.replayPendingConflictIntents(context.Background())

	require.Contains(t, spy.completed, intent.IntentID(), "a stale forward intent must be discarded (deleted) from the WAL")
}

// TestReplayPendingConflictIntents_StaleReverseDiscarded is the mirror: a reverse
// intent whose block is back on the longest chain (a later forward re-applied it)
// must be discarded, not replayed.
func TestReplayPendingConflictIntents_StaleReverseDiscarded(t *testing.T) {
	intent := utxo.ConflictIntent{
		Kind:        utxo.ConflictIntentReverse,
		BlockHeight: 300,
		BlockHash:   chainhash.HashH([]byte("re-applied-block")),
		TxHashes:    []chainhash.Hash{chainhash.HashH([]byte("stale-demoted"))},
		StartedAt:   1,
	}

	spy := &replaySpyStore{MockUtxostore: &utxo.MockUtxostore{}, pending: []utxo.ConflictIntent{intent}}
	// Block IS on the longest chain → reverse intent is stale.
	b := newReplayTestAssembler(spy, blockchainMembershipMock(true, false))

	b.replayPendingConflictIntents(context.Background())

	require.Contains(t, spy.completed, intent.IntentID(), "a stale reverse intent must be discarded (deleted) from the WAL")
}

// TestReplayPendingConflictIntents_RealStoreReverseConverges is the crash-
// recovery integration test (#861) against a REAL sqlitememory store and a real
// BlockAssembler. It reproduces the most common crash window — a SIGKILL AFTER
// ReverseProcessConflicting's terminal step but BEFORE the WAL completion delete
// — by leaving a reverse intent behind for an operation whose effects are
// already fully applied. Startup replay must re-run the reverse, observe it is
// already applied (idempotent no-op via isReverseFullyApplied), clear the WAL,
// and leave the UTXO state untouched.
func TestReplayPendingConflictIntents_RealStoreReverseConverges(t *testing.T) {
	initPrometheusMetrics()

	ctx := t.Context()
	items := setupBlockAssemblyTest(t)
	require.NotNil(t, items)

	store := items.utxoStore
	require.NoError(t, store.SetBlockHeight(10))

	// Parent tx with one spendable output (input references a nonexistent tx;
	// Create does not validate inputs, and PreviousTxSatoshis covers the fee).
	parent := bt.NewTx()
	parentIn := &bt.Input{PreviousTxOutIndex: 0, PreviousTxSatoshis: 200000, SequenceNumber: 0xFFFFFFFF, UnlockingScript: bscript.NewFromBytes([]byte{})}
	_ = parentIn.PreviousTxIDAdd(&chainhash.Hash{9, 9, 9})
	parent.Inputs = []*bt.Input{parentIn}
	parent.Outputs = []*bt.Output{{Satoshis: 100000, LockingScript: bscript.NewFromBytes([]byte{0x52})}}
	_, err := store.Create(ctx, parent, 1)
	require.NoError(t, err)
	parentHash := parent.TxIDChainHash()
	require.NoError(t, store.MarkTransactionsOnLongestChain(ctx, []chainhash.Hash{*parentHash}, true))

	// txD: the demoted loser, spends parent[0], created Conflicting=true.
	txD := bt.NewTx()
	require.NoError(t, txD.From(parentHash.String(), 0, parent.Outputs[0].LockingScript.String(), parent.Outputs[0].Satoshis))
	txD.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})
	txD.Outputs = []*bt.Output{{Satoshis: 90000, LockingScript: bscript.NewFromBytes([]byte{0x52})}}
	_, err = store.Create(ctx, txD, 10, utxo.WithConflicting(true))
	require.NoError(t, err)
	txDHash := txD.TxIDChainHash()

	// txC: the counter/winner, spends parent[0] for real so parent[0]'s spending
	// data points at C — the fully-applied post-reverse state.
	txC := bt.NewTx()
	require.NoError(t, txC.From(parentHash.String(), 0, parent.Outputs[0].LockingScript.String(), parent.Outputs[0].Satoshis))
	txC.Inputs[0].UnlockingScript = bscript.NewFromBytes([]byte{})
	txC.Outputs = []*bt.Output{{Satoshis: 80000, LockingScript: bscript.NewFromBytes([]byte{0x52})}}
	_, err = store.Create(ctx, txC, 10)
	require.NoError(t, err)
	_, err = store.Spend(ctx, txC, store.GetBlockHeight()+1)
	require.NoError(t, err)

	// The reverse for txD is already fully applied; only the WAL completion was
	// lost to the crash. Re-record the intent to simulate that.
	intent := utxo.ConflictIntent{
		Kind:        utxo.ConflictIntentReverse,
		BlockHeight: 10,
		TxHashes:    []chainhash.Hash{*txDHash},
		StartedAt:   1,
	}
	require.NoError(t, store.BeginConflictIntent(ctx, intent))

	pending, err := store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "intent should be pending before replay")

	// The intent's (zero) block hash is unknown to the chain → off-chain → a
	// reverse intent is not stale, so replay proceeds. Use a deterministic
	// membership mock rather than relying on the real client's not-found behaviour.
	items.blockAssembler.blockchainClient = blockchainMembershipMock(false, true)

	// Restart path.
	items.blockAssembler.replayPendingConflictIntents(ctx)

	// Converged: the WAL is clear and the UTXO state is unchanged.
	pending, err = store.PendingConflictIntents(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "replay must clear the WAL once the operation has converged")

	dMeta, err := store.Get(ctx, txDHash, utxofields.Conflicting)
	require.NoError(t, err)
	require.True(t, dMeta.Conflicting, "demoted tx must remain conflicting after idempotent reverse replay")
}

// TestReplayConflictIntent_UnknownKind surfaces an error for an unrecognised
// intent kind rather than silently skipping it.
func TestReplayConflictIntent_UnknownKind(t *testing.T) {
	b := newReplayTestAssembler(&replaySpyStore{MockUtxostore: &utxo.MockUtxostore{}}, blockchainMembershipMock(false, true))

	err := b.replayConflictIntent(context.Background(), utxo.ConflictIntent{
		Kind:     utxo.ConflictIntentKind("bogus"),
		TxHashes: []chainhash.Hash{chainhash.HashH([]byte("x"))},
	})

	require.Error(t, err)
}
