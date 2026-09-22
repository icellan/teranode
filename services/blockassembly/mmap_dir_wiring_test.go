package blockassembly

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	utxostoresql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/require"
)

// TestNewBlockAssembler_SubtreeMmapDir_WiresOption confirms a configured,
// writable blockassembly_subtreeMmapDir actually reaches the subtree
// processor construction (via WithMmapDir) rather than being silently
// ignored, which was the wiring bug this settings field fixes.
func TestNewBlockAssembler_SubtreeMmapDir_WiresOption(t *testing.T) {
	initPrometheusMetrics()

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.TxMapDirs = nil // isolate from any local settings_local.conf override
	tSettings.BlockAssembly.SubtreeMmapDir = t.TempDir()

	blockAssembler, err := buildTestBlockAssembler(t, tSettings)
	require.NoError(t, err)
	require.NotNil(t, blockAssembler)
}

// TestNewBlockAssembler_SubtreeMmapDir_NonExistent_FailsFast pins the
// fail-fast contract: a misconfigured directory must stop construction
// rather than silently falling back to heap-allocated subtree Nodes.
func TestNewBlockAssembler_SubtreeMmapDir_NonExistent_FailsFast(t *testing.T) {
	initPrometheusMetrics()

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.TxMapDirs = nil
	tSettings.BlockAssembly.SubtreeMmapDir = filepath.Join(t.TempDir(), "does-not-exist")

	_, err := buildTestBlockAssembler(t, tSettings)
	require.Error(t, err)
}

// TestNewBlockAssembler_TxMapDirs_WiresOption confirms configured, writable
// blockassembly_txMapDirs entries reach the subtree processor construction
// (via WithTxMapDirs).
func TestNewBlockAssembler_TxMapDirs_WiresOption(t *testing.T) {
	initPrometheusMetrics()

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.SubtreeMmapDir = ""
	tSettings.BlockAssembly.TxMapDirs = []string{t.TempDir(), t.TempDir()}

	blockAssembler, err := buildTestBlockAssembler(t, tSettings)
	require.NoError(t, err)
	require.NotNil(t, blockAssembler)
}

// TestNewBlockAssembler_TxMapDirs_NonExistent_FailsFast pins the fail-fast
// contract for a misconfigured entry in a multi-directory TxMapDirs list.
func TestNewBlockAssembler_TxMapDirs_NonExistent_FailsFast(t *testing.T) {
	initPrometheusMetrics()

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.SubtreeMmapDir = ""
	tSettings.BlockAssembly.TxMapDirs = []string{t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist")}

	_, err := buildTestBlockAssembler(t, tSettings)
	require.Error(t, err)
}

// TestNewBlockAssembler_TxMapDirs_NotWritable_FailsFast pins the fail-fast
// contract for a directory that exists but rejects writes.
func TestNewBlockAssembler_TxMapDirs_NotWritable_FailsFast(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, permission checks do not apply")
	}

	initPrometheusMetrics()

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))

	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
	})

	tSettings := createTestSettings(t)
	tSettings.BlockAssembly.SubtreeMmapDir = ""
	tSettings.BlockAssembly.TxMapDirs = []string{dir}

	_, err := buildTestBlockAssembler(t, tSettings)
	require.Error(t, err)
}

// buildTestBlockAssembler wires the minimal, real (in-memory/sqlite) dependencies
// NewBlockAssembler needs, so tests only vary the mmap/txmap settings under test.
func buildTestBlockAssembler(t *testing.T, tSettings *settings.Settings) (*BlockAssembler, error) {
	t.Helper()

	utxoStoreURL, err := url.Parse("sqlitememory:///test")
	require.NoError(t, err)

	store, err := utxostoresql.New(t.Context(), ulogger.TestLogger{}, tSettings, utxoStoreURL)
	require.NoError(t, err)

	stats := gocore.NewStat("test")

	blockchainStore, err := blockchainstore.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, tSettings)
	require.NoError(t, err)

	blockchainClient, err := blockchain.NewLocalClient(ulogger.TestLogger{}, tSettings, blockchainStore, nil, nil)
	require.NoError(t, err)

	return NewBlockAssembler(t.Context(), ulogger.TestLogger{}, tSettings, stats, store, nil, blockchainClient, nil)
}

// TestNewBlockAssembler_TxMapDirs_EmptyEntry_FailsFast pins the review finding
// that an empty list entry slipped through the writability check.
// ValidateWritableDir treats "" as "feature disabled" and returns nil, which is
// right for the setting as a whole but wrong per entry: gocore's GetMulti splits
// on "|" and only trims, so a trailing pipe, a doubled pipe or a whitespace-only
// entry yields "" in the middle of an otherwise valid list. NewDiskTxMap would
// then size itself for N disks and tempstore.New would map the empty base path
// to os.TempDir(), routing a share of the inpoints onto a RAM-backed tmpfs —
// silently inverting the purpose of the setting.
func TestNewBlockAssembler_TxMapDirs_EmptyEntry_FailsFast(t *testing.T) {
	initPrometheusMetrics()

	for name, dirs := range map[string][]string{
		"trailing empty":    {t.TempDir(), ""},
		"leading empty":     {"", t.TempDir()},
		"whitespace only":   {t.TempDir(), "   "},
		"single empty item": {""},
	} {
		t.Run(name, func(t *testing.T) {
			tSettings := createTestSettings(t)
			tSettings.BlockAssembly.SubtreeMmapDir = ""
			tSettings.BlockAssembly.TxMapDirs = dirs

			_, err := buildTestBlockAssembler(t, tSettings)
			require.Error(t, err)
			require.Contains(t, err.Error(), "empty directory")
		})
	}
}

// TestNewBlockAssembler_TxMapDirs_DuplicateEntry_FailsFast covers the other
// hand-editing slip in a "|"-separated list. DiskTxMap takes numDisks from
// len(paths) and builds one independent Badger shard, write batch and write-
// channel slice per entry, so a repeated path subdivides the hash space as if
// more physical devices backed it than actually do. The shards carry distinct
// prefixes so nothing collides, but the operator sees N directories and gets
// neither the striping nor the write-buffer sizing that implies.
func TestNewBlockAssembler_TxMapDirs_DuplicateEntry_FailsFast(t *testing.T) {
	initPrometheusMetrics()

	dir := t.TempDir()

	for name, dirs := range map[string][]string{
		"exact duplicate":    {dir, dir},
		"trailing separator": {dir, dir + string(filepath.Separator)},
		"unclean path":       {dir, filepath.Join(dir, "sub", "..")},
	} {
		t.Run(name, func(t *testing.T) {
			tSettings := createTestSettings(t)
			tSettings.BlockAssembly.SubtreeMmapDir = ""
			tSettings.BlockAssembly.TxMapDirs = dirs

			_, err := buildTestBlockAssembler(t, tSettings)
			require.Error(t, err)
			require.Contains(t, err.Error(), "duplicate directory")
		})
	}
}
