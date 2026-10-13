// Package subtreevalidation provides functionality for validating subtrees in a blockchain context.
// It handles the validation of transaction subtrees, manages transaction metadata caching,
// and interfaces with blockchain and validation services.
package subtreevalidation

import (
	"context"
	"sync/atomic"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/txmetacache"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"golang.org/x/sync/errgroup"
)

// processTxMetaUsingCache attempts to retrieve transaction metadata from the cache for a batch
// of transactions.
//
// The lookups are grouped by cache bucket range (txmetacache.ProbeRangeOf) and each range is
// probed with one GetMetaCachedMulti call, so every bucket's read lock is taken once per range
// rather than once per transaction. Range tasks run on probe workers whose number is limited
// across ALL concurrent calls (subtreevalidation_processTxMetaUsingCache_Concurrency): when
// hundreds of subtrees arrive at once they share the workers instead of each starting its own,
// which kept thousands of goroutines contending with the Kafka txmeta handler for the cache's
// bucket locks and the CPU.
//
// Parameters:
//   - ctx: Context for cancellation and tracing
//   - txHashes: Slice of transaction hashes to process
//   - txMetaSlice: Pre-allocated slice to store retrieved metadata
//   - failFast: If true, fails quickly when missing transaction threshold is exceeded
//
// Returns:
//   - int: Number of transactions missing from cache
//   - error: Any error encountered during processing
//
// The function will return a ThresholdExceededError if failFast is true and
// the number of missing transactions exceeds the configured threshold.
func (u *Server) processTxMetaUsingCache(ctx context.Context, txHashes []chainhash.Hash, txMetaSlice []metaSliceItem, failFast bool) (int, error) {
	if len(txHashes) != len(txMetaSlice) {
		return 0, errors.NewProcessingError("txHashes and txMetaSlice must be the same length")
	}

	ctx, _, deferFn := tracing.Tracer("subtreevalidation").Start(ctx, "processTxMetaUsingCache")
	defer deferFn()

	cache, ok := u.utxoStore.(*txmetacache.TxMetaCache)
	if !ok {
		u.logger.Warnf("[processTxMetaUsingCache] txMetaStore is not a cached implementation")
		return len(txHashes), nil // As there was no cache, we "missed" all the txHashes
	}

	missingTxThreshold := uint32(u.settings.SubtreeValidation.ProcessTxMetaUsingCacheMissingTxThreshold) // nolint: gosec // G115: configured threshold is non-negative

	// Group the indices still to look up by cache bucket range (counting sort).
	const skip = -1

	rangeOf := make([]int, len(txHashes))

	var counts [txmetacache.ProbeRanges]int

	for i := range txHashes {
		if txMetaSlice[i].isSet || txHashes[i].Equal(*subtree.CoinbasePlaceholderHash) {
			rangeOf[i] = skip
			continue
		}

		r := txmetacache.ProbeRangeOf(&txHashes[i])
		rangeOf[i] = r
		counts[r]++
	}

	var next [txmetacache.ProbeRanges]int

	offset := 0
	for r, n := range counts {
		next[r] = offset
		offset += n
	}

	order := make([]int, offset)

	for i, r := range rangeOf {
		if r == skip {
			continue
		}

		order[next[r]] = i
		next[r]++
	}

	var missed atomic.Uint32

	g, gCtx := errgroup.WithContext(ctx)
	slots := u.cacheProbeSlots()

dispatch:
	for r, n := range counts {
		if n == 0 {
			continue
		}

		indices := order[next[r]-n : next[r]]

		// Take a shared probe slot before starting the goroutine, so the number
		// of probe goroutines is bounded across every concurrent call.
		select {
		case slots <- struct{}{}:
		case <-gCtx.Done():
			break dispatch
		}

		g.Go(func() error {
			defer func() { <-slots }()

			return probeCacheRange(gCtx, cache, txHashes, txMetaSlice, indices, &missed, failFast, missingTxThreshold)
		})
	}

	err := g.Wait()
	if err == nil && ctx.Err() != nil {
		err = errors.NewContextCanceledError("[processTxMetaUsingCache context cancelled]", ctx.Err())
	}

	if err != nil {
		return int(missed.Load()), errors.NewProcessingError("error processing txMeta using cache", err)
	}

	return int(missed.Load()), nil
}

// probeCacheRange looks up the given indices, which all fall into one cache
// bucket range, with a single batched cache read and fills txMetaSlice.
func probeCacheRange(ctx context.Context, cache *txmetacache.TxMetaCache, txHashes []chainhash.Hash, txMetaSlice []metaSliceItem,
	indices []int, missed *atomic.Uint32, failFast bool, missingTxThreshold uint32) error {
	if ctx.Err() != nil {
		return errors.NewContextCanceledError("[processTxMetaUsingCache context cancelled]", ctx.Err())
	}

	hashes := make([]chainhash.Hash, len(indices))
	for j, i := range indices {
		hashes[j] = txHashes[i]
	}

	found := make([]*meta.Data, len(indices))
	cache.GetMetaCachedMulti(ctx, hashes, found)

	for j, i := range indices {
		txMeta := found[j]

		// Defensively reject a partial cache entry: a non-coinbase tx with no
		// parent tx hashes is incomplete (TxInpoints was never populated) and
		// would later fail subtree-meta serialization and wedge the block. Treat
		// it as a miss so the store fallback — which can reconstruct inpoints from
		// the tx inputs — resolves it and self-heals the cache.
		if txMeta != nil && (txMeta.IsCoinbase || len(txMeta.TxInpoints.ParentTxHashes) > 0) {
			txMetaSlice[i] = metaSliceItem{
				fee:         txMeta.Fee,
				sizeInBytes: txMeta.SizeInBytes,
				coinbase:    txMeta.IsCoinbase,
				conflicting: txMeta.Conflicting,
				creating:    txMeta.Creating,
				isSet:       true,
				txInpoints:  txMeta.TxInpoints,
			}

			continue
		}

		newMissed := missed.Add(1)

		if failFast && missingTxThreshold > 0 && newMissed > missingTxThreshold {
			return errors.NewThresholdExceededError("threshold exceeded for missing txs: %d > %d", newMissed, missingTxThreshold)
		}
	}

	return nil
}

// cacheProbeSlots returns the server-wide pool of cache-probe worker slots,
// sized by subtreevalidation_processTxMetaUsingCache_Concurrency. Created on
// first use so struct-literal Servers (tests) get it too.
func (u *Server) cacheProbeSlots() chan struct{} {
	u.cacheProbeSlotsOnce.Do(func() {
		u.cacheProbeSlotsCh = make(chan struct{}, max(1, u.settings.SubtreeValidation.ProcessTxMetaUsingCacheConcurrency))
	})

	return u.cacheProbeSlotsCh
}
