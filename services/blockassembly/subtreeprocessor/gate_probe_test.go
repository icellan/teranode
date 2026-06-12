package subtreeprocessor

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
)

// BenchmarkGateMarkPassPrototype measures the cost of a mark-only pass:
// probe each block hash against the EXISTING pool map (currentTxMap) and
// stamp the hits — the work that an "inverted diff" design would substitute
// for CreateTransactionMap's block map build. Compare against the
// mapbuild-ms metric of BenchmarkForeignBlockMove at the same scale.
//
// The prototype uses Get+Set (two probes) where a real mark would hold the
// bucket lock once and update in place (~1 probe), so it bounds the mark
// cost from above.
//
// Verdict (recorded 2026-06, M3 Max 16 cores, after the bucketInserter +
// Freeze contention fixes): the inversion does NOT pay.
//   - mark pass 8M: ~50ms vs full map build 99ms — but the build figure
//     includes the file read + deserialize that the inversion must also pay
//     (insert-only is ~32ms at this scale), so the phase-1 saving is small;
//   - the remainder scan would probe this pool map (per-probe bucket mutex,
//     pointer values) instead of the frozen lock-free SplitSwissMap
//     (13ns vs 6ns per probe) — a regression on the larger phase;
//   - bucket-count sensitivity: with 16 pool buckets the mark pass is 920ms
//     (9x WORSE than the build) — any design probing the pool map at scale
//     lives or dies by splitMapBuckets.
//
// Kept as the measuring stick for any future attempt at replacing the block
// map build.
func BenchmarkGateMarkPassPrototype(b *testing.B) {
	for _, scale := range []struct {
		name        string
		blockTx     int
		overlapPct  int
		overhangPct int
	}{
		{"block=8M", 8_388_608, 95, 10},
		{"block=16M", 16_777_216, 95, 10},
	} {
		b.Run(scale.name, func(b *testing.B) {
			if testing.Short() {
				b.Skip("skipping gate benchmark in short mode")
			}

			overlap := scale.blockTx * scale.overlapPct / 100
			overhang := scale.blockTx * scale.overhangPct / 100

			blockTxs := genDeterministicHashes(scale.blockTx, 1)

			parent := chainhash.HashH([]byte("gate-parent"))
			pool := NewSplitTxInpointsMap(16 * 1024) // production splitMapBuckets default

			for _, h := range blockTxs[:overlap] {
				pool.Set(h, &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{parent}})
			}

			for _, h := range genDeterministicHashes(overhang, 2) {
				pool.Set(h, &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{parent}})
			}

			// Pre-bucket the block hashes by the pool map's bucket count
			// (the deserializer produces this shape for free).
			nBuckets := pool.Buckets()
			buckets := make([][]chainhash.Hash, nBuckets)
			for _, h := range blockTxs {
				bkt := txmap.Bytes2Uint16Buckets(h, nBuckets)
				buckets[bkt] = append(buckets[bkt], h)
			}

			numWorkers := runtime.GOMAXPROCS(0)

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var found, notFound int64

				var wg sync.WaitGroup
				results := make([]struct{ found, notFound int64 }, numWorkers)

				for w := 0; w < numWorkers; w++ {
					wg.Add(1)

					go func(w int) {
						defer wg.Done()

						var f, nf int64

						for bkt := w; bkt < int(nBuckets); bkt += numWorkers {
							for _, h := range buckets[bkt] {
								if tip, ok := pool.Get(h); ok {
									pool.Set(h, tip) // stand-in for the gen stamp
									f++
								} else {
									nf++
								}
							}
						}

						results[w].found = f
						results[w].notFound = nf
					}(w)
				}

				wg.Wait()

				for _, r := range results {
					found += r.found
					notFound += r.notFound
				}

				if int(found) != overlap || int(notFound) != scale.blockTx-overlap {
					b.Fatalf("found=%d notFound=%d, want %d/%d", found, notFound, overlap, scale.blockTx-overlap)
				}
			}

			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "markpass-ms")
			fmt.Printf("  [gate] %s: mark pass %.1f ms/op over %d block txs\n",
				scale.name, float64(b.Elapsed().Milliseconds())/float64(b.N), scale.blockTx)
		})
	}
}
