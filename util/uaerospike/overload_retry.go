package uaerospike

import (
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/aerospike-client-go/v8/types"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/retry"
	"github.com/ordishs/gocore"
)

const (
	// defaultOverloadRetryMaxElapsed bounds the total time a single wrapper
	// call may spend retrying overload rejections before the error is
	// returned to the caller.
	defaultOverloadRetryMaxElapsed = 30 * time.Second

	// defaultOverloadRetryBaseBackoff is the first wait between overload
	// retries; it grows exponentially up to defaultOverloadRetryMaxBackoff.
	defaultOverloadRetryBaseBackoff = 50 * time.Millisecond

	// defaultOverloadRetryMaxBackoff caps the exponential backoff growth.
	defaultOverloadRetryMaxBackoff = 5 * time.Second

	// overloadRetryBackoffFactor is the exponential growth factor applied
	// between overload retries.
	overloadRetryBackoffFactor = 2.0
)

// overloadResultCodes are the result codes treated as "server overloaded":
// DEVICE_OVERLOAD is the server rejecting a write because the storage device
// cannot keep up; MAX_ERROR_RATE is the client's own per-node error-rate
// breaker tripping as a consequence of those rejections. Both are safe to
// re-issue: a DEVICE_OVERLOAD write was rejected before being applied and a
// MAX_ERROR_RATE call was never sent. TIMEOUT is deliberately excluded —
// timed-out writes may have been applied (in-doubt).
var overloadResultCodes = []types.ResultCode{types.DEVICE_OVERLOAD, types.MAX_ERROR_RATE}

// overloadRetryConfig holds the bounded-backoff parameters for overload
// retries. maxElapsed <= 0 disables the retry layer entirely.
type overloadRetryConfig struct {
	maxElapsed  time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
}

func (cfg overloadRetryConfig) enabled() bool {
	return cfg.maxElapsed > 0
}

// WithOverloadRetry configures the bounded retry the Client performs when the
// Aerospike server reports overload (DEVICE_OVERLOAD) or the client's local
// error-rate breaker rejects calls (MAX_ERROR_RATE).
//
//	maxElapsed   total retry budget per wrapper call; <= 0 disables the
//	             retry layer (errors propagate immediately, the prior
//	             behaviour).
//	baseBackoff  first wait between attempts; <= 0 falls back to the default.
//	maxBackoff   cap on the exponentially growing wait; raised to baseBackoff
//	             when smaller.
func WithOverloadRetry(maxElapsed, baseBackoff, maxBackoff time.Duration) ClientOption {
	return func(c *clientConfig) {
		if baseBackoff <= 0 {
			baseBackoff = defaultOverloadRetryBaseBackoff
		}

		if maxBackoff < baseBackoff {
			maxBackoff = baseBackoff
		}

		c.overloadRetry = overloadRetryConfig{
			maxElapsed:  maxElapsed,
			baseBackoff: baseBackoff,
			maxBackoff:  maxBackoff,
		}
	}
}

// WithLogger sets the logger used to report overload retries. When unset the
// retry layer is silent (stats are still recorded).
func WithLogger(logger ulogger.Logger) ClientOption {
	return func(c *clientConfig) {
		c.logger = logger
	}
}

// isOverloadError reports whether err (or any error wrapped inside it) is one
// of the overload result codes. nil-safe.
func isOverloadError(err aerospike.Error) bool {
	return err != nil && err.Matches(overloadResultCodes...)
}

// retryOnOverload runs do, retrying with capped exponential backoff while it
// keeps failing with an overload result code, until the configured maxElapsed
// budget is spent. Any other outcome — success or a non-overload error — is
// returned immediately. The connection-semaphore permit is held by the caller
// for the whole loop on purpose: an overloaded server should see fewer new
// operations, not more.
func (c *Client) retryOnOverload(do func() aerospike.Error) aerospike.Error {
	err := do()
	if err == nil || !isOverloadError(err) || !c.overloadRetry.enabled() {
		return err
	}

	deadline := time.Now().Add(c.overloadRetry.maxElapsed)
	backoff := c.overloadRetry.baseBackoff

	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			c.logOverloadGiveUp(attempt, err)
			return err
		}

		c.logOverloadRetry(attempt, backoff, err)

		start := gocore.CurrentTime()

		time.Sleep(backoff)

		backoff = retry.CappedExponentialBackoff(backoff, overloadRetryBackoffFactor, c.overloadRetry.maxBackoff)

		err = do()

		c.stats.overloadRetryStat.AddTime(start)

		if err == nil || !isOverloadError(err) {
			return err
		}
	}
}

// retryBatchOnOverload runs do over records and, while individual records (or
// the whole call) keep failing with overload result codes, resubmits only the
// still-overloaded records with capped exponential backoff until the
// configured maxElapsed budget is spent.
//
// Contract: only overload failures are converted into successes. Non-overload
// per-record errors (KEY_NOT_FOUND_ERROR etc.) are never resubmitted and stay
// on their records exactly as the underlying client set them; non-overload
// top-level errors are returned unchanged.
func (c *Client) retryBatchOnOverload(records []aerospike.BatchRecordIfc, do func([]aerospike.BatchRecordIfc) aerospike.Error) aerospike.Error {
	err := do(records)
	if !c.overloadRetry.enabled() {
		return err
	}

	// Only overload (or BATCH_FAILED, which may be carrying per-record
	// overload failures) outcomes are this layer's concern.
	if err != nil && !isOverloadError(err) && !err.Matches(types.BATCH_FAILED) {
		return err
	}

	failed := overloadedRecords(records, isOverloadError(err))
	if len(failed) == 0 {
		return err
	}

	deadline := time.Now().Add(c.overloadRetry.maxElapsed)
	backoff := c.overloadRetry.baseBackoff

	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			c.logOverloadGiveUp(attempt, err)
			return batchOverloadError(err, failed)
		}

		c.logOverloadRetry(attempt, backoff, err)

		start := gocore.CurrentTime()

		time.Sleep(backoff)

		backoff = retry.CappedExponentialBackoff(backoff, overloadRetryBackoffFactor, c.overloadRetry.maxBackoff)

		err = do(failed)

		c.stats.overloadRetryStat.AddTime(start)

		if err != nil && !isOverloadError(err) && !err.Matches(types.BATCH_FAILED) {
			return err
		}

		failed = overloadedRecords(failed, isOverloadError(err))
		if len(failed) == 0 {
			// No overloaded records remain. A residual BATCH_FAILED from the
			// last attempt means a non-overload record failure surfaced
			// inside the retried subset — return it for the caller to handle.
			if err != nil && !isOverloadError(err) {
				return err
			}

			return nil
		}
	}
}

// overloadedRecords returns the subset of records that must be resubmitted.
// includeUnfinished covers client-side rejections (e.g. the MAX_ERROR_RATE
// breaker) where the call failed as a whole and unprocessed records still
// carry their initial NO_RESPONSE result code.
func overloadedRecords(records []aerospike.BatchRecordIfc, includeUnfinished bool) []aerospike.BatchRecordIfc {
	var failed []aerospike.BatchRecordIfc

	for _, rec := range records {
		switch rec.BatchRec().ResultCode {
		case types.DEVICE_OVERLOAD, types.MAX_ERROR_RATE:
			failed = append(failed, rec)
		case types.NO_RESPONSE:
			if includeUnfinished {
				failed = append(failed, rec)
			}
		}
	}

	return failed
}

// batchOverloadError picks the error to return when the retry budget is
// exhausted: the last overload-matching top-level error, else the first
// still-failed record's error, so the result always Matches an overload code
// and downstream classification (e.g. the spend circuit breaker) keeps
// working.
func batchOverloadError(lastErr aerospike.Error, failed []aerospike.BatchRecordIfc) aerospike.Error {
	if isOverloadError(lastErr) {
		return lastErr
	}

	for _, rec := range failed {
		if recErr := rec.BatchRec().Err; recErr != nil {
			return recErr
		}
	}

	if lastErr != nil {
		return lastErr
	}

	return &aerospike.AerospikeError{ResultCode: types.DEVICE_OVERLOAD}
}

func (c *Client) logOverloadRetry(attempt int, wait time.Duration, err aerospike.Error) {
	if c.logger == nil {
		return
	}

	// DEBUG for the first attempts to keep brief overload blips quiet, WARN
	// once the condition persists — same convention as util/retry.
	if attempt < 5 {
		c.logger.Debugf("aerospike overloaded (attempt %d): %v, retrying in %.3fs", attempt, err, wait.Seconds())
	} else {
		c.logger.Warnf("aerospike overloaded (attempt %d): %v, retrying in %.3fs", attempt, err, wait.Seconds())
	}
}

func (c *Client) logOverloadGiveUp(attempt int, err aerospike.Error) {
	if c.logger == nil {
		return
	}

	c.logger.Errorf("aerospike still overloaded after %v (attempt %d), giving up: %v", c.overloadRetry.maxElapsed, attempt, err)
}
