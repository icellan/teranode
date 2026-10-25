package subtreevalidation

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
)

// checkStoredConflictingAtHeight runs validator.CheckStoredTxAtHeight on every tx the lookups
// took from a conflicting record. The block path creates a tx as conflicting at its block's
// height, and another candidate block at another height may have created the record: a tx valid
// there can be invalid here, for instance spending a coinbase that matures in between. The check
// reads the tx and its parents and writes nothing; validating the tx again would spend inputs the
// record does not hold. txs, when not nil, holds the block's txs by index; otherwise a tx is read
// from the store.
func (u *Server) checkStoredConflictingAtHeight(ctx context.Context, txHashes []chainhash.Hash, txMetaSlice []metaSliceItem,
	txs []*bt.Tx, blockHeight uint32, opts *validator.Options) error {
	for i := range txMetaSlice {
		if !txMetaSlice[i].isSet || !txMetaSlice[i].conflicting {
			continue
		}

		var tx *bt.Tx
		if txs != nil {
			tx = txs[i]
		}

		if tx == nil {
			stored, err := u.utxoStore.Get(ctx, &txHashes[i], fields.Tx)
			if err != nil {
				return errors.NewProcessingError("[checkStoredConflictingAtHeight][%s] failed to read the stored conflicting tx", txHashes[i].String(), err)
			}

			if stored.Tx == nil {
				return errors.NewProcessingError("[checkStoredConflictingAtHeight][%s] the stored conflicting tx has no body", txHashes[i].String())
			}

			tx = stored.Tx
		}

		parents, err := u.inputParents(ctx, tx)
		if err != nil {
			return err
		}

		if err = validator.CheckStoredTxAtHeight(tx, parents, blockHeight, u.settings, opts); err != nil {
			return err
		}
	}

	return nil
}

// inputParents reads, for each input of tx, whether its parent is a coinbase and the lowest height
// it was mined at.
func (u *Server) inputParents(ctx context.Context, tx *bt.Tx) ([]validator.InputParent, error) {
	read := make(map[chainhash.Hash]validator.InputParent, len(tx.Inputs))
	parents := make([]validator.InputParent, len(tx.Inputs))

	for i, input := range tx.Inputs {
		hash := *input.PreviousTxIDChainHash()

		parent, ok := read[hash]
		if !ok {
			data, err := u.utxoStore.Get(ctx, &hash, fields.IsCoinbase, fields.BlockHeights)
			if err != nil {
				return nil, errors.NewProcessingError("[inputParents][%s] failed to read parent %s", tx.TxIDChainHash().String(), hash.String(), err)
			}

			parent.IsCoinbase = data.IsCoinbase

			if len(data.BlockHeights) > 0 {
				parent.Height = data.BlockHeights[0]
				for _, h := range data.BlockHeights[1:] {
					parent.Height = min(parent.Height, h)
				}
			} else if parent.IsCoinbase {
				return nil, errors.NewProcessingError("[inputParents][%s] coinbase parent %s has no block height", tx.TxIDChainHash().String(), hash.String())
			}

			read[hash] = parent
		}

		parents[i] = parent
	}

	return parents, nil
}
