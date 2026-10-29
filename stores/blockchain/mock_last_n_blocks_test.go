package blockchain

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMockStore_GetLastNBlocksNonPositiveN: a count of zero or less returns no blocks
// instead of panicking on a negative slice capacity.
func TestMockStore_GetLastNBlocksNonPositiveN(t *testing.T) {
	m := NewMockStore()

	for _, n := range []int64{0, -1} {
		infos, err := m.GetLastNBlocks(context.Background(), n, true, 0)
		require.NoError(t, err)
		require.Empty(t, infos)
	}
}
