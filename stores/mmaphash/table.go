// Package mmaphash provides an off-heap, file-backed, open-addressing hash
// table used by block validation for ephemeral (per-block) membership and
// key/value maps. The backing file is created sparse, mmap'd, and unlinked
// immediately, so it is reclaimed on Close or process exit. There is no
// durability: the table exists only for the lifetime of one block validation.
package mmaphash

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/teranode/errors"
	"golang.org/x/sys/unix"
)

const (
	minSegSlots       = 64    // smallest segment so linear probing has room
	maxSeg            = 4096  // max independently-locked segments
	segTarget         = 65536 // ~entries per segment used to pick segment count
	defaultLoadFactor = 0.5
)

// nextPow2 returns the smallest power of two >= n (and >= 1). Inputs must be <= 2^63; above that the shift loop never terminates (inputs here are bounded by transaction counts).
func nextPow2(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	p := uint64(1)
	for p < n {
		p <<= 1
	}
	return p
}

// clampU64 bounds v to [lo, hi].
func clampU64(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// layout describes the segment geometry of a table.
type layout struct {
	numSeg      uint64 // power of two, the K segments
	slotsPerSeg uint64 // power of two slots in each segment
}

// computeLayout picks segment count and per-segment slot count for the
// expected number of entries and target load factor.
func computeLayout(expected uint64, loadFactor float64) layout {
	if loadFactor <= 0 {
		loadFactor = defaultLoadFactor
	}
	numSeg := clampU64(nextPow2(expected/segTarget), 1, maxSeg)
	// slots needed across all segments to hold expected at loadFactor
	needed := uint64(float64(expected)/loadFactor) + 1
	perSeg := nextPow2((needed + numSeg - 1) / numSeg)
	if perSeg < minSegSlots {
		perSeg = minSegSlots
	}
	return layout{numSeg: numSeg, slotsPerSeg: perSeg}
}

// Options configures a Table.
type Options struct {
	Dir        string  // directory for the backing file (one physical disk)
	Prefix     string  // file name prefix
	KeySize    int     // bytes of key compared for equality (>=16)
	ValueSize  int     // bytes of value (0 for a pure set)
	Expected   uint64  // expected entries in THIS table
	LoadFactor float64 // 0 => defaultLoadFactor
}

type seg struct {
	mu sync.RWMutex
}

// Table is a concurrent, off-heap, open-addressing hash table.
type Table struct {
	data        []byte
	slotSize    int
	keySize     int
	valueSize   int
	slotsPerSeg uint64
	segMask     uint64
	segs        []seg
	count       atomic.Int64
}

// ErrTableFull is returned when a segment has no empty slot (capacity exceeded).
var ErrTableFull = errors.NewProcessingError("mmaphash: table segment full")

// New creates a Table backed by a sparse, immediately-unlinked mmap file.
func New(opts Options) (*Table, error) {
	if opts.KeySize < 16 {
		return nil, errors.NewProcessingError("mmaphash: KeySize must be >= 16, got %d", opts.KeySize)
	}
	if opts.ValueSize < 0 {
		return nil, errors.NewProcessingError("mmaphash: ValueSize must be >= 0, got %d", opts.ValueSize)
	}

	l := computeLayout(opts.Expected, opts.LoadFactor)
	slotSize := 1 + opts.KeySize + opts.ValueSize
	totalSlots := l.numSeg * l.slotsPerSeg
	fileBytes := int64(totalSlots) * int64(slotSize)

	f, err := os.CreateTemp(opts.Dir, opts.Prefix+"-*.mmh")
	if err != nil {
		return nil, errors.NewStorageError("mmaphash: create temp file in %s", opts.Dir, err)
	}
	// Unlink now: the open fd and mmap keep the inode alive; space is reclaimed
	// on Close (munmap + last fd close) or on process exit, even after a crash.
	_ = os.Remove(f.Name())

	if err = f.Truncate(fileBytes); err != nil {
		_ = f.Close()
		return nil, errors.NewStorageError("mmaphash: ftruncate %d bytes", fileBytes, err)
	}

	data, err := unix.Mmap(int(f.Fd()), 0, int(fileBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, errors.NewStorageError("mmaphash: mmap %d bytes", fileBytes, err)
	}
	// Random access pattern: disable readahead so a fault pages in 4K, not 128K.
	_ = unix.Madvise(data, unix.MADV_RANDOM)
	// fd no longer needed; mapping survives close.
	_ = f.Close()

	return &Table{
		data:        data,
		slotSize:    slotSize,
		keySize:     opts.KeySize,
		valueSize:   opts.ValueSize,
		slotsPerSeg: l.slotsPerSeg,
		segMask:     l.numSeg - 1,
		segs:        make([]seg, l.numSeg),
	}, nil
}

// Close unmaps the region, releasing RSS and reclaiming the file's blocks.
func (t *Table) Close() error {
	if t.data == nil {
		return nil
	}
	err := unix.Munmap(t.data)
	t.data = nil
	if err != nil {
		return errors.NewStorageError("mmaphash: munmap", err)
	}
	return nil
}

// Len returns the number of entries inserted.
func (t *Table) Len() int64 { return t.count.Load() }
