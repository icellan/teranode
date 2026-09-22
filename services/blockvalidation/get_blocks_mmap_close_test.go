package blockvalidation

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// TestFetchSubtreeAndDataFromPeer_ClosesMmapSubtree pins the catchup half of the
// mmap lifecycle. fetchAndStoreSubtree's local short-circuit — the common branch
// during catchup, since the subtree is usually already on disk — returns an
// mmap-backed subtree that the caller only reads. It is a local, so
// model.Block.ReleaseSubtreeNodes never sees it, and only Close() unmaps the
// region and unlinks the backing file.
//
// Leaking one mapping and one capacity*48-byte file per subtree walks the
// process into the default vm.max_map_count (~65530) during a long catchup;
// every later mmap then fails and the heap-fallback warning fires per subtree,
// blaming disk or permissions. The files outlive a restart, because nothing else
// removes them.
func TestFetchSubtreeAndDataFromPeer_ClosesMmapSubtree(t *testing.T) {
	ctx := context.Background()
	mmapDir := t.TempDir()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockValidation.SubtreeMmapDir = mmapDir

	subtreeStore := memory.New()

	server := &Server{
		logger:       ulogger.TestLogger{},
		subtreeStore: subtreeStore,
		settings:     tSettings,
	}

	coinbaseTx, err := bt.NewTxFromString(model.CoinbaseHex)
	require.NoError(t, err)

	subtree, err := subtreepkg.NewTreeByLeafCount(2)
	require.NoError(t, err)
	require.NoError(t, subtree.AddCoinbaseNode())

	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)

	subtreeData := subtreepkg.NewSubtreeData(subtree)
	require.NoError(t, subtreeData.AddTx(coinbaseTx, 0))

	subtreeDataBytes, err := subtreeData.Serialize()
	require.NoError(t, err)

	hash := subtree.RootHash()

	// Both files present locally: fetchAndStoreSubtree takes its localExists
	// branch and mmap-deserializes, fetchAndStoreSubtreeData short-circuits.
	require.NoError(t, subtreeStore.Set(ctx, hash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes))
	require.NoError(t, subtreeStore.Set(ctx, hash[:], fileformat.FileTypeSubtreeData, subtreeDataBytes))

	block := &model.Block{Height: 100}

	require.NoError(t, server.fetchSubtreeAndDataFromPeer(ctx, block, hash, "peer", "http://peer:8000", false, nil))

	leaked, err := filepath.Glob(filepath.Join(mmapDir, "subtree-nodes-*"))
	require.NoError(t, err)
	require.Empty(t, leaked, "the mmap-backed subtree must be closed once catchup has finished reading it")
}
