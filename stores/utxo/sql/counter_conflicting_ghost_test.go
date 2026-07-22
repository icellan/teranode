package sql

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/stretchr/testify/require"
)

// TestCounterConflictingGhostSpender_WalkToleratesGhost reproduces, against a
// real store, the field failure behind the counter-conflicting wedge (#1320):
//
// A multi-input transaction is validated and makes its valid spends, but errors
// on another input (already spent, or any other failure), so the transaction
// record itself is never created — while the completed spends survive. The
// parent outputs are left recording a spender that does not exist (a "ghost").
// When a conflicting transaction spending one of those outputs is later
// processed, the counter-conflicting walk used to fail with TX_NOT_FOUND (block
// wedged forever), and conflict resolution could never spend the slot because
// the ghost's dangling spend was unclearable.
//
// The test plants exactly that end state (the ghost's spends applied through the
// store, its record never created), then asserts the walk tolerates the ghost.
//
// The full resolution flow (ProcessConflicting healing the contested slot) is
// covered against real Aerospike in
// stores/utxo/aerospike/counter_conflicting_ghost_test.go — it cannot run here:
// SetConflicting opens a write transaction and then reads via the pool, which
// self-deadlocks on SQLite (see setconflicting_cascade_test.go).
func TestCounterConflictingGhostSpender_WalkToleratesGhost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, parentTx := setup(ctx, t)

	_, err := store.Create(ctx, parentTx, 0)
	require.NoError(t, err)

	// The ghost: a multi-input tx spending both parent outputs through the store,
	// whose own record is never created (its validation failed on another input).
	ghostTx := spendingTxWithFee(t, parentTx, 0, 1)
	_, err = store.Spend(ctx, ghostTx, store.GetBlockHeight()+1)
	require.NoError(t, err)
	// deliberately NO store.Create(ghostTx): the spender record does not exist

	// The conflicting tx spends parent output 0 — the slot the ghost holds. The
	// validator marked it conflicting when its spend collided with the ghost's;
	// create it in that state (a conflicting tx holds no spends of its own).
	conflictingTx := spendingTxWithFee(t, parentTx, 0)
	_, err = store.Create(ctx, conflictingTx, 0, utxo.WithConflicting(true))
	require.NoError(t, err)

	conflictingTxHash := *conflictingTx.TxIDChainHash()

	// 1. The counter-conflicting walk must tolerate the confirmed ghost instead
	// of failing with TX_NOT_FOUND — this is what unwedges block validation.
	counterHashes, err := utxo.GetCounterConflictingTxHashes(ctx, store, conflictingTxHash)
	require.NoError(t, err)
	require.Equal(t, []chainhash.Hash{conflictingTxHash}, counterHashes)

	// Both ghost slots are still recorded as spent by the ghost — the tolerance
	// is read-side only; healing happens in ProcessConflicting (aerospike test).
	utxoHash0, err := util.UTXOHashFromOutput(parentTx.TxIDChainHash(), parentTx.Outputs[0], 0)
	require.NoError(t, err)

	resp, err := store.GetSpend(ctx, &utxo.Spend{TxID: parentTx.TxIDChainHash(), Vout: 0, UTXOHash: utxoHash0})
	require.NoError(t, err)
	require.NotNil(t, resp.SpendingData)
	require.True(t, resp.SpendingData.TxID.Equal(*ghostTx.TxIDChainHash()),
		"the walk must not modify the dangling slot")
}

// spendingTxWithFee builds a tx spending the given parent outputs, paying out
// less than it consumes so the store's fee check on Create passes.
func spendingTxWithFee(t *testing.T, parentTx *bt.Tx, vOuts ...uint32) *bt.Tx {
	t.Helper()

	newTx := bt.NewTx()

	total := uint64(0)
	for _, vOut := range vOuts {
		require.NoError(t, newTx.From(parentTx.TxIDChainHash().String(), vOut, parentTx.Outputs[vOut].LockingScript.String(), parentTx.Outputs[vOut].Satoshis))
		total += parentTx.Outputs[vOut].Satoshis
	}

	require.NoError(t, newTx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", total/2))

	for _, input := range newTx.Inputs {
		input.UnlockingScript = &bscript.Script{bscript.OpTRUE}
	}

	return newTx
}
