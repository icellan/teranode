//go:build amd64

package txmetacache

// storeFence and loadFence are no-ops on amd64: under TSO stores are not
// reordered with earlier stores, loads are not reordered with earlier loads,
// and the reservation CAS (LOCK CMPXCHG) is a full fence. The Go compiler does
// not move memory accesses across the atomic operations around these calls.
func storeFence() {}

func loadFence() {}
