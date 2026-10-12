package seeder

import (
	"context"
	"encoding/binary"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob"
	bloboptions "github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// TestProcessHeaders_ReRun_IsIdempotentOnAlreadyStoredBlock is the
// reproduction for the first blocker in the -force recovery: a -force re-run
// still runs processHeaders, which re-stores every block from the headers file,
// including ones a prior run already stored. StoreBlock reports those as
// ErrBlockExists; if that is fatal the run fails before it reaches the
// BlockAssembler checkpoint write. It must be treated as an idempotent replay.
func TestProcessHeaders_ReRun_IsIdempotentOnAlreadyStoredBlock(t *testing.T) {
	ctx := context.Background()
	store := newTestBlockchainStore(t)

	genesis, err := store.GetBlockByID(ctx, 0)
	require.NoError(t, err)

	genesisRecord, err := blockToIndexBytes(t, genesis, genesis.Header.Hash())
	require.NoError(t, err)

	coinbase := makeCoinbaseTx(t, 1, 1)
	block1 := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			Timestamp:      1700000001,
			Nonce:          1,
			HashPrevBlock:  genesis.Header.Hash(),
			HashMerkleRoot: coinbase.TxIDChainHash(),
			Bits:           genesis.Header.Bits,
		},
		CoinbaseTx:       coinbase,
		TransactionCount: 1,
		Height:           1,
	}
	block1Record, err := blockToIndexBytes(t, block1, block1.Header.Hash())
	require.NoError(t, err)

	path := writeUtxoHeadersFile(t, 1, genesisRecord, block1Record)

	// First pass: a complete, successful header import.
	require.NoError(t, processHeaders(ctx, ulogger.TestLogger{}, store, path))

	// Recovery re-run against the identical, complete file: this is exactly
	// what re-running with -force does. It must not fail on blocks the first
	// pass already stored.
	err = processHeaders(ctx, ulogger.TestLogger{}, store, path)
	require.NoError(t, err, "a re-run over already-stored blocks must be idempotent, not fatal")
}

// writeUtxoSetPreamble writes a minimal but complete .utxo-set file
// containing only the preamble readUTXOSetTip depends on (file header, tip
// hash, tip height, previous-block-hash placeholder) and no UTXO records.
// Sufficient for exercising the cheap checkpoint-recovery path, which never
// reads past the preamble.
func writeUtxoSetPreamble(t *testing.T, tipHash chainhash.Hash, tipHeight uint32) string {
	t.Helper()

	header := fileformat.NewHeader(fileformat.FileTypeUtxoSet)

	heightBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(heightBytes, tipHeight)

	var previousBlockHash [32]byte

	data := make([]byte, 0, len(header.Bytes())+len(tipHash)+len(heightBytes)+len(previousBlockHash))
	data = append(data, header.Bytes()...)
	data = append(data, tipHash[:]...)
	data = append(data, heightBytes...)
	data = append(data, previousBlockHash[:]...)

	path := filepath.Join(t.TempDir(), "test.utxo-set")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// TestReadUTXOSetTip_ReadsPreambleWithoutFullImport is the positive-path
// coverage for the cheapest of the three recovery options the review offered:
// the utxo-set file's own preamble already carries the tip hash and height,
// so the missing checkpoint can be recovered by reading a handful of bytes -
// no re-import of the (already complete) UTXO set required.
func TestReadUTXOSetTip_ReadsPreambleWithoutFullImport(t *testing.T) {
	var tipHash chainhash.Hash
	tipHash[0] = 0xab
	tipHash[31] = 0xcd

	path := writeUtxoSetPreamble(t, tipHash, 42)

	tip, err := readUTXOSetTip(path)
	require.NoError(t, err)
	require.Equal(t, uint32(42), tip.height)
	require.Equal(t, tipHash.String(), tip.hash.String())
}

// TestCheckpointRecoverable_MarkerWithoutCheckpoint_ReportsRecoverable covers
// the decision the -force path uses to avoid re-importing an already-complete
// UTXO set: lastProcessed.dat present with no BlockAssembler checkpoint is
// exactly the state a cheap, preamble-only recovery applies to.
func TestCheckpointRecoverable_MarkerWithoutCheckpoint_ReportsRecoverable(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	markLastProcessed(t, ctx, blockStore)

	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 1)

	recoverable, err := checkpointRecoverable(ctx, blockStore, blockchainStore, utxoFile)
	require.NoError(t, err)
	require.True(t, recoverable)
}

// TestCheckpointRecoverable_NoMarker_NotRecoverable covers the fresh-seed
// case: no lastProcessed.dat means there is nothing to recover - the normal
// full import must run instead.
func TestCheckpointRecoverable_NoMarker_NotRecoverable(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 1)

	recoverable, err := checkpointRecoverable(ctx, blockStore, blockchainStore, utxoFile)
	require.NoError(t, err)
	require.False(t, recoverable)
}

// TestCheckpointRecoverable_MarkerWithCheckpoint_NotRecoverable covers the
// cleanly-completed case: both the marker and the checkpoint are present, so
// there is nothing left to recover.
func TestCheckpointRecoverable_MarkerWithCheckpoint_NotRecoverable(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	markLastProcessed(t, ctx, blockStore)

	block := storeBlockAboveGenesis(t, ctx, blockchainStore)
	require.NoError(t, writeBlockAssemblerState(ctx, ulogger.TestLogger{}, blockchainStore, &utxoSetTip{hash: *block.Hash(), height: 1}))

	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 1)

	recoverable, err := checkpointRecoverable(ctx, blockStore, blockchainStore, utxoFile)
	require.NoError(t, err)
	require.False(t, recoverable)
}

// TestCheckpointRecoverable_MarkerHeightMismatchesUtxoFile_NotRecoverable is
// the reproduction for the snapshot-agnostic marker regression: the marker only
// proves *an* import finished, not that it was for the utxoFile this run was
// invoked with. Sequence: seed from snapshot A (tip 800000), the header pass
// rejects A's truncated utxo-headers file after the UTXO pass already wrote
// lastProcessed.dat, the operator fetches a complete snapshot B (tip 810000)
// and re-runs with -force. checkpointRecoverable must see that the marker's
// recorded height does not match snapshot B's preamble height and refuse the
// cheap path - taking it here would commit a BlockAssembler checkpoint at B's
// tip over a UTXO store that still holds A's set.
func TestCheckpointRecoverable_MarkerHeightMismatchesUtxoFile_NotRecoverable(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	// Marker left behind by the import of snapshot A (tip height 800000).
	err := blockStore.Set(ctx, nil, fileformat.FileTypeDat, []byte("800000\n"),
		bloboptions.WithFilename("lastProcessed"), bloboptions.WithNoHashPrefix())
	require.NoError(t, err)

	// The -force re-run is now pointed at snapshot B (tip height 810000).
	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 810000)

	recoverable, err := checkpointRecoverable(ctx, blockStore, blockchainStore, utxoFile)
	require.NoError(t, err)
	require.False(t, recoverable, "a marker for a different snapshot must not be treated as proof this utxoFile was already imported")
}

// forceSeederSettings builds settings for a processUTXOs run against an
// in-memory UTXO and blockchain store and an on-disk block store, so the
// lastProcessed.dat marker is a real blob the test can pre-populate and read
// back.
func forceSeederSettings(t *testing.T) *settings.Settings {
	t.Helper()

	tSettings := test.CreateBaseTestSettings(t)

	blockchainStoreURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)

	tSettings.BlockChain.StoreURL = blockchainStoreURL

	utxoStoreURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)

	tSettings.UtxoStore.UtxoStore = utxoStoreURL

	blockStoreURL, err := url.Parse("file://" + t.TempDir())
	require.NoError(t, err)

	tSettings.Block.BlockStore = blockStoreURL

	return tSettings
}

func openTestBlockStore(t *testing.T, tSettings *settings.Settings) blob.Store {
	t.Helper()

	store, err := blob.NewStore(ulogger.TestLogger{}, tSettings.Block.BlockStore)
	require.NoError(t, err)

	return store
}

func readMarker(t *testing.T, ctx context.Context, store blob.Store) string {
	t.Helper()

	b, err := store.Get(ctx, nil, fileformat.FileTypeDat, bloboptions.WithFilename("lastProcessed"), bloboptions.WithNoHashPrefix())
	require.NoError(t, err)

	return strings.TrimSpace(string(b))
}

// TestProcessUTXOs_Force_OverExistingMarkerOfOtherSnapshot_ReimportsAndRewritesMarker
// covers two things at once: a marker recording a different snapshot's height
// must not take the cheap recovery path (the UTXO store would otherwise keep
// the other snapshot's set), and the full re-import that follows must be able
// to rewrite lastProcessed.dat, which already exists. Without
// WithAllowOverwrite on the marker write the re-import fails with
// BlobAlreadyExists after the whole import has run.
func TestProcessUTXOs_Force_OverExistingMarkerOfOtherSnapshot_ReimportsAndRewritesMarker(t *testing.T) {
	ctx := context.Background()
	tSettings := forceSeederSettings(t)
	blockStore := openTestBlockStore(t, tSettings)

	require.NoError(t, blockStore.Set(ctx, nil, fileformat.FileTypeDat, []byte("800000\n"),
		bloboptions.WithFilename("lastProcessed"), bloboptions.WithNoHashPrefix()))

	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 810000)

	tip, err := processUTXOs(ctx, ulogger.TestLogger{}, tSettings, newTestBlockchainStore(t), utxoFile, "", true)
	require.NoError(t, err)
	require.NotNil(t, tip)
	require.Equal(t, uint32(810000), tip.height)
	require.Equal(t, "810000", readMarker(t, ctx, blockStore), "the full re-import must have rewritten the marker")
}

// TestProcessUTXOs_Force_MarkerMatchesFile_RecoversTipWithoutReimport covers
// the cheap recovery: the marker's height equals the utxo-set preamble height
// and no checkpoint exists, so the tip comes straight from the preamble. The
// marker is left untouched, which is how this test tells it from a re-import
// (a re-import rewrites it).
func TestProcessUTXOs_Force_MarkerMatchesFile_RecoversTipWithoutReimport(t *testing.T) {
	ctx := context.Background()
	tSettings := forceSeederSettings(t)
	blockStore := openTestBlockStore(t, tSettings)

	require.NoError(t, blockStore.Set(ctx, nil, fileformat.FileTypeDat, []byte("42\n"),
		bloboptions.WithFilename("lastProcessed"), bloboptions.WithNoHashPrefix()))

	var tipHash chainhash.Hash
	tipHash[0] = 0xab

	utxoFile := writeUtxoSetPreamble(t, tipHash, 42)

	tip, err := processUTXOs(ctx, ulogger.TestLogger{}, tSettings, newTestBlockchainStore(t), utxoFile, "", true)
	require.NoError(t, err)
	require.NotNil(t, tip)
	require.Equal(t, uint32(42), tip.height)
	require.Equal(t, tipHash.String(), tip.hash.String())
	require.Equal(t, "42", readMarker(t, ctx, blockStore))
}

// TestCheckpointRecoverable_UnparsableMarker_NotRecoverable: a marker whose
// contents are not a height proves nothing about this utxoFile, so -force
// falls through to the full re-import instead of failing on it.
func TestCheckpointRecoverable_UnparsableMarker_NotRecoverable(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	require.NoError(t, blockStore.Set(ctx, nil, fileformat.FileTypeDat, []byte("not a height\n"),
		bloboptions.WithFilename("lastProcessed"), bloboptions.WithNoHashPrefix()))

	utxoFile := writeUtxoSetPreamble(t, chainhash.Hash{}, 1)

	recoverable, err := checkpointRecoverable(ctx, blockStore, blockchainStore, utxoFile)
	require.NoError(t, err)
	require.False(t, recoverable)
}

// TestCheckSkipUTXOImport_MarkerWithoutCheckpoint_NamesWorkingRecovery
// guards the error message operators actually see: it must not promise a
// recovery ("-force to re-import the UTXO set") that cannot complete. The
// real recovery -force now performs is a cheap checkpoint recovery, not a
// re-import.
func TestCheckSkipUTXOImport_MarkerWithoutCheckpoint_NamesWorkingRecovery(t *testing.T) {
	ctx := context.Background()
	blockchainStore := newTestBlockchainStore(t)
	blockStore := newTestBlobStore(t)

	markLastProcessed(t, ctx, blockStore)

	_, err := checkSkipUTXOImport(ctx, blockStore, blockchainStore)
	require.Error(t, err)
	require.ErrorContains(t, err, "-force")
	require.NotContains(t, err.Error(), "re-import the UTXO set",
		"the message must not promise a re-import as the recovery - the actual recovery never re-imports the UTXO set")
}
