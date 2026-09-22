package blockvalidation

import (
	"path/filepath"
	"sync"
	"testing"

	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// warnCountingLogger counts Warnf calls so the test can assert the fallback log
// is rate-limited rather than emitted per subtree.
type warnCountingLogger struct {
	ulogger.TestLogger

	mu    sync.Mutex
	warns int
}

func (l *warnCountingLogger) Warnf(format string, args ...interface{}) {
	l.mu.Lock()
	l.warns++
	l.mu.Unlock()

	l.TestLogger.Warnf(format, args...)
}

func (l *warnCountingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.warns
}

// TestSubtreeFromBytesWithMmap_FallbackIsRateLimitedAndCounted pins the review
// note that the mmap-to-heap fallback warning had neither a rate limit nor a
// metric. subtreeFromBytesWithMmap runs once per subtree load, so a directory
// that fills up mid-catchup emitted one warning line per subtree of every block
// — volume that gets filtered out of a log pipeline — while nothing counted the
// events, leaving the node silently running heap-only with nothing to alert on.
func TestSubtreeFromBytesWithMmap_FallbackIsRateLimitedAndCounted(t *testing.T) {
	initPrometheusMetrics()

	// The gate is process-wide, so reset it for this test.
	subtreeMmapFallbackLogOnce = sync.Once{}

	subtree, err := subtreepkg.NewTreeByLeafCount(2)
	require.NoError(t, err)
	require.NoError(t, subtree.AddCoinbaseNode())

	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)

	// A non-existent directory makes the mmap allocator's os.CreateTemp fail,
	// which is the runtime condition (disk full, permissions changed) the
	// fallback exists for.
	badDir := filepath.Join(t.TempDir(), "does-not-exist")

	logger := &warnCountingLogger{}

	const loads = 25

	before := testutil.ToFloat64(prometheusBlockValidationSubtreeMmapFallback)

	for i := 0; i < loads; i++ {
		st, err := subtreeFromBytesWithMmap(logger, subtreeBytes, badDir)
		require.NoError(t, err, "the heap fallback must still produce a usable subtree")
		require.False(t, st.IsMmapBacked())
	}

	require.Equal(t, 1, logger.count(), "the fallback must log once, not once per subtree")

	after := testutil.ToFloat64(prometheusBlockValidationSubtreeMmapFallback)
	require.Equal(t, float64(loads), after-before, "every fallback must be counted, so the degraded state is alertable")
}
