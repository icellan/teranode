package mmaphash

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func BenchmarkUpsert(b *testing.B) {
	tbl, err := New(Options{Dir: b.TempDir(), Prefix: "bench", KeySize: 36, ValueSize: 0, Expected: uint64(b.N)})
	if err != nil {
		b.Fatal(err)
	}
	defer tbl.Close()

	key := make([]byte, 36)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.LittleEndian.PutUint64(key[0:8], uint64(i))
		binary.LittleEndian.PutUint64(key[8:16], uint64(i)*0x9e3779b97f4a7c15)
		binary.LittleEndian.PutUint64(key[16:24], uint64(i)*2654435761)
		if _, _, err := tbl.Upsert(key, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTableGrow characterizes the cost of a single grow() — allocate a new
// mmap at 2x and rehash every live entry — at a range of table sizes. grow holds
// growMu exclusively, so this is also the stop-the-world window during which all
// Upsert/Lookup block. The cost is O(live entries): a table holding N entries
// rehashes N slots, so a large-block table (e.g. ~2^28 entries) extrapolates to
// ~64x the entries=2^22 figure here. Population is excluded from the timing;
// only grow() is measured.
func BenchmarkTableGrow(b *testing.B) {
	for _, n := range []uint64{1 << 16, 1 << 18, 1 << 20, 1 << 22} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			key := make([]byte, 36)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				tbl, err := New(Options{Dir: b.TempDir(), Prefix: "growbench", KeySize: 36, ValueSize: 0, Expected: n})
				if err != nil {
					b.Fatal(err)
				}
				for j := uint64(0); j < n; j++ {
					binary.LittleEndian.PutUint64(key[0:8], j)
					binary.LittleEndian.PutUint64(key[8:16], j*0x9e3779b97f4a7c15)
					binary.LittleEndian.PutUint64(key[16:24], j*2654435761)
					if _, _, err := tbl.Upsert(key, 0); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()

				if err := tbl.grow(tbl.gen.Load()); err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				_ = tbl.Close()
			}
		})
	}
}
