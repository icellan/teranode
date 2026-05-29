package model

import (
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/stretchr/testify/require"
)

func newTestDiskParentSpendsMap(t *testing.T) *DiskParentSpendsMap {
	t.Helper()
	m, err := NewDiskParentSpendsMap(DiskParentSpendsMapOptions{
		BasePaths:      []string{t.TempDir()},
		Prefix:         "test-parentspends",
		FilterCapacity: 100_000, // sized for TestDiskParentSpendsMap_ManyEntries (50K inserts)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func makeInpoint(hashIdx, index int) subtreepkg.Inpoint {
	return subtreepkg.Inpoint{
		Hash:  makeHash(hashIdx),
		Index: uint32(index),
	}
}

func TestDiskParentSpendsMap_SetIfNotExists_New(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	ip := makeInpoint(1, 0)
	inserted, err := m.SetIfNotExists(ip)
	require.NoError(t, err)
	require.True(t, inserted)
}

func TestDiskParentSpendsMap_SetIfNotExists_Duplicate(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	ip := makeInpoint(1, 0)
	got1, err := m.SetIfNotExists(ip)
	require.NoError(t, err)
	require.True(t, got1)
	got2, err := m.SetIfNotExists(ip)
	require.NoError(t, err)
	require.False(t, got2)
}

func TestDiskParentSpendsMap_DifferentIndexes(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	ip0 := makeInpoint(1, 0)
	ip1 := makeInpoint(1, 1)

	got, err := m.SetIfNotExists(ip0)
	require.NoError(t, err)
	require.True(t, got)
	got, err = m.SetIfNotExists(ip1)
	require.NoError(t, err)
	require.True(t, got)
	got, err = m.SetIfNotExists(ip0)
	require.NoError(t, err)
	require.False(t, got)
	got, err = m.SetIfNotExists(ip1)
	require.NoError(t, err)
	require.False(t, got)
}

func TestDiskParentSpendsMap_ConcurrentSetIfNotExists(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	const n = 1000
	var wg sync.WaitGroup
	wg.Add(n)
	results := make([]bool, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			inserted, err := m.SetIfNotExists(makeInpoint(idx, 0))
			require.NoError(t, err)
			results[idx] = inserted
		}(i)
	}
	wg.Wait()

	for i, inserted := range results {
		require.True(t, inserted, "unique inpoint %d should have been inserted", i)
	}
}

func TestDiskParentSpendsMap_ConcurrentDuplicateDetection(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	ip := makeInpoint(42, 0)
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make([]bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			inserted, err := m.SetIfNotExists(ip)
			require.NoError(t, err)
			results[idx] = inserted
		}(i)
	}
	wg.Wait()

	insertCount := 0
	for _, inserted := range results {
		if inserted {
			insertCount++
		}
	}
	require.Equal(t, 1, insertCount, "exactly one goroutine should succeed")
}

func TestDiskParentSpendsMap_MultiDisk(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	m, err := NewDiskParentSpendsMap(DiskParentSpendsMapOptions{
		BasePaths:      dirs,
		Prefix:         "test-multi-ps",
		FilterCapacity: 10_000,
	})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	const n = 500
	for i := 0; i < n; i++ {
		got, err := m.SetIfNotExists(makeInpoint(i, 0))
		require.NoError(t, err)
		require.True(t, got)
	}

	// all duplicates should be detected
	for i := 0; i < n; i++ {
		got, err := m.SetIfNotExists(makeInpoint(i, 0))
		require.NoError(t, err)
		require.False(t, got)
	}
}

func TestDiskParentSpendsMap_ImplementsInterface(t *testing.T) {
	var _ ParentSpendsMap = (*DiskParentSpendsMap)(nil)
	var _ ParentSpendsMap = (*SplitSyncedParentMap)(nil)
}

func TestDiskParentSpendsMap_NoPaths(t *testing.T) {
	_, err := NewDiskParentSpendsMap(DiskParentSpendsMapOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one base path")
}

func TestDiskParentSpendsMap_ManyEntries(t *testing.T) {
	m := newTestDiskParentSpendsMap(t)

	const n = 50_000
	for i := 0; i < n; i++ {
		// Use different hash values to get good distribution across shards
		var h chainhash.Hash
		h[0] = byte(i >> 24)
		h[1] = byte(i >> 16)
		h[2] = byte(i >> 8)
		h[3] = byte(i)
		h[4] = byte(i >> 12) // extra entropy

		ip := subtreepkg.Inpoint{Hash: h, Index: uint32(i % 10)}
		got, err := m.SetIfNotExists(ip)
		require.NoError(t, err)
		require.True(t, got, "insert failed at %d", i)
	}

	// verify count
	require.Equal(t, int64(n), m.Stats().Entries)
}

func TestDiskParentSpendsMap_Stats(t *testing.T) {
	m, err := NewDiskParentSpendsMap(DiskParentSpendsMapOptions{
		BasePaths:      []string{t.TempDir()},
		Prefix:         "test-stats-ps",
		FilterCapacity: 10_000,
	})
	require.NoError(t, err)

	stats := m.Stats()
	require.Equal(t, int64(0), stats.Entries)
	require.Equal(t, int64(0), stats.FilterMemBytes, "mmap impl has no in-RAM filter")
	require.Equal(t, int64(0), stats.DiskBytesWritten)

	const n = 500
	for i := 0; i < n; i++ {
		got, err := m.SetIfNotExists(makeInpoint(i, 0))
		require.NoError(t, err)
		require.True(t, got)
	}

	// Close is still valid to call; mmap tables flush on close.
	require.NoError(t, m.Close())

	stats = m.Stats()
	require.Equal(t, int64(n), stats.Entries)
	require.Equal(t, int64(0), stats.FilterMemBytes, "mmap impl has no in-RAM filter")
	// Each entry = 36B inpoint key + 1B slot marker = 37B
	require.Equal(t, int64(n*37), stats.DiskBytesWritten)
}
