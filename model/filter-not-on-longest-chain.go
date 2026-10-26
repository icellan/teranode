package model

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"golang.org/x/sync/errgroup"
)

// txBlockIDsGetter is the slice of the UTXO store FilterTxsNotOnLongestChain reads from.
type txBlockIDsGetter interface {
	Get(ctx context.Context, hash *chainhash.Hash, fields ...fields.FieldName) (*meta.Data, error)
}

// longestChainClient is the slice of the blockchain client FilterTxsNotOnLongestChain needs.
type longestChainClient interface {
	GetBlockHeaders(ctx context.Context, blockHash *chainhash.Hash, numberOfHeaders uint64) ([]*BlockHeader, []*BlockHeaderMeta, error)
	CheckBlockIsInCurrentChain(ctx context.Context, blockIDs []uint32) (bool, error)
}

// FilterTxsNotOnLongestChain returns the subset of hashes that are not mined in any block on
// the current longest chain, i.e. the txs it is correct to mark not-on-longest-chain.
//
// It exists for blocks that left the longest chain after their txs were written as mined:
// a tx the block shares with the longest chain is mined there and must keep its cleared
// unmined_since, so only the rest may be re-marked.
//
//   - offChainBlockID is a block known to be off the longest chain. A tx whose only block it
//     is gets returned without a chain lookup.
//   - anchorHash is a block on the longest chain. The 1000 headers ending at it form a fast
//     path: a tx in any of those blocks is on the longest chain. Older blocks are resolved
//     with CheckBlockIsInCurrentChain.
//
// Every hash must exist in the UTXO store. A missing one is an error, not a skip.
func FilterTxsNotOnLongestChain(ctx context.Context, logger ulogger.Logger, utxoStore txBlockIDsGetter, blockchainClient longestChainClient,
	anchorHash *chainhash.Hash, offChainBlockID uint32, hashes []chainhash.Hash, concurrency int) ([]chainhash.Hash, error) {
	notOnLongestChain := make([]chainhash.Hash, 0, len(hashes))

	_, lastBlockHeaderMetas, err := blockchainClient.GetBlockHeaders(ctx, anchorHash, 1000)
	if err != nil {
		return nil, errors.NewProcessingError("[FilterTxsNotOnLongestChain] error getting last block headers", err)
	}

	lastBlockHeaderIDs := make(map[uint32]bool)
	for _, header := range lastBlockHeaderMetas {
		lastBlockHeaderIDs[header.ID] = true
	}

	txMetas := make([]*meta.Data, len(hashes))
	g, gCtx := errgroup.WithContext(ctx)
	// SafeSetLimit panics loudly on a zero limit rather than letting g.Go deadlock on a
	// zero-capacity semaphore.
	util.SafeSetLimit(logger, g, concurrency)

	for idx, hash := range hashes {
		h := hash // per-iteration copy for pointer safety
		g.Go(func() error {
			txMeta, err := utxoStore.Get(gCtx, &h, fields.BlockIDs)
			if err != nil {
				return errors.NewProcessingError("[FilterTxsNotOnLongestChain] error getting transaction from utxo store", err)
			}

			txMetas[idx] = txMeta

			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return nil, err
	}

	for idx, hash := range hashes {
		txMeta := txMetas[idx]
		if txMeta == nil {
			// transaction not found, not OK
			return nil, errors.NewProcessingError("[FilterTxsNotOnLongestChain] transaction %s not found on longest chain", hash.String())
		}

		if len(txMeta.BlockIDs) == 1 && txMeta.BlockIDs[0] == offChainBlockID {
			// the transaction is only in the off-chain block, so it is definitely not on the longest chain
			notOnLongestChain = append(notOnLongestChain, hash)
			continue
		}

		foundInRecentBlock := false
		for _, blockID := range txMeta.BlockIDs {
			if lastBlockHeaderIDs[blockID] {
				// the transaction is still in one of the last 1000 blocks, so it is still on the longest chain
				foundInRecentBlock = true
				break
			}
		}

		if foundInRecentBlock {
			continue
		}

		// check BlockIDs to see if the transaction is still on the longest chain
		onLongestChain, err := blockchainClient.CheckBlockIsInCurrentChain(ctx, txMeta.BlockIDs)
		if err != nil {
			return nil, errors.NewProcessingError("[FilterTxsNotOnLongestChain] error checking if transaction is on longest chain", err)
		}

		if !onLongestChain {
			notOnLongestChain = append(notOnLongestChain, hash)
		}
	}

	return notOnLongestChain, nil
}
