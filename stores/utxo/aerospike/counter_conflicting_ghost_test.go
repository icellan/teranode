package aerospike_test

import (
	"testing"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/tests"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	"github.com/stretchr/testify/require"
)

// buildSpendingTx builds a tx spending the given parent outputs, paying out
// less than it consumes so the store's fee check on Create passes.
func buildSpendingTx(t *testing.T, parentTx *bt.Tx, vOuts ...uint32) *bt.Tx {
	t.Helper()

	newTx := bt.NewTx()

	total := uint64(0)
	for _, vOut := range vOuts {
		require.NoError(t, newTx.From(
			parentTx.TxIDChainHash().String(), vOut,
			parentTx.Outputs[vOut].LockingScript.String(),
			parentTx.Outputs[vOut].Satoshis,
		))
		total += parentTx.Outputs[vOut].Satoshis
	}

	require.NoError(t, newTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", total/2))

	return newTx
}

// TestCounterConflictingGhostSpender_Aerospike reproduces, against real
// Aerospike, the field failure behind the counter-conflicting wedge (#1320):
//
// A multi-input transaction is validated and makes its valid spends, but errors
// on another input (already spent, or some other failure), so the transaction
// record itself is never created — while the completed spends survive. The
// parent outputs are left recording a spender that does not exist (a "ghost").
// A conflicting transaction spending one of those outputs then cannot be
// processed: the counter-conflicting walk fails with TX_NOT_FOUND (wedging
// block validation forever), and even if it passed, the winner could never
// spend the slot — the Lua spend rejects a mismatched occupant and the unspend
// ownership check refuses to clear a spend the caller does not own.
//
// The test plants exactly that end state, then asserts the walk tolerates the
// confirmed ghost and ProcessConflicting heals the contested slot — exercising
// the real Lua unspend ownership check with the recorded ghost spending data.
func TestCounterConflictingGhostSpender_Aerospike(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	cleanDB(t, client)

	// tests.ParentTx is the grandparent — tx's input references it and
	// SetConflicting resolves parents internally.
	_, err := store.Create(ctx, tests.ParentTx, 999)
	require.NoError(t, err)

	_, err = store.Create(ctx, tx, 1000)
	require.NoError(t, err)

	// The ghost: a multi-input tx spending tx:0 and tx:1 through the store,
	// whose own record is never created (its validation failed on another input).
	ghostTx := buildSpendingTx(t, tx, 0, 1)
	_, err = store.Spend(ctx, ghostTx, store.GetBlockHeight()+1)
	require.NoError(t, err)
	// deliberately NO store.Create(ghostTx): the spender record does not exist

	// The conflicting tx spends tx:0 — the slot the ghost holds. The validator
	// marked it conflicting when its spend collided with the ghost's.
	conflictingTx := buildSpendingTx(t, tx, 0)
	_, err = store.Create(ctx, conflictingTx, 1001, utxo.WithConflicting(true))
	require.NoError(t, err)

	conflictingTxHash := *conflictingTx.TxIDChainHash()

	// 1. The counter-conflicting walk must tolerate the confirmed ghost instead
	// of failing with TX_NOT_FOUND — this is what unwedges block validation.
	counterHashes, err := utxo.GetCounterConflictingTxHashes(ctx, store, conflictingTxHash)
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{conflictingTxHash}, counterHashes)

	// 2. Conflict resolution must clear the ghost's dangling spend — passing the
	// recorded ghost spending data through the Lua unspend ownership check — and
	// let the winner spend the slot.
	losingMap, _, err := utxo.ProcessConflicting(ctx, store, store.GetBlockHeight()+1,
		[]chainhash.Hash{conflictingTxHash}, map[chainhash.Hash]struct{}{})
	require.NoError(t, err)
	require.NotNil(t, losingMap)

	md, err := store.Get(ctx, tx.TxIDChainHash(), fields.Utxos)
	require.NoError(t, err)

	// The contested slot (tx:0) now records the winner.
	require.NotNil(t, md.SpendingDatas[0], "contested slot must be spent after resolution")
	require.True(t, md.SpendingDatas[0].TxID.Equal(conflictingTxHash),
		"contested slot must be spent by the winner after resolution, not the ghost")

	// The ghost's uncontested slot (tx:1) stays as it was: without a record the
	// ghost's other spends are not enumerable; a slot is healed when contested.
	require.NotNil(t, md.SpendingDatas[1])
	require.True(t, md.SpendingDatas[1].TxID.Equal(*ghostTx.TxIDChainHash()),
		"uncontested ghost slot must be untouched by resolution")

	// The winner is no longer flagged conflicting, and its parent is unlocked.
	mdWinner, err := store.Get(ctx, &conflictingTxHash, fields.Conflicting)
	require.NoError(t, err)
	require.False(t, mdWinner.Conflicting)

	mdParent, err := store.Get(ctx, tx.TxIDChainHash(), fields.Locked)
	require.NoError(t, err)
	require.False(t, mdParent.Locked, "parent must be unlocked after step 5")
}

// TestCounterConflictingGhostDescendant_Aerospike reproduces the depth-2
// variant of the ghost wedge against real Aerospike: the parent output's
// spender L is alive (and conflicting-eligible), but L's own output slot
// records a spender whose record was never created — the same write failure
// one level deeper. The walk's BFS must skip the ghost descendant, and
// ProcessConflicting must demote L and let the winner spend the contested
// slot, with the cascade probing (not choking on) the record-less child.
func TestCounterConflictingGhostDescendant_Aerospike(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	cleanDB(t, client)

	_, err := store.Create(ctx, tests.ParentTx, 999)
	require.NoError(t, err)

	_, err = store.Create(ctx, tx, 1000)
	require.NoError(t, err)

	// L: the live spender of tx:0 (it also takes tx:1, which keeps its txid
	// distinct from the winner's — both spend the contested tx:0).
	liveLoserTx := buildSpendingTx(t, tx, 0, 1)
	_, err = store.Create(ctx, liveLoserTx, 0)
	require.NoError(t, err)

	_, err = store.Spend(ctx, liveLoserTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	// The ghost descendant: spends L's output through the store, record never
	// created.
	ghostTx := buildSpendingTx(t, liveLoserTx, 0)
	_, err = store.Spend(ctx, ghostTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	// The winner: double-spends tx:0, marked conflicting by the validator.
	conflictingTx := buildSpendingTx(t, tx, 0)
	_, err = store.Create(ctx, conflictingTx, 1001, utxo.WithConflicting(true))
	require.NoError(t, err)

	conflictingTxHash := *conflictingTx.TxIDChainHash()
	liveLoserTxHash := *liveLoserTx.TxIDChainHash()

	// 1. The walk must tolerate the ghost descendant: L is enumerated as the
	// counter, the ghost is skipped.
	counterHashes, err := utxo.GetCounterConflictingTxHashes(ctx, store, conflictingTxHash)
	require.NoError(t, err)
	require.ElementsMatch(t, []chainhash.Hash{conflictingTxHash, liveLoserTxHash}, counterHashes)

	// 2. Conflict resolution must demote L (the cascade skips the record-less
	// ghost child) and let the winner spend the contested slot.
	losingMap, _, err := utxo.ProcessConflicting(ctx, store, store.GetBlockHeight()+1,
		[]chainhash.Hash{conflictingTxHash}, map[chainhash.Hash]struct{}{})
	require.NoError(t, err)
	require.NotNil(t, losingMap)

	md, err := store.Get(ctx, tx.TxIDChainHash(), fields.Utxos)
	require.NoError(t, err)
	require.NotNil(t, md.SpendingDatas[0], "contested slot must be spent after resolution")
	require.True(t, md.SpendingDatas[0].TxID.Equal(conflictingTxHash),
		"contested slot must be spent by the winner after resolution")

	mdLoser, err := store.Get(ctx, &liveLoserTxHash, fields.Conflicting)
	require.NoError(t, err)
	require.True(t, mdLoser.Conflicting, "the live loser must be demoted")
}

// TestCounterConflictingReapedSpender_Aerospike covers the counterpart of the
// ghost tolerance: the spender record is absent not because it was never
// created, but because the DAH pruner reaped it after it was mined and fully
// spent. The pruner marks every reaped child in its surviving parents'
// deletedChildren bin; the walk must fail closed on such a spender instead of
// treating its settled spend as a ghost — clearing it would let a block
// double-spend a settled output.
func TestCounterConflictingReapedSpender_Aerospike(t *testing.T) {
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	client, store, ctx, deferFn := initAerospike(t, tSettings, logger)
	t.Cleanup(deferFn)

	cleanDB(t, client)

	_, err := store.Create(ctx, tests.ParentTx, 999)
	require.NoError(t, err)

	_, err = store.Create(ctx, tx, 1000)
	require.NoError(t, err)

	// The reaped spender: spent tx:0 and tx:1 through the store; its record is
	// absent (as after a DAH delete) and the parent's deletedChildren bin lists
	// it, exactly as the pruner leaves things.
	reapedTx := buildSpendingTx(t, tx, 0, 1)
	_, err = store.Spend(ctx, reapedTx, store.GetBlockHeight()+1)
	require.NoError(t, err)

	keySource := uaerospike.CalculateKeySource(tx.TxIDChainHash(), 0, store.GetUtxoBatchSize())
	parentKey, aErr := aerospike.NewKey(store.GetNamespace(), store.GetName(), keySource)
	require.NoError(t, aErr)

	aErr = client.Put(nil, parentKey, aerospike.BinMap{
		fields.DeletedChildren.String(): map[interface{}]interface{}{
			reapedTx.TxIDChainHash().String(): true,
		},
	})
	require.NoError(t, aErr)

	conflictingTx := buildSpendingTx(t, tx, 0)
	_, err = store.Create(ctx, conflictingTx, 1001, utxo.WithConflicting(true))
	require.NoError(t, err)

	conflictingTxHash := *conflictingTx.TxIDChainHash()

	// 1. The walk must fail closed on the reaped spender, not tolerate it.
	_, err = utxo.GetCounterConflictingTxHashes(ctx, store, conflictingTxHash)
	require.Error(t, err)
	require.Contains(t, err.Error(), "deletedChildren")

	// 2. Conflict resolution must refuse likewise and leave the settled slot
	// untouched.
	_, _, err = utxo.ProcessConflicting(ctx, store, store.GetBlockHeight()+1,
		[]chainhash.Hash{conflictingTxHash}, map[chainhash.Hash]struct{}{})
	require.Error(t, err)

	md, err := store.Get(ctx, tx.TxIDChainHash(), fields.Utxos)
	require.NoError(t, err)
	require.NotNil(t, md.SpendingDatas[0], "settled slot must still be spent")
	require.True(t, md.SpendingDatas[0].TxID.Equal(*reapedTx.TxIDChainHash()),
		"settled slot must remain held by the reaped spender")
}
