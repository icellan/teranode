package utxo

import (
	"context"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// unspendRetryBackoffBase is the base delay for the exponential backoff between
// rollback attempts. A variable so tests can shorten it.
var unspendRetryBackoffBase = time.Second

// sequentialStore is the subset of a concrete store's methods needed by
// SequentialSpendAndCreate.
type sequentialStore interface {
	Create(ctx context.Context, tx *bt.Tx, blockHeight uint32, opts ...CreateOption) (*meta.Data, error)
	Spend(ctx context.Context, tx *bt.Tx, blockHeight uint32, ignoreFlags ...IgnoreFlags) ([]*Spend, error)
	Unspend(ctx context.Context, spends []*Spend, flagAsLocked ...bool) error
}

// SequentialSpendAndCreate implements the Store.SpendAndCreate contract as the
// original two-call sequence: spend the inputs, create the outputs, and roll the
// spends back (with retries) when the create fails with anything other than
// ErrTxExists. Concrete stores delegate to it until they implement SpendAndCreate
// atomically (Postgres transactions, Aerospike-specific logic).
func SequentialSpendAndCreate(ctx context.Context, logger ulogger.Logger, s sequentialStore,
	tx *bt.Tx, blockHeight uint32, opts ...CreateOption) (*meta.Data, []*Spend, error) {
	options := &CreateOptions{}
	for _, opt := range opts {
		opt(options)
	}

	if options.CreateOnly && options.SpendOnly {
		return nil, nil, errors.NewInvalidArgumentError("SpendAndCreate: WithCreateOnly and WithSpendOnly are mutually exclusive")
	}

	var spends []*Spend

	if !options.CreateOnly {
		var err error

		spends, err = s.Spend(ctx, tx, blockHeight, options.IgnoreFlags)
		if err != nil {
			return nil, spends, err
		}

		if options.SpendOnly {
			return nil, spends, nil
		}
	}

	md, err := s.Create(ctx, tx, blockHeight, opts...)
	if err != nil {
		if errors.Is(err, errors.ErrTxExists) {
			// the tx already exists; leave the spends in place for the caller to decide
			return nil, spends, err
		}

		if rollbackErr := unspendWithRetry(ctx, logger, s, spends); rollbackErr != nil {
			return nil, spends, errors.NewProcessingError("SpendAndCreate: error reversing utxo spends: %v", rollbackErr, err)
		}

		return nil, spends, err
	}

	return md, spends, nil
}

// unspendWithRetry reverses spends with up to 3 attempts and exponential backoff,
// ported from the validator's reverseSpends.
func unspendWithRetry(ctx context.Context, logger ulogger.Logger, s sequentialStore, spends []*Spend) error {
	for retries := uint(0); retries < 3; retries++ {
		if errReset := s.Unspend(ctx, spends); errReset != nil {
			if retries < 2 {
				backoff := time.Duration(1<<retries) * unspendRetryBackoffBase
				logger.Errorf("error resetting utxos, retrying in %s: %v", backoff.String(), errReset)
				time.Sleep(backoff)
			} else {
				return errors.NewProcessingError("error resetting utxos", errReset)
			}
		} else {
			break
		}
	}

	return nil
}
