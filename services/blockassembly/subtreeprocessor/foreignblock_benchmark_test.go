package subtreeprocessor

import (
	"fmt"
	"testing"
)

// BenchmarkForeignBlockMove drives the two foreign-block moveForwardBlock
// phases (block map build + remainder rebuild) end-to-end at parameterized
// scale. ns/op includes per-iteration setup (subtree generation + blob
// store); compare the reported phase metrics, not ns/op:
//
//	mapbuild-ms    — CreateTransactionMap wall time
//	remainder-ms   — processRemainderTransactionsAndDequeue wall time
//	alloc-mb       — TotalAlloc delta across both phases
func BenchmarkForeignBlockMove(b *testing.B) {
	cases := []struct {
		name  string
		cfg   ForeignBlockBenchConfig
		large bool
	}{
		{
			name: "block=500k/overlap=95/overhang=10",
			cfg: ForeignBlockBenchConfig{
				NumBlockSubtrees: 128,
				TxsPerSubtree:    4096,
				OverlapPct:       95,
				PoolOverhangPct:  10,
				Seed:             42,
			},
		},
		{
			name: "block=8M/overlap=95/overhang=10",
			cfg: ForeignBlockBenchConfig{
				NumBlockSubtrees: 128,
				TxsPerSubtree:    65536,
				OverlapPct:       95,
				PoolOverhangPct:  10,
				Seed:             42,
			},
			large: true,
		},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			if c.large && testing.Short() {
				b.Skip("skipping large foreign-block benchmark in short mode")
			}

			var mapBuildMS, remainderMS, allocMB float64

			for i := 0; i < b.N; i++ {
				result, err := RunForeignBlockMoveBenchmark(c.cfg, "", "")
				if err != nil {
					b.Fatal(err)
				}

				if result.BenchErr != nil {
					b.Fatal(result.BenchErr)
				}

				// +1: the coinbase placeholder of block subtree 0 is
				// deserialized into the map like any other leaf.
				if result.MapLength != result.BlockTxCount+1 {
					b.Fatalf("map length %d, want %d", result.MapLength, result.BlockTxCount+1)
				}

				// Remainder = pool txs not in the block (the overhang) plus the
				// coinbase placeholders of the rebuilt current subtree chain.
				wantRemainder := result.PoolTxCount - (result.BlockTxCount * c.cfg.OverlapPct / 100)
				if result.RemainderCount < wantRemainder {
					b.Fatalf("remainder count %d, want at least %d", result.RemainderCount, wantRemainder)
				}

				mapBuildMS += float64(result.MapBuildElapsed.Milliseconds())
				remainderMS += float64(result.RemainderElapsed.Milliseconds())
				allocMB += float64(result.AllocDeltaMB)
			}

			n := float64(b.N)
			b.ReportMetric(mapBuildMS/n, "mapbuild-ms")
			b.ReportMetric(remainderMS/n, "remainder-ms")
			b.ReportMetric(allocMB/n, "alloc-mb")
		})
	}
}

// TestRunForeignBlockMoveBenchmarkSmoke pins the benchmark harness itself:
// tiny scale, asserts the phases run and the remainder matches the overhang.
func TestRunForeignBlockMoveBenchmarkSmoke(t *testing.T) {
	cfg := ForeignBlockBenchConfig{
		NumBlockSubtrees: 4,
		TxsPerSubtree:    1024,
		OverlapPct:       95,
		PoolOverhangPct:  10,
		Seed:             7,
	}

	result, err := RunForeignBlockMoveBenchmark(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if result.BenchErr != nil {
		t.Fatal(result.BenchErr)
	}

	// +1: the coinbase placeholder of block subtree 0 is deserialized into
	// the map like any other leaf.
	if result.MapLength != result.BlockTxCount+1 {
		t.Fatalf("map length %d, want %d", result.MapLength, result.BlockTxCount+1)
	}

	fmt.Printf("smoke: block=%d pool=%d mapbuild=%s remainder=%s remainderCount=%d alloc=%dMB\n",
		result.BlockTxCount, result.PoolTxCount, result.MapBuildElapsed, result.RemainderElapsed,
		result.RemainderCount, result.AllocDeltaMB)
}
