package subtreeprocessor

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/settings"
	blob_memory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// CreateTransactionMapBenchmarkResult holds results from the CreateTransactionMap benchmark
type CreateTransactionMapBenchmarkResult struct {
	NumSubtrees      int
	TxsPerSubtree    int
	TotalTxs         int
	Elapsed          time.Duration
	TxPerSec         float64
	MapLength        int
	ConflictingNodes int
	BenchErr         error
}

// ProcessRemainderBenchmarkResult holds results from the processRemainderTransactionsAndDequeue benchmark
type ProcessRemainderBenchmarkResult struct {
	NumChainedSubtrees int
	TxsPerSubtree      int
	TotalTxs           int
	Elapsed            time.Duration
	TxPerSec           float64
	RemainderCount     int
	BenchErr           error
}

func createBenchmarkSettings(txsPerSubtree int) *settings.Settings {
	s := settings.NewSettings()
	s.DataFolder = os.TempDir() + "/txmapbench"

	chainParams := chaincfg.RegressionNetParams
	chainParams.CoinbaseMaturity = 1
	s.ChainCfgParams = &chainParams
	s.GlobalBlockHeightRetention = 10
	s.BlockValidation.OptimisticMining = false
	s.BlockAssembly.InitialMerkleItemsPerSubtree = txsPerSubtree

	return s
}

// RunCreateTransactionMapBenchmark runs the CreateTransactionMap benchmark with profiling
func RunCreateTransactionMapBenchmark(numSubtrees, txsPerSubtree int, cpuProfile, memProfile string) (CreateTransactionMapBenchmarkResult, error) {
	totalTxs := numSubtrees * txsPerSubtree
	benchStartTime := time.Now()

	fmt.Printf("CreateTransactionMap Benchmark\n")
	fmt.Printf("==============================\n")
	fmt.Printf("Subtrees:         %d\n", numSubtrees)
	fmt.Printf("Txs per subtree:  %d\n", txsPerSubtree)
	fmt.Printf("Total Txs:        %d\n", totalTxs)
	fmt.Printf("CPU Cores:        %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS:       %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	// ===== SETUP PHASE (not profiled) =====
	fmt.Printf("[%s] Setting up benchmark...\n", time.Since(benchStartTime))

	ctx := context.Background()
	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	defer close(newSubtreeChan)

	subtreeStore := blob_memory.New()

	tSettings := createBenchmarkSettings(txsPerSubtree)

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	if err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to parse utxo store URL: %w", err)
	}

	utxoStore, err := sql.New(ctx, ulogger.TestLogger{}, tSettings, utxoStoreURL)
	if err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to create utxo store: %w", err)
	}

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, tSettings, subtreeStore, nil, utxoStore, newSubtreeChan)
	if err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to create subtree processor: %w", err)
	}

	stp.SetCurrentItemsPerFile(1024 * 1024)

	fmt.Printf("[%s] Creating %d subtrees with %d txs each...\n", time.Since(benchStartTime), numSubtrees, txsPerSubtree)

	blockSubtreesMap := make(map[chainhash.Hash]int, numSubtrees)

	for s := 0; s < numSubtrees; s++ {
		subtree, err := subtreepkg.NewTreeByLeafCount(txsPerSubtree)
		if err != nil {
			return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to create subtree %d: %w", s, err)
		}

		if s == 0 {
			if err := subtree.AddCoinbaseNode(); err != nil {
				return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to add coinbase node: %w", err)
			}
		}

		for i := 0; i < txsPerSubtree-1; i++ {
			txHash := chainhash.HashH([]byte(fmt.Sprintf("tx-%d-%d", s, i)))
			if err := subtree.AddNode(txHash, uint64(s*txsPerSubtree+i+1), 100); err != nil {
				return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to add node: %w", err)
			}
		}

		subtreeBytes, err := subtree.Serialize()
		if err != nil {
			return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to serialize subtree: %w", err)
		}

		// DAH = currentBlockHeight + retention. Benchmark runs at height 0, so DAH = retention.
		if err := subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes, options.WithDeleteAt(0+tSettings.GlobalBlockHeightRetention)); err != nil {
			return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to store subtree: %w", err)
		}

		blockSubtreesMap[*subtree.RootHash()] = s
	}
	fmt.Printf("[%s] Subtrees created and stored.\n", time.Since(benchStartTime))
	fmt.Printf("[%s] Setup complete. Starting profiled benchmark...\n\n", time.Since(benchStartTime))

	// ===== PROFILING PHASE =====
	cpuFile, err := os.Create(cpuProfile)
	if err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to create CPU profile: %w", err)
	}

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		cpuFile.Close()
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to start CPU profile: %w", err)
	}

	// Run the benchmark
	startTime := time.Now()
	transactionMap, conflictingNodes, benchErr := stp.CreateTransactionMap(ctx, blockSubtreesMap, numSubtrees, uint64(totalTxs))
	elapsed := time.Since(startTime)

	// Stop CPU profiling
	pprof.StopCPUProfile()
	if err := cpuFile.Close(); err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to close CPU profile: %w", err)
	}

	// Write memory profile
	runtime.GC()
	memFile, err := os.Create(memProfile)
	if err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to create memory profile: %w", err)
	}
	if err := pprof.WriteHeapProfile(memFile); err != nil {
		memFile.Close()
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to write memory profile: %w", err)
	}
	if err := memFile.Close(); err != nil {
		return CreateTransactionMapBenchmarkResult{}, errors.NewProcessingError("failed to close memory profile: %w", err)
	}

	// Build result
	mapLength := 0
	if transactionMap != nil {
		mapLength = transactionMap.Length()
	}

	result := CreateTransactionMapBenchmarkResult{
		NumSubtrees:      numSubtrees,
		TxsPerSubtree:    txsPerSubtree,
		TotalTxs:         totalTxs,
		Elapsed:          elapsed,
		TxPerSec:         float64(totalTxs) / elapsed.Seconds(),
		MapLength:        mapLength,
		ConflictingNodes: len(conflictingNodes),
		BenchErr:         benchErr,
	}

	return result, nil
}

// RunProcessRemainderBenchmark runs the processRemainderTransactionsAndDequeue benchmark with profiling
func RunProcessRemainderBenchmark(numChainedSubtrees, txsPerSubtree int, cpuProfile, memProfile string) (ProcessRemainderBenchmarkResult, error) {
	totalTxs := (numChainedSubtrees + 1) * txsPerSubtree
	benchStartTime := time.Now()

	fmt.Printf("ProcessRemainderTransactionsAndDequeue Benchmark\n")
	fmt.Printf("================================================\n")
	fmt.Printf("Chained Subtrees:   %d\n", numChainedSubtrees)
	fmt.Printf("Txs per subtree:    %d\n", txsPerSubtree)
	fmt.Printf("Total Txs:          %d\n", totalTxs)
	fmt.Printf("CPU Cores:          %d\n", runtime.NumCPU())
	fmt.Printf("GOMAXPROCS:         %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()

	// ===== SETUP PHASE (not profiled) =====
	fmt.Printf("[%s] Setting up benchmark...\n", time.Since(benchStartTime))

	ctx := context.Background()
	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	defer close(newSubtreeChan)

	tSettings := createBenchmarkSettings(txsPerSubtree * 2)

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, tSettings, nil, nil, nil, newSubtreeChan)
	if err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create subtree processor: %w", err)
	}

	// Initialize current subtree
	newSubtree, err := subtreepkg.NewTreeByLeafCount(txsPerSubtree * 2)
	if err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create new subtree: %w", err)
	}
	stp.currentSubtree.Store(newSubtree)
	stp.chainedSubtrees = nil
	_ = stp.GetCurrentSubtree().AddCoinbaseNode()

	fmt.Printf("[%s] Creating chained subtrees and transaction data...\n", time.Since(benchStartTime))

	// Create chained subtrees with transaction data
	chainedSubtrees := make([]*subtreepkg.Subtree, numChainedSubtrees)
	allTxHashes := make([]chainhash.Hash, 0, totalTxs)
	parentHash := chainhash.HashH([]byte("parent-tx-benchmark"))

	for s := 0; s < numChainedSubtrees; s++ {
		subtree, err := subtreepkg.NewTreeByLeafCount(txsPerSubtree)
		if err != nil {
			return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create chained subtree %d: %w", s, err)
		}

		if s == 0 {
			_ = subtree.AddCoinbaseNode()
		}

		for i := 0; i < txsPerSubtree-1; i++ {
			txHash := chainhash.HashH([]byte(fmt.Sprintf("tx-chained-%d-%d", s, i)))
			if err := subtree.AddSubtreeNode(subtreepkg.Node{Hash: txHash, Fee: 100}); err != nil {
				return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to add chained subtree node: %w", err)
			}
			allTxHashes = append(allTxHashes, txHash)
		}
		chainedSubtrees[s] = subtree
	}

	// Create current subtree with transactions
	currentSubtree, err := subtreepkg.NewTreeByLeafCount(txsPerSubtree)
	if err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create current subtree: %w", err)
	}
	_ = currentSubtree.AddCoinbaseNode()
	for i := 0; i < txsPerSubtree-1; i++ {
		txHash := chainhash.HashH([]byte(fmt.Sprintf("tx-current-%d", i)))
		if err := currentSubtree.AddSubtreeNode(subtreepkg.Node{Hash: txHash, Fee: 100}); err != nil {
			return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to add current subtree node: %w", err)
		}
		allTxHashes = append(allTxHashes, txHash)
	}

	fmt.Printf("[%s] Created %d transaction hashes.\n", time.Since(benchStartTime), len(allTxHashes))

	// Create transaction map with all transactions (simulating external block)
	transactionMap := NewSplitSwissMap(1024, len(allTxHashes))
	for _, hash := range allTxHashes {
		if err := transactionMap.Put(hash); err != nil {
			return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to put hash in transaction map: %w", err)
		}
	}

	// Production freezes the map after CreateTransactionMap; match it so the
	// benchmark exercises the lock-free Exists path.
	transactionMap.Freeze()
	fmt.Printf("[%s] Transaction map created with %d entries.\n", time.Since(benchStartTime), transactionMap.Length())

	// Create current tx map for parent lookups
	currentTxMap := NewSplitTxInpointsMap(16)
	for _, hash := range allTxHashes {
		currentTxMap.Set(hash, &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{parentHash}})
	}
	fmt.Printf("[%s] Current tx map created with %d entries.\n", time.Since(benchStartTime), currentTxMap.Length())

	losingTxHashesMap := txmap.NewSplitSwissMap(4, 10)

	// Create mock block
	block := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  &chainhash.Hash{},
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      uint32(time.Now().Unix()), // nolint: gosec
			Bits:           model.NBit{},
			Nonce:          0,
		},
		CoinbaseTx: &bt.Tx{},
	}

	params := &RemainderTransactionParams{
		Block:             block,
		ChainedSubtrees:   chainedSubtrees,
		CurrentSubtree:    currentSubtree,
		TransactionMap:    transactionMap,
		LosingTxHashesMap: losingTxHashesMap,
		CurrentTxMap:      currentTxMap,
		SkipDequeue:       true,
		SkipNotification:  true,
	}

	fmt.Printf("[%s] Setup complete. Starting profiled benchmark...\n\n", time.Since(benchStartTime))

	// ===== PROFILING PHASE =====
	cpuFile, err := os.Create(cpuProfile)
	if err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create CPU profile: %w", err)
	}

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		cpuFile.Close()
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to start CPU profile: %w", err)
	}

	// Run the benchmark
	startTime := time.Now()
	benchErr := stp.processRemainderTransactionsAndDequeue(ctx, params)
	elapsed := time.Since(startTime)

	// Stop CPU profiling
	pprof.StopCPUProfile()
	if err := cpuFile.Close(); err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to close CPU profile: %w", err)
	}

	// Write memory profile
	runtime.GC()
	memFile, err := os.Create(memProfile)
	if err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to create memory profile: %w", err)
	}
	if err := pprof.WriteHeapProfile(memFile); err != nil {
		memFile.Close()
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to write memory profile: %w", err)
	}
	if err := memFile.Close(); err != nil {
		return ProcessRemainderBenchmarkResult{}, errors.NewProcessingError("failed to close memory profile: %w", err)
	}

	// Count remainder nodes
	remainderCount := 0
	for _, st := range stp.GetChainedSubtrees() {
		remainderCount += st.Length()
	}
	remainderCount += stp.GetCurrentSubtree().Length()

	result := ProcessRemainderBenchmarkResult{
		NumChainedSubtrees: numChainedSubtrees,
		TxsPerSubtree:      txsPerSubtree,
		TotalTxs:           totalTxs,
		Elapsed:            elapsed,
		TxPerSec:           float64(totalTxs) / elapsed.Seconds(),
		RemainderCount:     remainderCount,
		BenchErr:           benchErr,
	}

	return result, nil
}

// ForeignBlockBenchConfig parameterizes RunForeignBlockMoveBenchmark.
//
// The block's tx set and the local pool overlap like production: the pool
// holds OverlapPct% of the block's txs (arranged into *differently chunked*
// local subtrees) plus PoolOverhangPct% extra local-only txs — the remainder
// that must be rebuilt into fresh subtrees after the block is processed.
type ForeignBlockBenchConfig struct {
	NumBlockSubtrees int
	TxsPerSubtree    int
	OverlapPct       int // % of block txs also present in the local pool
	PoolOverhangPct  int // local-only txs as % of block tx count
	Seed             uint64
}

// ForeignBlockBenchResult holds per-phase results from RunForeignBlockMoveBenchmark.
type ForeignBlockBenchResult struct {
	BlockTxCount     int
	PoolTxCount      int
	MapBuildElapsed  time.Duration // CreateTransactionMap (block tx set construction)
	RemainderElapsed time.Duration // processRemainderTransactionsAndDequeue
	TotalElapsed     time.Duration
	MapLength        int
	RemainderCount   int
	AllocDeltaMB     uint64 // TotalAlloc delta across both phases
	BenchErr         error
}

// genDeterministicHashes generates n pseudo-random 32-byte hashes from seed.
// PCG output matches the key-distribution properties of real tx hashes at a
// fraction of the cost of hashing real data.
func genDeterministicHashes(n int, seed uint64) []chainhash.Hash {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	hashes := make([]chainhash.Hash, n)

	for i := range hashes {
		binary.LittleEndian.PutUint64(hashes[i][0:8], rng.Uint64())
		binary.LittleEndian.PutUint64(hashes[i][8:16], rng.Uint64())
		binary.LittleEndian.PutUint64(hashes[i][16:24], rng.Uint64())
		binary.LittleEndian.PutUint64(hashes[i][24:32], rng.Uint64())
	}

	return hashes
}

// RunForeignBlockMoveBenchmark times the two foreign-block phases of
// moveForwardBlock — CreateTransactionMap (block map build) and
// processRemainderTransactionsAndDequeue (pool scan + remainder rebuild) —
// against a realistic pool/block overlap. cpuProfile/memProfile may be empty
// to skip profiling (e.g. when driven from go-test benchmarks).
func RunForeignBlockMoveBenchmark(cfg ForeignBlockBenchConfig, cpuProfile, memProfile string) (ForeignBlockBenchResult, error) {
	ctx := context.Background()

	blockTxCount := cfg.NumBlockSubtrees * (cfg.TxsPerSubtree - 1) // subtree 0 carries the coinbase placeholder
	overlapCount := blockTxCount * cfg.OverlapPct / 100
	overhangCount := blockTxCount * cfg.PoolOverhangPct / 100

	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0xdeadbeef))

	// Block txs: the first overlapCount are shared with the pool.
	blockTxs := genDeterministicHashes(blockTxCount, cfg.Seed+1)
	poolTxs := make([]chainhash.Hash, 0, overlapCount+overhangCount)
	poolTxs = append(poolTxs, blockTxs[:overlapCount]...)
	poolTxs = append(poolTxs, genDeterministicHashes(overhangCount, cfg.Seed+2)...)

	// The local pool saw the same txs in a different order than the miner
	// chunked them — shuffle before building local subtrees.
	rng.Shuffle(len(poolTxs), func(i, j int) {
		poolTxs[i], poolTxs[j] = poolTxs[j], poolTxs[i]
	})

	newSubtreeChan := make(chan NewSubtreeRequest, 10)
	go func() {
		for req := range newSubtreeChan {
			if req.ErrChan != nil {
				req.ErrChan <- nil
			}
		}
	}()
	defer close(newSubtreeChan)

	subtreeStore := blob_memory.New()
	tSettings := createBenchmarkSettings(cfg.TxsPerSubtree)

	stp, err := NewSubtreeProcessor(ctx, ulogger.TestLogger{}, tSettings, subtreeStore, nil, nil, newSubtreeChan)
	if err != nil {
		return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create subtree processor: %w", err)
	}

	stp.SetCurrentItemsPerFile(cfg.TxsPerSubtree)

	// ----- foreign block: serialize subtrees into the blob store -----
	blockSubtreesMap := make(map[chainhash.Hash]int, cfg.NumBlockSubtrees)
	txIdx := 0

	for s := 0; s < cfg.NumBlockSubtrees; s++ {
		subtree, err := subtreepkg.NewTreeByLeafCount(cfg.TxsPerSubtree)
		if err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create block subtree %d: %w", s, err)
		}

		if s == 0 {
			if err = subtree.AddCoinbaseNode(); err != nil {
				return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to add coinbase node: %w", err)
			}
		}

		perSubtree := cfg.TxsPerSubtree - 1
		for i := 0; i < perSubtree && txIdx < blockTxCount; i++ {
			if err = subtree.AddNode(blockTxs[txIdx], 100, 200); err != nil {
				return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to add block subtree node: %w", err)
			}
			txIdx++
		}

		subtreeBytes, err := subtree.Serialize()
		if err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to serialize block subtree: %w", err)
		}

		if err = subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtree, subtreeBytes, options.WithDeleteAt(tSettings.GlobalBlockHeightRetention)); err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to store block subtree: %w", err)
		}

		blockSubtreesMap[*subtree.RootHash()] = s
	}

	// ----- local pool: chained subtrees + current subtree + currentTxMap -----
	parentHash := chainhash.HashH([]byte("parent-tx-foreign-block-bench"))
	poolTxMap := NewSplitTxInpointsMap(16)

	numLocalSubtrees := (len(poolTxs) + cfg.TxsPerSubtree - 1) / cfg.TxsPerSubtree
	chainedSubtrees := make([]*subtreepkg.Subtree, 0, numLocalSubtrees)
	poolIdx := 0

	for s := 0; s < numLocalSubtrees; s++ {
		subtree, err := subtreepkg.NewTreeByLeafCount(cfg.TxsPerSubtree)
		if err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create local subtree %d: %w", s, err)
		}

		if s == 0 {
			_ = subtree.AddCoinbaseNode()
		}

		for !subtree.IsComplete() && poolIdx < len(poolTxs) {
			if err = subtree.AddSubtreeNode(subtreepkg.Node{Hash: poolTxs[poolIdx], Fee: 100}); err != nil {
				return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to add local subtree node: %w", err)
			}
			poolTxMap.Set(poolTxs[poolIdx], &subtreepkg.TxInpoints{ParentTxHashes: []chainhash.Hash{parentHash}})
			poolIdx++
		}

		chainedSubtrees = append(chainedSubtrees, subtree)

		if poolIdx >= len(poolTxs) {
			break
		}
	}

	// Fresh current subtree, as after resetSubtreeState.
	currentSubtree, err := subtreepkg.NewTreeByLeafCount(cfg.TxsPerSubtree)
	if err != nil {
		return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create current subtree: %w", err)
	}
	_ = currentSubtree.AddCoinbaseNode()
	stp.currentSubtree.Store(currentSubtree)

	losingTxHashesMap := txmap.NewSplitSwissMap(4, 10)

	block := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  &chainhash.Hash{},
			HashMerkleRoot: &chainhash.Hash{},
			Timestamp:      uint32(time.Now().Unix()), // nolint: gosec
			Bits:           model.NBit{},
			Nonce:          0,
		},
		CoinbaseTx: &bt.Tx{},
	}

	// ----- profiled phases -----
	var cpuFile *os.File
	if cpuProfile != "" {
		if cpuFile, err = os.Create(cpuProfile); err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create CPU profile: %w", err)
		}

		if err = pprof.StartCPUProfile(cpuFile); err != nil {
			cpuFile.Close()
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to start CPU profile: %w", err)
		}
	}

	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	result := ForeignBlockBenchResult{
		BlockTxCount: blockTxCount,
		PoolTxCount:  len(poolTxs),
	}

	// Phase 1: block tx set construction (the phase the inverted diff replaces).
	mapStart := time.Now()
	transactionMap, _, benchErr := stp.CreateTransactionMap(ctx, blockSubtreesMap, cfg.NumBlockSubtrees, uint64(blockTxCount))
	result.MapBuildElapsed = time.Since(mapStart)

	if benchErr == nil {
		result.MapLength = transactionMap.Length()

		// Phase 2: pool scan + remainder rebuild.
		params := &RemainderTransactionParams{
			Block:             block,
			ChainedSubtrees:   chainedSubtrees,
			CurrentSubtree:    currentSubtree,
			TransactionMap:    transactionMap,
			LosingTxHashesMap: losingTxHashesMap,
			CurrentTxMap:      poolTxMap,
			SkipDequeue:       true,
			SkipNotification:  true,
		}

		remainderStart := time.Now()
		benchErr = stp.processRemainderTransactionsAndDequeue(ctx, params)
		result.RemainderElapsed = time.Since(remainderStart)
	}

	result.TotalElapsed = result.MapBuildElapsed + result.RemainderElapsed
	result.BenchErr = benchErr

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	result.AllocDeltaMB = (memAfter.TotalAlloc - memBefore.TotalAlloc) / (1024 * 1024)

	if cpuProfile != "" {
		pprof.StopCPUProfile()
		if err = cpuFile.Close(); err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to close CPU profile: %w", err)
		}
	}

	if memProfile != "" {
		runtime.GC()

		memFile, err := os.Create(memProfile)
		if err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to create memory profile: %w", err)
		}

		if err = pprof.WriteHeapProfile(memFile); err != nil {
			memFile.Close()
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to write memory profile: %w", err)
		}

		if err = memFile.Close(); err != nil {
			return ForeignBlockBenchResult{}, errors.NewProcessingError("failed to close memory profile: %w", err)
		}
	}

	// Count remainder nodes rebuilt into the processor.
	for _, st := range stp.GetChainedSubtrees() {
		result.RemainderCount += st.Length()
	}
	result.RemainderCount += stp.GetCurrentSubtree().Length()

	return result, nil
}
