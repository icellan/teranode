package subtreevalidation

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/validator"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/test/utils/transactions"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// delegatingRecorder records which transactions reached the validator, and with which
// options, and lets the real validator decide.
type delegatingRecorder struct {
	validator.Interface

	mu    sync.Mutex
	calls map[chainhash.Hash][]*validator.Options
}

func (r *delegatingRecorder) ValidateWithOptions(ctx context.Context, tx *bt.Tx, blockHeight uint32, opts *validator.Options) (*meta.Data, error) {
	r.mu.Lock()
	r.calls[*tx.TxIDChainHash()] = append(r.calls[*tx.TxIDChainHash()], opts)
	r.mu.Unlock()

	return r.Interface.ValidateWithOptions(ctx, tx, blockHeight, opts)
}

func (r *delegatingRecorder) validated(h chainhash.Hash) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.calls[h]) > 0
}

// conflictingFixture is a node that accepted winner, which spends the only output of
// a mined parent. loser spends the same output, and child spends loser's output: the
// shape a peer's subtree has when the peer accepted loser instead.
type conflictingFixture struct {
	server   *Server
	store    *sql.Store
	recorder *delegatingRecorder
	winner   *bt.Tx
	loser    *bt.Tx
	child    *bt.Tx
}

func newConflictingFixture(t *testing.T) *conflictingFixture {
	t.Helper()

	InitPrometheusMetrics()

	ctx := t.Context()
	logger := ulogger.TestLogger{}

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockAssembly.Disabled = true
	tSettings.SubtreeValidation.SpendBatcherSize = 10

	utxoURL, err := url.Parse("sqlitememory:///conflicting_kept_aside_" + t.Name())
	require.NoError(t, err)

	store, err := sql.New(ctx, logger, tSettings, utxoURL)
	require.NoError(t, err)
	store.RawDB().SetMaxOpenConns(1)
	require.NoError(t, store.SetBlockHeight(100))
	require.NoError(t, store.SetMedianBlockTime(uint32(time.Now().Unix()))) //nolint:gosec

	privateKey, publicKey := bec.PrivateKeyFromBytes([]byte("CONFLICTING_KEPT_ASIDE_TEST_KEY!"))

	parent := transactions.Create(t,
		transactions.WithCoinbaseData(1, "/conflicting kept aside test/"),
		transactions.WithP2PKHOutputs(1, 100_000, publicKey),
	)
	_, err = store.Create(ctx, parent, 1, utxostore.WithMinedBlockInfo(utxostore.MinedBlockInfo{BlockID: 1, BlockHeight: 1, SubtreeIdx: 0}))
	require.NoError(t, err)

	winner := transactions.Create(t,
		transactions.WithPrivateKey(privateKey),
		transactions.WithInput(parent, 0, privateKey),
		transactions.WithP2PKHOutputs(1, 90_000, publicKey),
	)
	loser := transactions.Create(t,
		transactions.WithPrivateKey(privateKey),
		transactions.WithInput(parent, 0, privateKey),
		transactions.WithP2PKHOutputs(1, 80_000, publicKey),
	)
	child := transactions.Create(t,
		transactions.WithPrivateKey(privateKey),
		transactions.WithInput(loser, 0, privateKey),
		transactions.WithP2PKHOutputs(1, 70_000, publicKey),
	)

	realValidator, err := validator.New(ctx, logger, tSettings, store, nil, nil, nil, nil, nil)
	require.NoError(t, err)

	_, err = realValidator.Validate(ctx, winner, 101)
	require.NoError(t, err)

	recorder := &delegatingRecorder{Interface: realValidator, calls: map[chainhash.Hash][]*validator.Options{}}

	server := &Server{
		logger:                logger,
		settings:              tSettings,
		utxoStore:             store,
		validatorClient:       recorder,
		policyRejectedTxCache: newTxPolicyRejectedCache(1024 * 1024),
	}

	return &conflictingFixture{server: server, store: store, recorder: recorder, winner: winner, loser: loser, child: child}
}

func (f *conflictingFixture) requireNotStored(t *testing.T, txs ...*bt.Tx) {
	t.Helper()

	for _, tx := range txs {
		err := f.store.GetMeta(t.Context(), tx.TxIDChainHash(), &meta.Data{})
		require.True(t, errors.Is(err, errors.ErrTxNotFound), "%s must not be in the UTXO store: %v", tx.TxID(), err)
	}
}

// TestBlessMissingTransactions_KeepsConflictingAside pins the peer-subtree rule: a
// transaction that conflicts with one this node accepted is kept aside, and so is its
// descendant, without either reaching the UTXO store. Only a block with proof of work
// may make the node write a conflicting record.
func TestBlessMissingTransactions_KeepsConflictingAside(t *testing.T) {
	t.Run("loser and its child in one subtree are kept aside", func(t *testing.T) {
		f := newConflictingFixture(t)

		missing := []missingTx{{tx: f.loser, idx: 0}, {tx: f.child, idx: 1}}
		txMetaSlice := make([]metaSliceItem, len(missing))

		err := f.server.blessMissingTransactions(t.Context(), chainhash.Hash{}, missing, "", "", txMetaSlice, 101, map[uint32]bool{})
		require.NoError(t, err, "a subtree whose only failures are conflicting txs must not fail")

		require.True(t, txMetaSlice[0].keptAside)
		require.True(t, txMetaSlice[1].keptAside)
		require.False(t, txMetaSlice[0].isSet)
		require.False(t, txMetaSlice[1].isSet)

		require.False(t, f.recorder.validated(*f.child.TxIDChainHash()), "a child of a kept-aside tx is never validated on the peer path")
		f.requireNotStored(t, f.loser, f.child)

		for _, opts := range f.recorder.calls[*f.loser.TxIDChainHash()] {
			require.False(t, opts.CreateConflicting, "the peer path must never create conflicting records")
		}
	})

	t.Run("child whose parent is cached as conflicting is kept aside", func(t *testing.T) {
		f := newConflictingFixture(t)
		f.server.policyRejectedTxCache.SetConflicting(*f.loser.TxIDChainHash(), f.loser)

		missing := []missingTx{{tx: f.child, idx: 0}}
		txMetaSlice := make([]metaSliceItem, len(missing))

		err := f.server.blessMissingTransactions(t.Context(), chainhash.Hash{}, missing, "", "", txMetaSlice, 101, map[uint32]bool{})
		require.NoError(t, err)
		require.True(t, txMetaSlice[0].keptAside)
		require.False(t, f.recorder.validated(*f.child.TxIDChainHash()))
		f.requireNotStored(t, f.child)
	})

	t.Run("child of a parent stored as conflicting is kept aside", func(t *testing.T) {
		f := newConflictingFixture(t)

		// an earlier block stored the loser as conflicting
		_, err := f.recorder.Interface.ValidateWithOptions(t.Context(), f.loser, 101, validator.ProcessOptions(validator.WithCreateConflicting(true)))
		require.True(t, errors.Is(err, errors.ErrTxConflicting), "fixture: loser must be stored as conflicting: %v", err)

		missing := []missingTx{{tx: f.child, idx: 0}}
		txMetaSlice := make([]metaSliceItem, len(missing))

		err = f.server.blessMissingTransactions(t.Context(), chainhash.Hash{}, missing, "", "", txMetaSlice, 101, map[uint32]bool{})
		require.NoError(t, err)
		require.True(t, txMetaSlice[0].keptAside)
		f.requireNotStored(t, f.child)
	})

	t.Run("child whose parent is unknown still fails", func(t *testing.T) {
		f := newConflictingFixture(t)

		missing := []missingTx{{tx: f.child, idx: 0}}
		txMetaSlice := make([]metaSliceItem, len(missing))

		err := f.server.blessMissingTransactions(t.Context(), chainhash.Hash{}, missing, "", "", txMetaSlice, 101, map[uint32]bool{})
		require.Error(t, err, "a missing parent that is not known to conflict keeps failing the subtree")
		require.False(t, txMetaSlice[0].keptAside)
	})

	t.Run("block path creates the loser as conflicting instead", func(t *testing.T) {
		f := newConflictingFixture(t)

		missing := []missingTx{{tx: f.loser, idx: 0}, {tx: f.child, idx: 1}}
		txMetaSlice := make([]metaSliceItem, len(missing))

		err := f.server.blessMissingTransactions(t.Context(), chainhash.Hash{}, missing, "", "", txMetaSlice, 101, map[uint32]bool{},
			validator.WithCreateConflicting(true))
		require.NoError(t, err)

		for i := range missing {
			require.False(t, txMetaSlice[i].keptAside, "nothing is kept aside behind proof of work")
			require.True(t, txMetaSlice[i].isSet)
			require.True(t, txMetaSlice[i].conflicting)
		}
	})
}
