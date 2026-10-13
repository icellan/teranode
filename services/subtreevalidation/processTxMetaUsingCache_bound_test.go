package subtreevalidation

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The cache-probe worker limit is shared by every subtree being validated, not
// applied per subtree: on a collapsing node 192 subtrees each ran their own 32
// probe workers, ~6,000 goroutines contending with the Kafka txmeta handler for
// the cache's bucket locks and the CPU.
func TestProcessTxMetaUsingCache_ProbeWorkersBoundedAcrossSubtrees(t *testing.T) {
	const (
		limit    = 2
		subtrees = 16
	)

	cache := setupTestCache(t)
	server := setupCacheTestServer(t, cache, 1024, limit, 0)

	hashes := generateTestHashes(20000)

	baseline := runtime.NumGoroutine()

	var (
		peak    atomic.Int64
		stop    = make(chan struct{})
		sampled = make(chan struct{})
	)

	go func() {
		defer close(sampled)

		for {
			select {
			case <-stop:
				return
			default:
			}

			if n := int64(runtime.NumGoroutine() - baseline); n > peak.Load() {
				peak.Store(n)
			}

			time.Sleep(50 * time.Microsecond)
		}
	}()

	start := make(chan struct{})

	var wg sync.WaitGroup

	for s := 0; s < subtrees; s++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			for round := 0; round < 20; round++ {
				metaSlice := make([]metaSliceItem, len(hashes))
				_, _ = server.processTxMetaUsingCache(context.Background(), hashes, metaSlice, false)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(stop)
	<-sampled

	// subtrees callers + the sampler + limit probe workers, plus up to limit
	// workers that have handed their slot back but not exited yet. Per-subtree
	// limits allow subtrees*limit probe workers on top of the callers (48 here).
	require.LessOrEqual(t, peak.Load(), int64(subtrees+1+2*limit))
}
