package subtreevalidation

import (
	"context"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

// skippedSubtree is a peer subtree announced while the node was not RUNNING.
type skippedSubtree struct {
	hash    chainhash.Hash
	baseURL *url.URL
	peerID  string
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

	s.entries[entry.hash] = entry
	s.order = append(s.order, entry.hash)

	return true, evicted
}

// take removes and returns every kept subtree, oldest first.
func (s *skippedSubtrees) take() []skippedSubtree {
	s.mu.Lock()
	defer s.mu.Unlock()

	taken := make([]skippedSubtree, 0, len(s.order))
	for _, hash := range s.order {
		taken = append(taken, s.entries[hash])
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
func (u *Server) recordSkippedSubtreeMessage(state blockchain.FSMStateType, value []byte) {
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

	u.recordSkippedSubtree(state, hash, baseURL, kafkaMsg.PeerId)
}

// recordSkippedSubtree keeps a peer subtree that was not validated because the node is catching
// up. Only CATCHINGBLOCKS: IDLE (an operator STOP) can last days, by which time the peers may have
// pruned the subtree, and a replay that cannot fetch it reports the peer.
func (u *Server) recordSkippedSubtree(state blockchain.FSMStateType, hash *chainhash.Hash, baseURL *url.URL, peerID string) {
	if state != blockchain.FSMStateCATCHINGBLOCKS {
		return
	}

	added, evicted := u.skippedSubtrees.record(skippedSubtree{hash: *hash, baseURL: baseURL, peerID: peerID}, u.settings.SubtreeValidation.SkippedSubtreeBackfillMax)
	if added {
		prometheusSkippedSubtrees.WithLabelValues("recorded").Inc()
	}

	if evicted {
		prometheusSkippedSubtrees.WithLabelValues("evicted").Inc()
	}
}

// replayLimit is how many kept subtrees are validated at once: half the peer-subtree admission
// slots, so live peer subtrees keep the other half.
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

		g, gCtx := errgroup.WithContext(ctx)
		g.SetLimit(replayLimit(u.settings.SubtreeValidation.PeerSubtreeConcurrency))

		for _, entry := range entries {
			if gCtx.Err() != nil {
				break
			}

			g.Go(func() error {
				err := u.replaySubtree(gCtx, &entry.hash, entry.baseURL, entry.peerID)

				switch {
				case err == nil, errors.Is(err, errors.ErrSubtreeExists):
					prometheusSkippedSubtrees.WithLabelValues("replayed").Inc()
				case gCtx.Err() != nil || errors.Is(err, errors.ErrContextCanceled):
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
