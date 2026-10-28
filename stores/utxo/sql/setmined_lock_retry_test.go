package sql

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// A lock error on a SetMinedMulti chunk is retried, not returned. Under SQLite's
// shared cache a lock cycle between connections fails one side with
// SQLITE_LOCKED; when the failed side is SetMinedMulti, setTxMined failed the
// whole block over it. Each chunk is its own transaction and is rolled back on
// failure, so running it again is safe. The write is the real chunk write; the
// first attempts are refused with the error text the engine raised in
// services/blockvalidation's overtaken-tip repair tests.
func TestSetMinedMultiRetriesLockError(t *testing.T) {
	ctx := context.Background()
	db, tx := setup(ctx, t)

	_, err := db.Create(ctx, tx, 0)
	require.NoError(t, err)

	hashes := []*chainhash.Hash{tx.TxIDChainHash()}
	info := utxo.MinedBlockInfo{BlockID: 7, BlockHeight: 1, OnLongestChain: true}
	lockErr := errors.NewStorageError("database table is locked: database is deadlocked (6)")

	t.Run("a refused write succeeds on a later attempt", func(t *testing.T) {
		attempts := 0

		var blockIDs map[chainhash.Hash][]uint32

		err := retryWriteOnLockError(ctx, ulogger.TestLogger{}, len(hashes), func() error {
			attempts++
			if attempts < setMinedMultiRetries {
				return lockErr
			}

			var err error

			blockIDs, err = db.setMinedMultiChunk(ctx, hashes, info)

			return err
		})
		require.NoError(t, err)
		require.Equal(t, setMinedMultiRetries, attempts)
		require.Equal(t, []uint32{7}, blockIDs[*hashes[0]], "the tx is mined in the block once the lock clears")
	})

	t.Run("a lock that never clears returns the lock error", func(t *testing.T) {
		attempts := 0

		err := retryWriteOnLockError(ctx, ulogger.TestLogger{}, len(hashes), func() error {
			attempts++
			return lockErr
		})
		require.ErrorIs(t, err, lockErr)
		require.Equal(t, setMinedMultiRetries, attempts)
	})

	t.Run("any other error is not retried", func(t *testing.T) {
		attempts := 0
		other := errors.NewTxNotFoundError("transaction not found")

		err := retryWriteOnLockError(ctx, ulogger.TestLogger{}, len(hashes), func() error {
			attempts++
			return other
		})
		require.ErrorIs(t, err, other)
		require.Equal(t, 1, attempts)
	})

	t.Run("a cancelled context stops the retries", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		attempts := 0

		err := retryWriteOnLockError(cctx, ulogger.TestLogger{}, len(hashes), func() error {
			attempts++
			cancel()

			return lockErr
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, attempts)
	})
}
