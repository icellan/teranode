package uaerospike

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/aerospike-client-go/v8/types"
	"github.com/stretchr/testify/require"
)

func newOverloadTestClient(maxElapsed, baseBackoff, maxBackoff time.Duration) *Client {
	return &Client{
		stats: NewClientStats(),
		overloadRetry: overloadRetryConfig{
			maxElapsed:  maxElapsed,
			baseBackoff: baseBackoff,
			maxBackoff:  maxBackoff,
		},
	}
}

func overloadErr(code types.ResultCode) aerospike.Error {
	return &aerospike.AerospikeError{ResultCode: code}
}

func newTestBatchRecords(t *testing.T, n int) []aerospike.BatchRecordIfc {
	t.Helper()

	records := make([]aerospike.BatchRecordIfc, n)

	for i := 0; i < n; i++ {
		key, err := aerospike.NewKey("test", "test", i)
		require.NoError(t, err)

		records[i] = aerospike.NewBatchWrite(nil, key, aerospike.PutOp(aerospike.NewBin("bin", i)))
	}

	return records
}

func TestIsOverloadError(t *testing.T) {
	t.Run("nil error is not overload", func(t *testing.T) {
		require.False(t, isOverloadError(nil))
	})

	t.Run("DEVICE_OVERLOAD is overload", func(t *testing.T) {
		require.True(t, isOverloadError(overloadErr(types.DEVICE_OVERLOAD)))
	})

	t.Run("MAX_ERROR_RATE is overload", func(t *testing.T) {
		require.True(t, isOverloadError(overloadErr(types.MAX_ERROR_RATE)))
	})

	t.Run("other codes are not overload", func(t *testing.T) {
		require.False(t, isOverloadError(overloadErr(types.KEY_NOT_FOUND_ERROR)))
		require.False(t, isOverloadError(overloadErr(types.TIMEOUT)))
		require.False(t, isOverloadError(overloadErr(types.KEY_BUSY)))
	})
}

func TestOverloadRetryDefaults(t *testing.T) {
	t.Run("defaults applied when no option given", func(t *testing.T) {
		cfg := newClientConfig(nil)
		require.Equal(t, defaultOverloadRetryMaxElapsed, cfg.overloadRetry.maxElapsed)
		require.Equal(t, defaultOverloadRetryBaseBackoff, cfg.overloadRetry.baseBackoff)
		require.Equal(t, defaultOverloadRetryMaxBackoff, cfg.overloadRetry.maxBackoff)
	})

	t.Run("WithOverloadRetry overrides defaults", func(t *testing.T) {
		cfg := newClientConfig([]ClientOption{WithOverloadRetry(time.Second, time.Millisecond, 10*time.Millisecond)})
		require.Equal(t, time.Second, cfg.overloadRetry.maxElapsed)
		require.Equal(t, time.Millisecond, cfg.overloadRetry.baseBackoff)
		require.Equal(t, 10*time.Millisecond, cfg.overloadRetry.maxBackoff)
	})
}

func TestRetryOnOverload(t *testing.T) {
	t.Run("success first try calls once", func(t *testing.T) {
		c := newOverloadTestClient(50*time.Millisecond, time.Millisecond, 4*time.Millisecond)

		calls := 0
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			return nil
		})

		require.Nil(t, err)
		require.Equal(t, 1, calls)
	})

	t.Run("overload twice then success", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)

		calls := 0
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			if calls <= 2 {
				return overloadErr(types.DEVICE_OVERLOAD)
			}
			return nil
		})

		require.Nil(t, err)
		require.Equal(t, 3, calls)
	})

	t.Run("non-overload error returned immediately", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)

		calls := 0
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			return overloadErr(types.KEY_NOT_FOUND_ERROR)
		})

		require.NotNil(t, err)
		require.True(t, err.Matches(types.KEY_NOT_FOUND_ERROR))
		require.Equal(t, 1, calls)
	})

	t.Run("overload turning into non-overload error stops retrying", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)

		calls := 0
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			if calls == 1 {
				return overloadErr(types.MAX_ERROR_RATE)
			}
			return overloadErr(types.PARAMETER_ERROR)
		})

		require.NotNil(t, err)
		require.True(t, err.Matches(types.PARAMETER_ERROR))
		require.Equal(t, 2, calls)
	})

	t.Run("permanent overload returns overload error after budget", func(t *testing.T) {
		c := newOverloadTestClient(20*time.Millisecond, time.Millisecond, 4*time.Millisecond)

		calls := 0
		start := time.Now()
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			return overloadErr(types.DEVICE_OVERLOAD)
		})
		elapsed := time.Since(start)

		require.NotNil(t, err)
		require.True(t, err.Matches(types.DEVICE_OVERLOAD))
		require.Greater(t, calls, 1, "should have retried at least once")
		require.GreaterOrEqual(t, elapsed, 20*time.Millisecond, "should have kept retrying until the budget elapsed")
	})

	t.Run("maxElapsed zero disables retry", func(t *testing.T) {
		c := newOverloadTestClient(0, time.Millisecond, 4*time.Millisecond)

		calls := 0
		err := c.retryOnOverload(func() aerospike.Error {
			calls++
			return overloadErr(types.DEVICE_OVERLOAD)
		})

		require.NotNil(t, err)
		require.True(t, err.Matches(types.DEVICE_OVERLOAD))
		require.Equal(t, 1, calls)
	})
}

func TestRetryBatchOnOverload(t *testing.T) {
	setResult := func(rec aerospike.BatchRecordIfc, code types.ResultCode) {
		br := rec.BatchRec()
		br.ResultCode = code

		if code == types.OK {
			br.Err = nil
		} else {
			br.Err = overloadErr(code)
		}
	}

	t.Run("all OK first try calls once", func(t *testing.T) {
		c := newOverloadTestClient(50*time.Millisecond, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 3)

		calls := 0
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			calls++
			for _, rec := range recs {
				setResult(rec, types.OK)
			}
			return nil
		})

		require.Nil(t, err)
		require.Equal(t, 1, calls)
	})

	t.Run("overloaded subset retried until success", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 5)

		var callSizes []int
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			callSizes = append(callSizes, len(recs))

			for i, rec := range recs {
				if len(callSizes) == 1 && (i == 0 || i == 2) {
					setResult(rec, types.DEVICE_OVERLOAD)
				} else {
					setResult(rec, types.OK)
				}
			}
			return nil
		})

		require.Nil(t, err)
		require.Equal(t, []int{5, 2}, callSizes, "second call must resubmit exactly the overloaded records")

		for _, rec := range records {
			require.Equal(t, types.OK, rec.BatchRec().ResultCode)
		}
	})

	t.Run("non-overload per-record errors are preserved and not resubmitted", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 3)

		var calls [][]aerospike.BatchRecordIfc
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			calls = append(calls, recs)

			for _, rec := range recs {
				if len(calls) == 1 {
					switch rec {
					case records[0]:
						setResult(rec, types.MAX_ERROR_RATE)
					case records[1]:
						setResult(rec, types.KEY_NOT_FOUND_ERROR)
					default:
						setResult(rec, types.OK)
					}
				} else {
					setResult(rec, types.OK)
				}
			}
			return nil
		})

		require.Nil(t, err)
		require.Len(t, calls, 2)
		require.Len(t, calls[1], 1)
		require.Same(t, records[0], calls[1][0])

		require.Equal(t, types.OK, records[0].BatchRec().ResultCode)
		require.Equal(t, types.KEY_NOT_FOUND_ERROR, records[1].BatchRec().ResultCode)
		require.NotNil(t, records[1].BatchRec().Err)
		require.Equal(t, types.OK, records[2].BatchRec().ResultCode)
	})

	t.Run("non-overload top-level error returned unchanged without retry", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 2)

		calls := 0
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			calls++
			return overloadErr(types.NETWORK_ERROR)
		})

		require.NotNil(t, err)
		require.True(t, err.Matches(types.NETWORK_ERROR))
		require.Equal(t, 1, calls)
	})

	t.Run("top-level overload error retries unfinished records", func(t *testing.T) {
		c := newOverloadTestClient(time.Second, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 4)

		var callSizes []int
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			callSizes = append(callSizes, len(recs))

			if len(callSizes) == 1 {
				// Client-side rejection (e.g. MAX_ERROR_RATE breaker): records keep
				// their initial NO_RESPONSE result code.
				return overloadErr(types.MAX_ERROR_RATE)
			}

			for _, rec := range recs {
				setResult(rec, types.OK)
			}
			return nil
		})

		require.Nil(t, err)
		require.Equal(t, []int{4, 4}, callSizes, "all unfinished records must be resubmitted")
	})

	t.Run("exhaustion returns overload error", func(t *testing.T) {
		c := newOverloadTestClient(20*time.Millisecond, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 2)

		calls := 0
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			calls++
			for _, rec := range recs {
				setResult(rec, types.DEVICE_OVERLOAD)
			}
			return nil
		})

		require.NotNil(t, err)
		require.True(t, err.Matches(types.DEVICE_OVERLOAD))
		require.Greater(t, calls, 1, "should have retried at least once")
	})

	t.Run("maxElapsed zero disables batch retry", func(t *testing.T) {
		c := newOverloadTestClient(0, time.Millisecond, 4*time.Millisecond)
		records := newTestBatchRecords(t, 2)

		calls := 0
		err := c.retryBatchOnOverload(records, func(recs []aerospike.BatchRecordIfc) aerospike.Error {
			calls++
			for _, rec := range recs {
				setResult(rec, types.DEVICE_OVERLOAD)
			}
			return nil
		})

		require.Nil(t, err, "disabled retry must return the original outcome unchanged")
		require.Equal(t, 1, calls)
	})
}
