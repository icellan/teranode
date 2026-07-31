package sql

import (
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

// hasTransientLockFailure reports whether any spend failed with ErrTxLocked.
//
// Mirrors the aerospike store's helper of the same name. Deliberately duplicated
// rather than shared: the two stores' Spend implementations are independent, and
// this predicate is the only thing they would have in common. Keep them in step.
//
// ErrTxLocked is the one failure class that means "try again in a moment": the
// parent's record is locked only for the two-phase-commit window between its own
// create and its block-assembly ack, which is milliseconds. A concurrent attempt
// at the SAME txid (duplicate relay, or a second validator pod behind the same
// store) can therefore be spending those inputs successfully while this attempt
// sees the lock — and because spending_data is just {TxID, Vin} with no attempt
// identity, rolling back here can clear the winner's slots, leaving a live or
// mined transaction whose input reads unspent. That is the inverse of #1214 and
// worse: the node would go on to accept and mine a double-spend.
//
// So the ErrTxLocked flavour of the #1214 dangling ref is NOT fixed by this
// rollback. Covering it needs ordering that makes the record exist before any
// spend (create-first, #1355) or attempt identity in the stored spending data
// (#1291); neither belongs in an error-path rollback.
//
// Every other class is safe to roll back: either this txid can never win (spent,
// conflicting, frozen, hash mismatch), so no concurrent attempt at it can be a
// legitimate owner, or the failure is not a "someone else may be winning right
// now" state at all (missing parent, infrastructure errors).
func hasTransientLockFailure(spends []*utxo.Spend) bool {
	for _, spend := range spends {
		if spend != nil && spend.Err != nil && errors.Is(spend.Err, errors.ErrTxLocked) {
			return true
		}
	}

	return false
}
