package utxo

// Outcome label values for the partial-spend rollback counter
// (teranode_<backend>_utxo_partial_spend_rollbacks{outcome=...}).
//
// Shared by every store implementation on purpose: the counter is the dashboard
// signal for a corruption-prevention invariant (#1214), so a backend that spells
// an outcome differently — or omits one — makes the same query mean different
// things per backend. Add a value here and to RollbackOutcomes together, never in
// a store package.
const (
	// RollbackOutcomeFired: the spender had no record, so its partial spends were
	// dangling refs and were reverted.
	RollbackOutcomeFired = "fired"
	// RollbackOutcomeSpenderExists: the spender has a record, so the refs are not
	// dangling and the slots belong to a live tx — left alone.
	RollbackOutcomeSpenderExists = "spender_exists"
	// RollbackOutcomeIndeterminate: the existence probe itself failed, so the
	// rollback was skipped and a dangling ref may survive. Nothing detects that
	// yet, which makes this counter the only signal it happened.
	RollbackOutcomeIndeterminate = "indeterminate"
	// RollbackOutcomeTransientLock: at least one spend failed with ErrTxLocked
	// while nothing in the batch made the tx unwinnable, so a concurrent attempt
	// at the same txid may legitimately own the slots and the rollback was
	// suppressed. This is the deliberately-uncovered flavour of #1214.
	RollbackOutcomeTransientLock = "transient_lock"
)

// RollbackOutcomes is every value the outcome label can take, so each store's
// tests can assert the full set and catch a rename before it breaks a dashboard.
var RollbackOutcomes = []string{
	RollbackOutcomeFired,
	RollbackOutcomeSpenderExists,
	RollbackOutcomeIndeterminate,
	RollbackOutcomeTransientLock,
}
