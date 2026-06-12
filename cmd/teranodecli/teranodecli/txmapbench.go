package teranodecli

import (
	"fmt"

	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
)

func runCreateTxMapBenchmark(numSubtrees, txsPerSubtree int, cpuProfile, memProfile string) error {
	result, err := subtreeprocessor.RunCreateTransactionMapBenchmark(numSubtrees, txsPerSubtree, cpuProfile, memProfile)
	if err != nil {
		return err
	}

	// Print results
	fmt.Printf("\nBenchmark Results\n")
	fmt.Printf("=================\n")
	fmt.Printf("Subtrees:            %d\n", result.NumSubtrees)
	fmt.Printf("Total Transactions:  %d\n", result.TotalTxs)
	fmt.Printf("Elapsed Time:        %.2fs\n", result.Elapsed.Seconds())
	fmt.Printf("Throughput:          %.2f tx/sec\n", result.TxPerSec)
	fmt.Printf("Map Length:          %d\n", result.MapLength)
	fmt.Printf("Conflicting Nodes:   %d\n", result.ConflictingNodes)
	fmt.Println()
	fmt.Printf("Profiles written to:\n")
	fmt.Printf("  CPU:    %s\n", cpuProfile)
	fmt.Printf("  Memory: %s\n", memProfile)
	fmt.Println()
	fmt.Printf("Analyze with:\n")
	fmt.Printf("  go tool pprof -http=:8080 %s\n", cpuProfile)
	fmt.Printf("  go tool pprof -http=:8081 %s\n", memProfile)

	if result.BenchErr != nil {
		fmt.Printf("\nNote: CreateTransactionMap returned error: %v\n", result.BenchErr)
	}

	return nil
}

func runProcessRemainderBenchmark(numChainedSubtrees, txsPerSubtree int, cpuProfile, memProfile string) error {
	result, err := subtreeprocessor.RunProcessRemainderBenchmark(numChainedSubtrees, txsPerSubtree, cpuProfile, memProfile)
	if err != nil {
		return err
	}

	// Print results
	fmt.Printf("\nBenchmark Results\n")
	fmt.Printf("=================\n")
	fmt.Printf("Chained Subtrees:    %d\n", result.NumChainedSubtrees)
	fmt.Printf("Total Transactions:  %d\n", result.TotalTxs)
	fmt.Printf("Elapsed Time:        %.2fs\n", result.Elapsed.Seconds())
	fmt.Printf("Throughput:          %.2f tx/sec\n", result.TxPerSec)
	fmt.Printf("Remainder Nodes:     %d\n", result.RemainderCount)
	fmt.Println()
	fmt.Printf("Profiles written to:\n")
	fmt.Printf("  CPU:    %s\n", cpuProfile)
	fmt.Printf("  Memory: %s\n", memProfile)
	fmt.Println()
	fmt.Printf("Analyze with:\n")
	fmt.Printf("  go tool pprof -http=:8080 %s\n", cpuProfile)
	fmt.Printf("  go tool pprof -http=:8081 %s\n", memProfile)

	if result.BenchErr != nil {
		fmt.Printf("\nNote: processRemainderTransactionsAndDequeue returned error: %v\n", result.BenchErr)
	}

	return nil
}

func runForeignBlockBenchmark(numSubtrees, txsPerSubtree, overlapPct, overhangPct int, seed uint64, cpuProfile, memProfile string) error {
	cfg := subtreeprocessor.ForeignBlockBenchConfig{
		NumBlockSubtrees: numSubtrees,
		TxsPerSubtree:    txsPerSubtree,
		OverlapPct:       overlapPct,
		PoolOverhangPct:  overhangPct,
		Seed:             seed,
	}

	result, err := subtreeprocessor.RunForeignBlockMoveBenchmark(cfg, cpuProfile, memProfile)
	if err != nil {
		return err
	}

	fmt.Printf("\nForeign Block Move Benchmark Results\n")
	fmt.Printf("====================================\n")
	fmt.Printf("Block Transactions:  %d\n", result.BlockTxCount)
	fmt.Printf("Pool Transactions:   %d\n", result.PoolTxCount)
	fmt.Printf("Map Build:           %.3fs\n", result.MapBuildElapsed.Seconds())
	fmt.Printf("Remainder:           %.3fs\n", result.RemainderElapsed.Seconds())
	fmt.Printf("Total:               %.3fs\n", result.TotalElapsed.Seconds())
	fmt.Printf("Map Length:          %d\n", result.MapLength)
	fmt.Printf("Remainder Nodes:     %d\n", result.RemainderCount)
	fmt.Printf("Alloc Delta:         %d MB\n", result.AllocDeltaMB)
	fmt.Println()
	fmt.Printf("Profiles written to:\n")
	fmt.Printf("  CPU:    %s\n", cpuProfile)
	fmt.Printf("  Memory: %s\n", memProfile)

	if result.BenchErr != nil {
		fmt.Printf("\nNote: benchmark returned error: %v\n", result.BenchErr)
	}

	return nil
}
