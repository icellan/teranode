// Package mmaphash provides an off-heap, file-backed, open-addressing hash
// table used by block validation for ephemeral (per-block) membership and
// key/value maps. The backing file is created sparse, mmap'd, and unlinked
// immediately, so it is reclaimed on Close or process exit. There is no
// durability: the table exists only for the lifetime of one block validation.
package mmaphash

import (
	"bytes"
	"encoding/binary"
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
// Uses the threshold-exceeded code so callers and tests can discriminate it via
// errors.Is from generic processing errors.
var ErrTableFull = errors.NewThresholdExceededError("mmaphash: table segment full")

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
	// If unlink fails we cannot honour the ephemeral/crash-safe contract, so fail.
	if rmErr := os.Remove(f.Name()); rmErr != nil {
		_ = f.Close()
		return nil, errors.NewStorageError("mmaphash: unlink temp file %s", f.Name(), rmErr)
	}

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

// locate derives the segment index and the in-segment start bucket from the
// key. Keys are uniformly-random hash output, so raw bytes are used as the
// hash. Disjoint byte windows avoid correlation between disk routing (done by
// the caller on key[0:2]), segment selection (key[8:16]), and bucket selection
// (key[0:8]).
func (t *Table) locate(key []byte) (segIdx, bucket uint64) {
	segHash := binary.LittleEndian.Uint64(key[8:16])
	bucketHash := binary.LittleEndian.Uint64(key[0:8])
	segIdx = segHash & t.segMask
	bucket = bucketHash & (t.slotsPerSeg - 1)
	return segIdx, bucket
}

// probe scans segment segIdx starting at the start bucket. It returns the byte
// offset of the matching slot (found=true) or the first empty slot
// (found=false). full=true means the whole segment was scanned with no empty
// slot. Probing wraps within the segment only.
func (t *Table) probe(segIdx, start uint64, key []byte) (off int, found, full bool) {
	base := segIdx * t.slotsPerSeg
	mask := t.slotsPerSeg - 1
	for i := uint64(0); i < t.slotsPerSeg; i++ {
		local := (start + i) & mask
		o := int(base+local) * t.slotSize
		if t.data[o] == 0 { // empty
			return o, false, false
		}
		if bytes.Equal(t.data[o+1:o+1+t.keySize], key) {
			return o, true, false
		}
	}
	return 0, false, true
}

func (t *Table) readValue(off int) uint64 {
	if t.valueSize == 0 {
		return 0
	}
	return binary.LittleEndian.Uint64(t.data[off+1+t.keySize : off+1+t.keySize+8])
}

func (t *Table) writeSlot(off int, key []byte, value uint64) {
	copy(t.data[off+1:off+1+t.keySize], key)
	if t.valueSize >= 8 {
		binary.LittleEndian.PutUint64(t.data[off+1+t.keySize:off+1+t.keySize+8], value)
	}
	t.data[off] = 1 // publish occupied state last
}

// Upsert inserts key (with value) if absent. Returns (existingOrNewValue,
// inserted, error). inserted=false with no error means the key was already
// present and the returned value is the stored one. ErrTableFull means the
// segment is full.
func (t *Table) Upsert(key []byte, value uint64) (uint64, bool, error) {
	segIdx, start := t.locate(key)
	s := &t.segs[segIdx]
	s.mu.Lock()
	defer s.mu.Unlock()

	off, found, full := t.probe(segIdx, start, key)
	if full {
		return 0, false, ErrTableFull
	}
	if found {
		return t.readValue(off), false, nil
	}
	t.writeSlot(off, key, value)
	t.count.Add(1)
	return value, true, nil
}

// Lookup returns (value, found).
func (t *Table) Lookup(key []byte) (uint64, bool, error) {
	segIdx, start := t.locate(key)
	s := &t.segs[segIdx]
	s.mu.RLock()
	defer s.mu.RUnlock()

	off, found, _ := t.probe(segIdx, start, key)
	if !found {
		return 0, false, nil
	}
	return t.readValue(off), true, nil
}
