//go:build !linux

package txmetacache

// adviseHugePages is a no-op where transparent huge pages are not available.
func adviseHugePages(_ []byte) {}
