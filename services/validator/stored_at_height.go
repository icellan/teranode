package validator

import (
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/util"
)

// InputParent is what CheckStoredTxAtHeight needs to know of an input's parent.
type InputParent struct {
	IsCoinbase bool
	// Height is the lowest height the parent was mined at.
	Height uint32
}

// CheckStoredTxAtHeight runs, on a tx already in the UTXO store, the checks whose verdict depends
// on the height of the block it is in: no input spends a coinbase output not yet mature, and the
// tx is final. Another candidate block, at another height, may have created the record. It reads
// and writes nothing, so it never takes a spend: parents gives each input's parent, in input order.
// An immature spend is TX_INVALID, as the validator answers it on the block path; a tx that is not
// final fails as the validator fails it, without a verdict.
func CheckStoredTxAtHeight(tx *bt.Tx, parents []InputParent, blockHeight uint32, tSettings *settings.Settings, opts *Options) error {
	txID := tx.TxIDChainHash().String()

	if len(parents) != len(tx.Inputs) {
		return errors.NewProcessingError("[CheckStoredTxAtHeight][%s] %d parents for %d inputs", txID, len(parents), len(tx.Inputs))
	}

	// the stores make a coinbase output spendable from its height plus the maturity
	maturity := uint32(tSettings.ChainCfgParams.CoinbaseMaturity)

	for i, parent := range parents {
		if parent.IsCoinbase && blockHeight < parent.Height+maturity {
			return errors.NewTxInvalidError("[CheckStoredTxAtHeight][%s] spends a coinbase output not yet mature at height %d", txID, blockHeight,
				errors.NewTxCoinbaseImmatureError("input %d spends a coinbase output mined at %d, spendable from %d", i, parent.Height, parent.Height+maturity))
		}
	}

	// a block path checks finality against its candidate times, never the tip's
	comparisonTime, skipFinality, err := selectFinalityComparisonTime(opts, blockHeight, uint32(tSettings.ChainCfgParams.CSVHeight), utxo.BlockState{})
	if err != nil {
		return err
	}

	if !skipFinality {
		if err = util.IsTransactionFinal(tx, blockHeight, comparisonTime); err != nil {
			return errors.NewUtxoNonFinalError("[CheckStoredTxAtHeight][%s] transaction is not final", txID, err)
		}
	}

	return nil
}
