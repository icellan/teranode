package subtreevalidation

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/txmetacache"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/stretchr/testify/require"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestTxMetaTombstones_RemembersDeletesForAtLeastTheWindow(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	ts := newTxMetaTombstones(time.Minute, clock.now)

	deleted := chainhash.HashH([]byte("deleted"))
	other := chainhash.HashH([]byte("other"))

	require.False(t, ts.contains(deleted[:]), "empty set holds nothing")

	ts.add([]chainhash.Hash{deleted})

	require.True(t, ts.contains(deleted[:]))
	require.False(t, ts.contains(other[:]))

	// Still remembered just before one full window has passed, and across the
	// first rotation.
	clock.t = clock.t.Add(59 * time.Second)
	require.True(t, ts.contains(deleted[:]))

	clock.t = clock.t.Add(30 * time.Second)
	require.True(t, ts.contains(deleted[:]), "an entry survives the first rotation")

	// Gone after two windows.
	clock.t = clock.t.Add(2 * time.Minute)
	require.False(t, ts.contains(deleted[:]), "an entry is forgotten after two windows")
	require.Zero(t, ts.size.Load(), "an expired set is empty again, so the ADD path is back on its fast path")
}

func TestTxMetaTombstones_NilIsEmpty(t *testing.T) {
	var ts *txMetaTombstones

	h := chainhash.HashH([]byte("x"))

	ts.add([]chainhash.Hash{h})
	require.False(t, ts.contains(h[:]))
}

// TestTxmetaHandler_LateAddAfterDeleteIsNotCached pins the reordering case: a
// DELETE from block assembly overtakes the validator's ADD for the same tx (they
// travel on different partitions). The late ADD must not re-cache the demoted tx
// as non-conflicting; other ADDs in the same message are still applied.
func TestTxmetaHandler_LateAddAfterDeleteIsNotCached(t *testing.T) {
	h := newTxmetaE2EHarness(t, "v1", 32)
	defer h.close()

	h.server.txMetaTombstones = newTxMetaTombstones(txMetaTombstoneWindow, time.Now)

	hashes, payloads := makeUniqueHashes(t, 2, 64)
	demoted, unrelated := hashes[0], hashes[1]

	require.NoError(t, h.server.txmetaHandler(h.ctx, &kafka.KafkaMessage{Value: txmetacache.EncodeDeleteBatch([]chainhash.Hash{demoted})}))

	h.publishV1(t, hashes, payloads)
	h.waitProcessed(t, 2, 5*time.Second)

	_, cached := h.cache.GetRaw(demoted[:])
	require.False(t, cached, "an ADD arriving after the DELETE for the same tx must not re-cache it")

	_, cached = h.cache.GetRaw(unrelated[:])
	require.True(t, cached, "other ADDs in the message are still applied")
}
