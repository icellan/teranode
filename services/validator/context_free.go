package validator

import (
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
)

// CheckTransactionContextFree runs the checks on a transaction that need no parent data
// and no chain state: not a coinbase, at least one input and one output, no null or
// duplicate prevout, output values in the money range, and the policy size limit. It is
// for a transaction that cannot be fully validated yet; it never replaces full validation.
func CheckTransactionContextFree(tx *bt.Tx, policy *settings.PolicySettings) error {
	if tx.IsCoinbase() {
		return errors.NewTxInvalidError("coinbase transactions are not supported")
	}

	if len(tx.Inputs) == 0 {
		return errors.NewTxInvalidError("bad-txns-vin-empty")
	}

	if len(tx.Outputs) == 0 {
		return errors.NewTxInvalidError("bad-txns-vout-empty")
	}

	type outpoint struct {
		hash  chainhash.Hash
		index uint32
	}

	seen := make(map[outpoint]struct{}, len(tx.Inputs))

	for _, input := range tx.Inputs {
		op := outpoint{hash: *input.PreviousTxIDChainHash(), index: input.PreviousTxOutIndex}

		if op.index == 0xffffffff && op.hash.IsEqual(&chainhash.Hash{}) {
			return errors.NewTxInvalidError("bad-txns-prevout-null")
		}

		if _, dup := seen[op]; dup {
			return errors.NewTxInvalidError("bad-txns-inputs-duplicate")
		}

		seen[op] = struct{}{}
	}

	// checkOutputValues reads no validator state
	if _, err := (&TxValidator{}).checkOutputValues(tx); err != nil {
		return err
	}

	if policy != nil && policy.MaxTxSizePolicy > 0 && tx.Size() > policy.MaxTxSizePolicy {
		return errors.NewTxPolicyError("tx-size")
	}

	return nil
}
