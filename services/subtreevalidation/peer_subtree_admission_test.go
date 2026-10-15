package subtreevalidation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// After a block, a backlog of peer subtrees arrives at once. Each one admitted
// allocates its per-subtree working set (~1 GiB for 1M transactions) before it
// waits for a cache-probe worker, so the gate must bound how many hold one.
func TestAdmissionGate_BoundsConcurrentHolders(t *testing.T) {
	const limit = 3

	var (
		gate    admissionGate
		holding atomic.Int32
		peak    atomic.Int32
		wg      sync.WaitGroup
	)

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			release, err := gate.acquire(context.Background(), limit, noBypass)
			require.NoError(t, err)

			n := holding.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}

			time.Sleep(2 * time.Millisecond)
			holding.Add(-1)
			release()
		}()
	}

	wg.Wait()

	require.LessOrEqual(t, peak.Load(), int32(limit))
	require.Positive(t, peak.Load())
}

func noBypass() bool { return false }

// Block validation can need a subtree whose peer goroutine is queued at the
// gate (it holds the subtree's quorum lock). Once the subtree is a priority
// check, the waiter must go ahead without a slot instead of making the block
// wait for the queue.
func TestAdmissionGate_BypassReleasesAWaiter(t *testing.T) {
	var gate admissionGate

	release, err := gate.acquire(context.Background(), 1, noBypass)
	require.NoError(t, err)

	defer release()

	var priority atomic.Bool

	done := make(chan error, 1)

	go func() {
		r, err := gate.acquire(context.Background(), 1, priority.Load)
		if err == nil {
			r()
		}
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("acquired a slot while the gate was full")
	case <-time.After(50 * time.Millisecond):
	}

	priority.Store(true)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a priority subtree stayed queued at the gate")
	}

	// The bypassing waiter took no slot: the gate is still exactly full.
	require.Len(t, gate.slots, 1)
}

func TestAdmissionGate_NonPositiveLimitDisablesIt(t *testing.T) {
	var gate admissionGate

	releases := make([]func(), 0, 100)

	for i := 0; i < 100; i++ {
		release, err := gate.acquire(context.Background(), 0, noBypass)
		require.NoError(t, err)

		releases = append(releases, release)
	}

	for _, release := range releases {
		release()
	}
}

func TestAdmissionGate_CancelWhileWaiting(t *testing.T) {
	var gate admissionGate

	release, err := gate.acquire(context.Background(), 1, noBypass)
	require.NoError(t, err)

	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = gate.acquire(ctx, 1, noBypass)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrContextCanceled), "got %v", err)
}

// A peer subtree waits for admission before it allocates its working set; a
// block's subtree (FromBlock) never waits on the peer gate.
func TestValidateSubtreeInternal_PeerSubtreeWaitsForAdmission(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.SubtreeValidation.PeerSubtreeConcurrency = 1

	server := &Server{
		logger:   ulogger.TestLogger{},
		settings: tSettings,
	}

	release, err := server.peerSubtreeAdmission.acquire(context.Background(), 1, noBypass)
	require.NoError(t, err)

	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	v := ValidateSubtree{
		SubtreeHash: chainhash.HashH([]byte("peer subtree")),
		TxHashes:    []chainhash.Hash{chainhash.HashH([]byte("tx"))},
		peerSubtree: true,
	}

	_, err = server.ValidateSubtreeInternal(ctx, v, 1, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrContextCanceled), "a peer subtree must wait at the gate, got %v", err)
}
