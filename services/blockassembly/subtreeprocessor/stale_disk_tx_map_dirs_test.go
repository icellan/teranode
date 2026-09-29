package subtreeprocessor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Only Close removes a disk tx map's Badger directories, so a node that exits
// without it (a kill, or a Stop that timed out on a running handler) leaves
// them behind, and every such exit adds another set. A new subtree processor
// removes the ones in its configured dirs before creating its own, and leaves
// everything else there alone.
func TestNewSubtreeProcessor_RemovesStaleDiskTxMapDirs(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}

	stale := []string{
		filepath.Join(dirs[0], "ba-txmap-disk0-1790000000000000000-1"),
		filepath.Join(dirs[0], "ba-txmap-shadow-disk0-1790000000000000000-1"),
		filepath.Join(dirs[1], "ba-txmap-reorg-disk1-1790000000000000000-1"),
	}

	for _, dir := range stale {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "000001.vlog"), []byte("x"), 0o600))
	}

	kept := []string{
		filepath.Join(dirs[0], "ba-txmap-notes"),                    // not a disk tx map dir name
		filepath.Join(dirs[0], "other-disk0-1790000000000000000-1"), // someone else's prefix
	}

	for _, dir := range kept {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	keptFile := filepath.Join(dirs[1], "ba-txmap-disk0-1790000000000000000-2")
	require.NoError(t, os.WriteFile(keptFile, []byte("x"), 0o600)) // a file, not a dir

	stp := newSubtreeProcessorWithTxMapDirs(t, dirs)
	require.NotNil(t, stp.diskTxMap, "the processor still creates its own disk tx maps")

	for _, dir := range stale {
		require.NoDirExists(t, dir)
	}

	for _, dir := range kept {
		require.DirExists(t, dir)
	}

	require.FileExists(t, keptFile)

	// Its own freshly created dirs are there.
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)

		own := 0

		for _, e := range entries {
			if e.IsDir() && staleDiskTxMapDirPattern.MatchString(e.Name()) {
				own++
			}
		}

		require.Equal(t, 2, own, "one dir each for the map and its shadow in %s", dir)
	}

	// The helper's cleanup checks nothing but the processor's own dirs is left.
	for _, dir := range kept {
		require.NoError(t, os.RemoveAll(dir))
	}
}
