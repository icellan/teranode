package subtreevalidation

import (
	"context"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

// skippedSubtreeMaxAge is how old a kept subtree may be and still be replayed. A catchup can be
// followed by an operator STOP that lasts days; by then the peers may have pruned the subtree, and
// a replay that cannot fetch it reports the peer.
const skippedSubtreeMaxAge = 30 * time.Minute

// skippedSubtree is a peer subtree announced while the node was catching up.
type skippedSubtree struct {
	hash       chainhash.Hash
	baseURL    *url.URL
	peerID     string
	recordedAt time.Time
}

// skippedSubtrees keeps the peer subtrees announced while the node was catching up, so they are
// validated once it is RUNNING again instead of lost. Dropping them left the next block with every
// subtree announced during a catchup unvalidated (286 subtrees and 300M txmeta reads from the UTXO
// store on scale-3, 2026-10-26). Entries keep announcement order; when full, the oldest goes,
// since the newest announcements are the ones the next block needs. The zero value is ready to use.
type skippedSubtrees struct {
	mu      sync.Mutex
	entries map[chainhash.Hash]skippedSubtree
	order   []chainhash.Hash // announcement order, oldest first

	// replaying is set while a replay runs, so only one runs at a time.
	replaying atomic.Bool

	// watching is set while a goroutine waits for RUNNING to start the replay.
	watching atomic.Bool
}

// record keeps entry, evicting the oldest entry when max are already kept. A subtree already kept
// is left as it is. It reports whether entry is a new entry and whether one was evicted for it.
// With max 0 or less nothing is kept.
func (s *skippedSubtrees) record(entry skippedSubtree, max int) (added, evicted bool) {
	if max <= 0 {
		return false, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[entry.hash]; ok {
		return false, false
	}

	if s.entries == nil {
		s.entries = make(map[chainhash.Hash]skippedSubtree)
	}

	for len(s.order) >= max {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
		evicted = true
	}

	entry.recordedAt = time.Now()
	s.entries[entry.hash] = entry
	s.order = append(s.order, entry.hash)

	return true, evicted
}

// take removes every kept subtree and returns those younger than skippedSubtreeMaxAge, oldest
// first.
func (s *skippedSubtrees) take() []skippedSubtree {
	s.mu.Lock()
	defer s.mu.Unlock()

	taken := make([]skippedSubtree, 0, len(s.order))
	for _, hash := range s.order {
		if entry := s.entries[hash]; time.Since(entry.recordedAt) <= skippedSubtreeMaxAge {
			taken = append(taken, entry)
		}
	}

	s.entries = nil
	s.order = nil

	return taken
}

func (s *skippedSubtrees) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.order)
}

// recordSkippedSubtreeMessage keeps a peer subtree announced while the node is catching up. A
// malformed message is ignored without the malformed-message metric, which stays a RUNNING-only
// signal. A blocks-only node never validates peer subtrees, so it keeps none.
func (u *Server) recordSkippedSubtreeMessage(ctx context.Context, state blockchain.FSMStateType, value []byte) {
	if u.settings.SubtreeValidation.BlocksOnly {
		return
	}

	var kafkaMsg kafkamessage.KafkaSubtreeTopicMessage
	if err := proto.Unmarshal(value, &kafkaMsg); err != nil {
		return
	}

	hash, err := chainhash.NewHashFromStr(kafkaMsg.Hash)
	if err != nil {
		return
	}

	baseURL, err := url.Parse(kafkaMsg.URL)
	if err != nil {
		return
	}

	u.recordSkippedSubtree(ctx, state, hash, baseURL, kafkaMsg.PeerId)
}

// recordSkippedSubtree keeps a peer subtree that was not validated because the node is catching
// up. Only CATCHINGBLOCKS: IDLE (an operator STOP) can last days, by which time the peers may have
// pruned the subtree, and a replay that cannot fetch it reports the peer. The first kept subtree
// starts a watcher that replays once the node is RUNNING.
func (u *Server) recordSkippedSubtree(ctx context.Context, state blockchain.FSMStateType, hash *chainhash.Hash, baseURL *url.URL, peerID string) {
	if state != blockchain.FSMStateCATCHINGBLOCKS {
		return
	}

	added, evicted := u.skippedSubtrees.record(skippedSubtree{hash: *hash, baseURL: baseURL, peerID: peerID}, u.settings.SubtreeValidation.SkippedSubtreeBackfillMax)
	if added {
		prometheusSkippedSubtrees.WithLabelValues("recorded").Inc()
		u.watchForRunning(ctx)
	}

	if evicted {
		prometheusSkippedSubtrees.WithLabelValues("evicted").Inc()
	}
}

// watchForRunning starts, unless one is already waiting, a goroutine that replays the kept
// subtrees as soon as the node is RUNNING. Catchup sends RUN only after its last block, and the
// blockchain client does not pass FSM notifications to subscribers, so without it the replay would
// wait for the next peer subtree message or block; on a quiet network that is the next block,
// validated with the kept subtrees still unvalidated.
func (u *Server) watchForRunning(ctx context.Context) {
	if !u.skippedSubtrees.watching.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer u.skippedSubtrees.watching.Store(false)

		if err := u.blockchainClient.WaitForFSMtoTransitionToGivenState(ctx, blockchain.FSMStateRUNNING); err != nil {
			return
		}

		u.replaySkippedSubtrees(ctx)
	}()
}

// replayLimit is how many kept subtrees are validated at once: half the peer-subtree admission
// slots (at least one), so live peer subtrees keep the rest.
func replayLimit(peerSubtreeConcurrency int) int {
	if peerSubtreeConcurrency <= 0 {
		peerSubtreeConcurrency = runtime.NumCPU()
	}

	return max(1, peerSubtreeConcurrency/2)
}

// replaySkippedSubtreesIfRunning starts a replay when subtrees are kept and the node is RUNNING.
// The block notification calls it, so a block that arrives before any peer subtree message still
// sets the replay going.
func (u *Server) replaySkippedSubtreesIfRunning(ctx context.Context) {
	if u.skippedSubtrees.len() == 0 {
		return
	}

	state, err := u.blockchainClient.GetFSMCurrentState(ctx)
	if err != nil || state == nil || *state != blockchain.FSMStateRUNNING {
		return
	}

	u.replaySkippedSubtrees(ctx)
}

// replaySkippedSubtrees validates the kept subtrees in the background, in announcement order,
// through the normal peer-subtree path (subtreesHandler), so the admission limit, the quorum lock
// (which skips a subtree validated meanwhile, for example by a block) and the RUNNING check all
// apply. A subtree that meets a catching-up node again is kept again by subtreesHandler. It
// returns at once, and does nothing while a replay is running or nothing is kept.
func (u *Server) replaySkippedSubtrees(ctx context.Context) {
	if u.skippedSubtrees.len() == 0 || !u.skippedSubtrees.replaying.CompareAndSwap(false, true) {
		return
	}

	entries := u.skippedSubtrees.take()

	go func() {
		defer u.skippedSubtrees.replaying.Store(false)

		u.logger.Infof("[replaySkippedSubtrees] validating %d peer subtrees announced while the node was catching up", len(entries))

		// A plain group on ctx, not errgroup.WithContext: a subtree kept again during the replay
		// starts a watcher with this ctx, which must outlive the replay.
		var g errgroup.Group

		g.SetLimit(replayLimit(u.settings.SubtreeValidation.PeerSubtreeConcurrency))

		for _, entry := range entries {
			if ctx.Err() != nil {
				break
			}

			g.Go(func() error {
				err := u.replaySubtree(ctx, &entry.hash, entry.baseURL, entry.peerID)

				switch {
				case err == nil, errors.Is(err, errors.ErrSubtreeExists):
					prometheusSkippedSubtrees.WithLabelValues("replayed").Inc()
				case ctx.Err() != nil:
					// shutting down
				default:
					// Like a live peer subtree, a failed one is not retried: block validation
					// validates it if a block needs it.
					prometheusSkippedSubtrees.WithLabelValues("replay_failed").Inc()
					u.logger.Warnf("[replaySkippedSubtrees] subtree %s from %s: %v", entry.hash.String(), entry.peerID, err)
				}

				return nil
			})
		}

		_ = g.Wait()
	}()
}

func (u *Server) replaySubtree(ctx context.Context, hash *chainhash.Hash, baseURL *url.URL, peerID string) error {
	if u.replaySubtreeFn != nil {
		return u.replaySubtreeFn(ctx, hash, baseURL, peerID)
	}

	return u.subtreesHandler(ctx, hash, baseURL, peerID)
}
