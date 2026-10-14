//go:build arm64

package txmetacache

// storeFence orders every earlier store (the reservation CAS included) before
// every later store. A writer issues it once after reserving ring space, so
// none of its record stores can become visible to a reader before the
// reservation that lets the reader detect the overwrite.
//
//go:noescape
func storeFence()

// loadFence orders every earlier load before every later load. A reader issues
// it after copying a record and before re-reading tail, so the copy cannot be
// performed after the check that validates it.
//
//go:noescape
func loadFence()
