package subtreeprocessor

import (
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/txmetacache"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
)

// txMetaEvictBatchSize caps the DELETE entries per Kafka message. An entry is 37
// bytes on the v1 wire, so 10,000 entries is 370 KB, comfortably under Kafka's
// default 1 MiB message limit even for a very large conflict cascade.
const txMetaEvictBatchSize = 10_000

// TxMetaEvictor tells subtree validation to drop txmeta cache entries for
// transactions that block assembly has just demoted to conflicting.
//
// Subtree validation caches non-conflicting txmeta fed by the validator's ADD
// messages on the txmeta topic and never learns about a later demotion on its
// own: block assembly's UTXO store is not cache-wrapped, so the store's own
// cache eviction never reaches the subtree validation process. Without a DELETE
// the stale non-conflicting entry would let a block containing the demoted tx
// pass the cached-meta fast path. DELETE is a hint to a cache that is repopulated
// from the store on a miss, so publishing is best effort and never fails the caller.
//
// A nil *TxMetaEvictor is valid and does nothing, so call sites need no guard.
type TxMetaEvictor struct {
	logger   ulogger.Logger
	producer kafka.KafkaAsyncProducerI
}

// NewTxMetaEvictor returns an evictor publishing to the given txmeta producer,
// or nil when there is no producer (Kafka txmeta not configured). The caller owns
// the producer's lifecycle (Start/Stop).
func NewTxMetaEvictor(logger ulogger.Logger, producer kafka.KafkaAsyncProducerI) *TxMetaEvictor {
	// A typed-nil *KafkaAsyncProducer stored in the interface is not == nil.
	if typed, ok := producer.(*kafka.KafkaAsyncProducer); (ok && typed == nil) || producer == nil {
		return nil
	}

	return &TxMetaEvictor{logger: logger, producer: producer}
}

// Evict publishes a DELETE for every hash. Call it only after the store write
// that marked the hashes conflicting has succeeded. It never blocks on a full
// producer buffer and never returns an error; a dropped message is logged at WARN.
func (e *TxMetaEvictor) Evict(hashes []chainhash.Hash) {
	if e == nil || len(hashes) == 0 {
		return
	}

	for start := 0; start < len(hashes); start += txMetaEvictBatchSize {
		batch := hashes[start:min(start+txMetaEvictBatchSize, len(hashes))]

		// Keyed by tx hash like the validator's txmeta messages.
		if !e.producer.TryPublish(&kafka.Message{
			Key:   batch[0][:],
			Value: txmetacache.EncodeDeleteBatch(batch),
		}) {
			e.logger.Warnf("[TxMetaEvictor] dropped txmeta DELETE for %d demoted txs, producer buffer full or stopped; subtree validation may hold stale non-conflicting entries", len(batch))
		}
	}
}
