package seeder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// writeSidecar writes a "<file>.sha256" sidecar for content, using the
// standard sha256sum layout the blob file store also writes.
func writeSidecar(t *testing.T, filePath string, content []byte) {
	t.Helper()

	sum := sha256.Sum256(content)
	sidecar := hex.EncodeToString(sum[:]) + "  " + filepath.Base(filePath) + "\n"

	require.NoError(t, os.WriteFile(filePath+checksumSidecarExtension, []byte(sidecar), 0o644))
}

// TestVerifyChecksum_MatchingSidecarSucceeds covers case (a): a snapshot file
// with a matching, correct checksum sidecar must verify without error.
func TestVerifyChecksum_MatchingSidecarSucceeds(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "abc123.utxo-set")
	content := []byte("this is the utxo-set snapshot content")

	require.NoError(t, os.WriteFile(filePath, content, 0o644))
	writeSidecar(t, filePath, content)

	err := verifyChecksum(ulogger.TestLogger{}, filePath)
	require.NoError(t, err)
}

// TestVerifyChecksum_CorruptedFileFailsVerification covers case (b): a
// snapshot file whose bytes were corrupted (a bit flip that leaves record
// counts untouched) but whose sidecar still reflects the original content
// must be rejected with a clear error, not silently imported.
func TestVerifyChecksum_CorruptedFileFailsVerification(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "abc123.utxo-set")
	original := []byte("this is the utxo-set snapshot content")

	require.NoError(t, os.WriteFile(filePath, original, 0o644))
	writeSidecar(t, filePath, original)

	// Simulate corruption: flip a single byte in place, leaving the file
	// length (and therefore any record count) unchanged.
	corrupted := append([]byte(nil), original...)
	corrupted[0] ^= 0xFF
	require.NoError(t, os.WriteFile(filePath, corrupted, 0o644))

	err := verifyChecksum(ulogger.TestLogger{}, filePath)
	require.Error(t, err, "corruption must be caught even though the sidecar exists")
	require.Contains(t, err.Error(), "checksum mismatch")
}

// TestVerifyChecksum_NoSidecarSucceedsWithWarning covers case (c): an older
// snapshot (or one from a source that never produced a sidecar) has no
// ".sha256" file — the import must proceed, not fail, with only a warning
// logged.
func TestVerifyChecksum_NoSidecarSucceedsWithWarning(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "abc123.utxo-set")
	content := []byte("this is the utxo-set snapshot content")

	require.NoError(t, os.WriteFile(filePath, content, 0o644))

	// No sidecar written.
	err := verifyChecksum(ulogger.TestLogger{}, filePath)
	require.NoError(t, err, "a missing sidecar must be backward compatible, not fatal")
}

// TestVerifyChecksum_EmptySidecarFails guards against a truncated/empty
// sidecar being silently treated as "no sidecar" rather than as a checksum
// verification failure.
func TestVerifyChecksum_EmptySidecarFails(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "abc123.utxo-set")
	content := []byte("this is the utxo-set snapshot content")

	require.NoError(t, os.WriteFile(filePath, content, 0o644))
	require.NoError(t, os.WriteFile(filePath+checksumSidecarExtension, []byte(""), 0o644))

	err := verifyChecksum(ulogger.TestLogger{}, filePath)
	require.Error(t, err)
}
