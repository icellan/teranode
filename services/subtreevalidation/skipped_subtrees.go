package subtreevalidation

import (
	"context"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
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

// skippedSubtrees keeps the peer subtrees announced while the node was not RUNNING, so they are
// validated once it is RUNNING again instead of lost. Dropping them left the next block with every
// subtree announced during a catchup unvalidated (286 subtrees and 300M txmeta reads from the UTXO
// store on scale-3, 2026-10-26). The zero value is ready to use.
type skippedSubtrees struct {
	mu      sync.Mutex
	entries map[chainhash.Hash]skippedSubtree

	// replaying is set while a replay runs, so only one runs at a time.
	replaying atomic.Bool
}

// record keeps entry unless max entries are already kept or max is 0 or less. A subtree already
// kept is not a new entry. It reports whether the subtree is kept.
func (s *skippedSubtrees) record(entry skippedSubtree, max int) bool {
	if max <= 0 {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[entry.hash]; ok {
		return true
	}

	if len(s.entries) >= max {
		return false
	}

	if s.entries == nil {
		s.entries = make(map[chainhash.Hash]skippedSubtree)
	}

	s.entries[entry.hash] = entry

	return true
}

// take removes and returns every kept subtree.
func (s *skippedSubtrees) take() []skippedSubtree {
	s.mu.Lock()
	defer s.mu.Unlock()

	taken := make([]skippedSubtree, 0, len(s.entries))
	for _, entry := range s.entries {
		taken = append(taken, entry)
	}

	s.entries = nil

	return taken
}

func (s *skippedSubtrees) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.entries)
}

// recordSkippedSubtreeMessage keeps a peer subtree announced while the node is not RUNNING. A
// malformed message is ignored without the malformed-message metric, which stays a RUNNING-only
// signal. A blocks-only node never validates peer subtrees, so it keeps none.
func (u *Server) recordSkippedSubtreeMessage(value []byte) {
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

	u.recordSkippedSubtree(hash, baseURL, kafkaMsg.PeerId)
}

func (u *Server) recordSkippedSubtree(hash *chainhash.Hash, baseURL *url.URL, peerID string) {
	if u.skippedSubtrees.record(skippedSubtree{hash: *hash, baseURL: baseURL, peerID: peerID}, u.settings.SubtreeValidation.SkippedSubtreeBackfillMax) {
		prometheusSkippedSubtrees.WithLabelValues("recorded").Inc()
		return
	}

	if u.settings.SubtreeValidation.SkippedSubtreeBackfillMax > 0 {
		prometheusSkippedSubtrees.WithLabelValues("dropped_over_cap").Inc()
	}
}

// replaySkippedSubtrees validates the kept subtrees in the background through the normal
// peer-subtree path (subtreesHandler), so the admission limit, the quorum lock (which skips a
// subtree validated meanwhile, for example by a block) and the RUNNING check all apply. A subtree
// that meets a non-RUNNING node again is kept again by subtreesHandler. It returns at once, and
// does nothing while a replay is running or nothing is kept.
func (u *Server) replaySkippedSubtrees(ctx context.Context) {
	if u.skippedSubtrees.len() == 0 || !u.skippedSubtrees.replaying.CompareAndSwap(false, true) {
		return
	}

	entries := u.skippedSubtrees.take()

	go func() {
		defer u.skippedSubtrees.replaying.Store(false)

		u.logger.Infof("[replaySkippedSubtrees] validating %d peer subtrees announced while the node was not RUNNING", len(entries))

		limit := u.settings.SubtreeValidation.PeerSubtreeConcurrency
		if limit <= 0 {
			limit = runtime.NumCPU()
		}

		g, gCtx := errgroup.WithContext(ctx)
		g.SetLimit(limit)

		for _, entry := range entries {
			g.Go(func() error {
				prometheusSkippedSubtrees.WithLabelValues("replayed").Inc()

				if err := u.replaySubtree(gCtx, &entry.hash, entry.baseURL, entry.peerID); err != nil && !errors.Is(err, errors.ErrSubtreeExists) {
					u.logger.Warnf("[replaySkippedSubtrees] subtree %s from %s: %v", entry.hash.String(), entry.peerID, err)
				}

				// Like a live peer subtree, a failed one is not retried: block validation
				// validates it if a block needs it.
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
