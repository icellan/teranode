package subtreevalidation

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The scale-3 incident of 2026-10-26: a 6-minute CATCHINGBLOCKS window dropped every peer subtree
// announced meanwhile, so the next block arrived with 286 subtrees unvalidated and checking it
// meant 300M txmeta reads from the UTXO store. Subtrees skipped while not RUNNING are now kept and
// validated once the node is RUNNING again.

// fsmState is the state the mocked blockchain client reports. Tests change it only while no
// handler goroutine reads it.
type fsmState struct {
	state blockchain.FSMStateType
}

func (f *fsmState) set(state blockchain.FSMStateType) { f.state = state }

type replayedSubtree struct {
	hash    chainhash.Hash
	baseURL string
	peerID  string
}

func newSkippedSubtreeServer(t *testing.T, state *fsmState, max int) (*Server, chan replayedSubtree) {
	t.Helper()
	InitPrometheusMetrics()

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.SubtreeValidation.BlocksOnly = false
	tSettings.SubtreeValidation.SkippedSubtreeBackfillMax = max

	blockchainClient := &blockchain.Mock{}
	blockchainClient.On("GetFSMCurrentState", mock.Anything).Return(&state.state, nil)
	// The RUNNING watcher gives up at once unless a test wires a real wait.
	blockchainClient.On("WaitForFSMtoTransitionToGivenState", mock.Anything, blockchain.FSMStateRUNNING).Return(errors.NewContextCanceledError("no watcher in this test")).Maybe()

	replayed := make(chan replayedSubtree, 16)
	server := &Server{
		logger:           ulogger.TestLogger{},
		settings:         tSettings,
		blockchainClient: blockchainClient,
	}
	server.replaySubtreeFn = func(_ context.Context, hash *chainhash.Hash, baseURL *url.URL, peerID string) error {
		replayed <- replayedSubtree{hash: *hash, baseURL: baseURL.String(), peerID: peerID}
		return nil
	}

	return server, replayed
}

func subtreeMessage(t *testing.T, hash string, peerID string) *kafka.KafkaMessage {
	t.Helper()

	value, err := proto.Marshal(&kafkamessage.KafkaSubtreeTopicMessage{Hash: hash, URL: "http://peer-" + peerID + ":8000", PeerId: peerID})
	require.NoError(t, err)

	return &kafka.KafkaMessage{Value: value}
}

const (
	skippedHashA = "d580e67e847f65c73496a9f1adafacc5f73b4ca9d44fbd0749d6d926914bdcaf"
	skippedHashB = "0000000000000000000000000000000000000000000000000000000000000001"
	skippedHashC = "0000000000000000000000000000000000000000000000000000000000000002"
)

func TestSubtreeMessageHandler_CatchingBlocks_RecordsSubtree(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, replayed := newSkippedSubtreeServer(t, fsm, 16)

	recorded := testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("recorded"))
	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, skippedHashA, "p1")))

	require.Equal(t, recorded+1, testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("recorded")))
	require.Equal(t, 1, server.skippedSubtrees.len())
	require.Empty(t, replayed, "nothing is validated while not RUNNING")
}

// TestSubtreeMessageHandler_Idle_DoesNotRecord: IDLE (an operator STOP) can last days. By then
// the announcing peers may have pruned the subtrees, and a replay that cannot fetch one reports
// the peer, so subtrees announced while IDLE are dropped as before.
func TestSubtreeMessageHandler_Idle_DoesNotRecord(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateIDLE}
	server, _ := newSkippedSubtreeServer(t, fsm, 16)

	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, skippedHashA, "p1")))
	require.Zero(t, server.skippedSubtrees.len())
}

func TestSubtreeMessageHandler_Running_ReplaysSkippedSubtrees(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, replayed := newSkippedSubtreeServer(t, fsm, 16)
	handler := server.subtreeMessageHandler(context.Background())

	require.NoError(t, handler(subtreeMessage(t, skippedHashA, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p2")))

	fsm.set(blockchain.FSMStateRUNNING)

	// The first message handled in RUNNING starts the replay. Its own subtree goes the live way
	// (subtreesHandler), which this test does not observe.
	require.NoError(t, handler(subtreeMessage(t, skippedHashC, "p3")))

	got := map[string]replayedSubtree{}
	for len(got) < 2 {
		select {
		case r := <-replayed:
			got[r.hash.String()] = r
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 skipped subtrees were replayed", len(got))
		}
	}

	require.Equal(t, "http://peer-p1:8000", got[skippedHashA].baseURL)
	require.Equal(t, "p1", got[skippedHashA].peerID)
	require.Equal(t, "p2", got[skippedHashB].peerID)
	require.Eventually(t, func() bool { return server.skippedSubtrees.len() == 0 }, time.Second, 10*time.Millisecond)
}

func TestSubtreeMessageHandler_SkippedSubtreeCapIsEnforced(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 2)
	handler := server.subtreeMessageHandler(context.Background())

	evicted := testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("evicted"))
	recorded := testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("recorded"))

	require.NoError(t, handler(subtreeMessage(t, skippedHashA, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p2")), "a duplicate is not a new entry")
	require.NoError(t, handler(subtreeMessage(t, skippedHashC, "p1")))

	// The newest announcements are the ones the next block needs, so the oldest goes.
	require.Equal(t, []string{skippedHashB, skippedHashC}, skippedHashes(server.skippedSubtrees.take()))
	require.Equal(t, evicted+1, testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("evicted")))
	require.Equal(t, recorded+3, testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("recorded")))
}

// TestSkippedSubtrees_TakeKeepsAnnouncementOrder: parents are announced before the subtrees that
// spend from them, so the replay starts them in announcement order.
func TestSkippedSubtrees_TakeKeepsAnnouncementOrder(t *testing.T) {
	var set skippedSubtrees

	for _, hash := range []string{skippedHashC, skippedHashA, skippedHashB} {
		h, err := chainhash.NewHashFromStr(hash)
		require.NoError(t, err)
		set.record(skippedSubtree{hash: *h}, 16)
	}

	require.Equal(t, []string{skippedHashC, skippedHashA, skippedHashB}, skippedHashes(set.take()))
	require.Zero(t, set.len())
}

func skippedHashes(entries []skippedSubtree) []string {
	hashes := make([]string, 0, len(entries))
	for _, entry := range entries {
		hashes = append(hashes, entry.hash.String())
	}

	return hashes
}

// TestReplaySkippedSubtreesIfRunning: the block notification also starts the replay, so a block
// arriving before any peer subtree message still finds the skipped subtrees being validated.
func TestReplaySkippedSubtreesIfRunning(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, replayed := newSkippedSubtreeServer(t, fsm, 16)

	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, skippedHashA, "p1")))

	server.replaySkippedSubtreesIfRunning(context.Background())
	require.Equal(t, 1, server.skippedSubtrees.len(), "no replay while still catching up")

	fsm.set(blockchain.FSMStateRUNNING)
	server.replaySkippedSubtreesIfRunning(context.Background())

	select {
	case r := <-replayed:
		require.Equal(t, skippedHashA, r.hash.String())
	case <-time.After(2 * time.Second):
		t.Fatal("the skipped subtree was not replayed")
	}
}

// TestReplaySkippedSubtrees_CancelledContextLaunchesNothing: on shutdown the replay stops
// instead of starting every remaining subtree.
func TestReplaySkippedSubtrees_CancelledContextLaunchesNothing(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, replayed := newSkippedSubtreeServer(t, fsm, 16)
	handler := server.subtreeMessageHandler(context.Background())

	require.NoError(t, handler(subtreeMessage(t, skippedHashA, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p1")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fsm.set(blockchain.FSMStateRUNNING)
	server.replaySkippedSubtrees(ctx)

	require.Eventually(t, func() bool { return !server.skippedSubtrees.replaying.Load() }, time.Second, 10*time.Millisecond)
	require.Empty(t, replayed)
}

// TestReplayLimit_LeavesSlotsForLiveSubtrees: the replay takes at most half the peer-subtree
// admission slots, so live subtrees keep the rest.
func TestReplayLimit_LeavesSlotsForLiveSubtrees(t *testing.T) {
	require.Equal(t, 16, replayLimit(32))
	require.Equal(t, 1, replayLimit(1))
	require.Equal(t, 1, replayLimit(2))
	require.Positive(t, replayLimit(0), "an unlimited admission gate still gets a bounded replay")
}

func TestSubtreeMessageHandler_SkippedSubtreeBackfillDisabled(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 0)

	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, skippedHashA, "p1")))
	require.Zero(t, server.skippedSubtrees.len())
}

func TestSubtreeMessageHandler_BlocksOnly_DoesNotRecord(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 16)
	server.settings.SubtreeValidation.BlocksOnly = true

	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, skippedHashA, "p1")))
	require.Zero(t, server.skippedSubtrees.len(), "a blocks-only node never validates peer subtrees, so it keeps none")
}

func TestSubtreeMessageHandler_NotRunning_MalformedIsNotRecordedOrCounted(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 16)

	badHash := prometheusSubtreeKafkaMalformed.WithLabelValues("bad_hash")
	before := testutil.ToFloat64(badHash)

	require.NoError(t, server.subtreeMessageHandler(context.Background())(subtreeMessage(t, "not-a-hex-hash", "p1")))
	require.Zero(t, server.skippedSubtrees.len())
	require.Equal(t, before, testutil.ToFloat64(badHash), "the malformed metric stays a RUNNING-only signal")
}

// TestReplaySkippedSubtrees_FailureIsLoggedNotFatal: a subtree that fails on replay is dropped
// like a live one would be; the others are still replayed.
func TestReplaySkippedSubtrees_FailureIsLoggedNotFatal(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 16)
	handler := server.subtreeMessageHandler(context.Background())

	require.NoError(t, handler(subtreeMessage(t, skippedHashA, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p2")))

	var mu sync.Mutex
	calls := 0
	server.replaySubtreeFn = func(context.Context, *chainhash.Hash, *url.URL, string) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return errors.NewProcessingError("peer gone")
	}

	failed := testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("replay_failed"))

	fsm.set(blockchain.FSMStateRUNNING)
	server.replaySkippedSubtrees(context.Background())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, 2*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(prometheusSkippedSubtrees.WithLabelValues("replay_failed")) == failed+2
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return !server.skippedSubtrees.replaying.Load() }, time.Second, 10*time.Millisecond)
}

// TestSubtreesHandler_LeftRunningWhileQueued_RecordsSubtree: a subtree that waited for an
// admission slot while the node left RUNNING is kept too, not dropped.
func TestSubtreesHandler_LeftRunningWhileQueued_RecordsSubtree(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, _ := newSkippedSubtreeServer(t, fsm, 16)

	hash, err := chainhash.NewHashFromStr(skippedHashA)
	require.NoError(t, err)
	baseURL, err := url.Parse("http://peer-p1:8000")
	require.NoError(t, err)

	require.NoError(t, server.subtreesHandler(context.Background(), hash, baseURL, "p1"))
	require.Equal(t, 1, server.skippedSubtrees.len())
}

// TestSkippedSubtrees_TakeDropsStaleEntries: a catchup can be followed by an operator STOP that
// lasts days. By then the peers may have pruned the subtrees, and a replay that cannot fetch one
// reports the peer, so entries older than skippedSubtreeMaxAge are not replayed.
func TestSkippedSubtrees_TakeDropsStaleEntries(t *testing.T) {
	var set skippedSubtrees

	stale, err := chainhash.NewHashFromStr(skippedHashA)
	require.NoError(t, err)
	fresh, err := chainhash.NewHashFromStr(skippedHashB)
	require.NoError(t, err)

	set.record(skippedSubtree{hash: *stale}, 16)
	set.record(skippedSubtree{hash: *fresh}, 16)

	entry := set.entries[*stale]
	entry.recordedAt = time.Now().Add(-skippedSubtreeMaxAge - time.Minute)
	set.entries[*stale] = entry

	require.Equal(t, []string{skippedHashB}, skippedHashes(set.take()))
}

// TestSkippedSubtrees_ReplayStartsWhenNodeReachesRunning: the first kept subtree starts a watcher
// that replays once the node is RUNNING, with no peer subtree message or block needed. Catchup
// sends RUN only after its last block, and a quiet network may announce nothing before the next
// block, which would then be validated with the kept subtrees still unvalidated.
func TestSkippedSubtrees_ReplayStartsWhenNodeReachesRunning(t *testing.T) {
	fsm := &fsmState{state: blockchain.FSMStateCATCHINGBLOCKS}
	server, replayed := newSkippedSubtreeServer(t, fsm, 16)

	running := make(chan struct{})
	blockchainClient := server.blockchainClient.(*blockchain.Mock)
	blockchainClient.ExpectedCalls = nil
	blockchainClient.On("GetFSMCurrentState", mock.Anything).Return(&fsm.state, nil)
	blockchainClient.On("WaitForFSMtoTransitionToGivenState", mock.Anything, blockchain.FSMStateRUNNING).
		Run(func(mock.Arguments) { <-running }).Return(nil).Once()

	handler := server.subtreeMessageHandler(context.Background())
	require.NoError(t, handler(subtreeMessage(t, skippedHashA, "p1")))
	require.NoError(t, handler(subtreeMessage(t, skippedHashB, "p1")), "one watcher for the whole catchup")

	select {
	case <-replayed:
		t.Fatal("replayed while still catching up")
	case <-time.After(100 * time.Millisecond):
	}

	close(running)

	got := 0
	for got < 2 {
		select {
		case <-replayed:
			got++
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 kept subtrees were replayed after RUNNING", got)
		}
	}

	require.Eventually(t, func() bool { return !server.skippedSubtrees.watching.Load() }, time.Second, 10*time.Millisecond)
}
