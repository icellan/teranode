package txmetacache

import (
	"sync"

	"github.com/cespare/xxhash/v2"
)

// writeBackLanes partitions the cache buckets for the BatchDecorate write-back
// the way the validator partitions the txmeta Kafka topic (see
// sendTxMetaBatchV2 in services/validator): each lane owns a contiguous,
// disjoint range of buckets. With 256 lanes the ranges match the 256 txmeta
// partitions, so a write-back lane competes for bucket locks with at most one
// Kafka partition writer.
//
// Each lane has at most one writer at a time. Under a miss burst thousands of
// BatchDecorate calls complete together; without lanes every one of them
// wrote into the cache at once and the Kafka txmeta handler, one waiter among
// thousands on every bucket lock it needed, fell behind for good.
const writeBackLanes = min(256, BucketsCount)

const bucketsPerWriteBackLane = BucketsCount / writeBackLanes

func writeBackLaneOf(key []byte) int {
	return int(xxhash.Sum64(key) % BucketsCount / bucketsPerWriteBackLane)
}

// writeBackRequest is one caller's share of a lane: a contiguous slice of the
// caller's keys/values that all hash into the lane.
type writeBackRequest struct {
	keys   [][]byte
	values [][]byte
	err    error
	wg     *sync.WaitGroup
}

// writeBackLane group-commits the requests queued for its bucket range. The
// first caller to find the lane idle becomes its writer and keeps writing
// everything queued, its own request and other callers', until the queue is
// empty. No goroutines are started.
type writeBackLane struct {
	mu      sync.Mutex
	pending []*writeBackRequest
	spare   []*writeBackRequest
	writing bool

	// Merge scratch, only touched by the lane's current writer.
	keys   [][]byte
	values [][]byte
}

// writeBack stores the BatchDecorate entries, batched per lane, and returns
// once every entry has been written into the cache. Callers rely on that: the
// values live in pooled buffers that are recycled after BatchDecorate returns.
func (t *TxMetaCache) writeBack(keys, values [][]byte) error {
	laneOf := make([]uint16, len(keys))

	var counts [writeBackLanes]int

	for i, key := range keys {
		lane := writeBackLaneOf(key)
		laneOf[i] = uint16(lane) //nolint:gosec // lane < writeBackLanes <= 256
		counts[lane]++
	}

	// Reorder the entries by lane so each request is a sub-slice.
	var starts [writeBackLanes]int

	nonEmpty := 0
	offset := 0

	for lane, n := range counts {
		starts[lane] = offset
		offset += n

		if n > 0 {
			nonEmpty++
		}
	}

	sortedKeys := make([][]byte, len(keys))
	sortedValues := make([][]byte, len(values))
	next := starts

	for i, lane := range laneOf {
		sortedKeys[next[lane]] = keys[i]
		sortedValues[next[lane]] = values[i]
		next[lane]++
	}

	requests := make([]writeBackRequest, 0, nonEmpty)

	var wg sync.WaitGroup

	for lane, n := range counts {
		if n == 0 {
			continue
		}

		requests = append(requests, writeBackRequest{
			keys:   sortedKeys[starts[lane] : starts[lane]+n],
			values: sortedValues[starts[lane] : starts[lane]+n],
			wg:     &wg,
		})

		wg.Add(1)
		t.lanes[lane].submit(t, &requests[len(requests)-1])
	}

	wg.Wait()

	for i := range requests {
		if requests[i].err != nil {
			return requests[i].err
		}
	}

	return nil
}

func (l *writeBackLane) submit(t *TxMetaCache, r *writeBackRequest) {
	l.mu.Lock()
	l.pending = append(l.pending, r)

	if l.writing {
		// The active writer picks this request up before it lets go of the lane.
		l.mu.Unlock()
		return
	}

	l.writing = true

	for {
		batch := l.pending
		l.pending = l.spare[:0]
		l.mu.Unlock()

		l.keys = l.keys[:0]
		l.values = l.values[:0]

		for _, req := range batch {
			l.keys = append(l.keys, req.keys...)
			l.values = append(l.values, req.values...)
		}

		err := t.SetCacheMultiValuesRaw(l.keys, l.values)

		// Drop references to the callers' (pooled) buffers before they are recycled.
		clear(l.keys)
		clear(l.values)

		for _, req := range batch {
			req.err = err
			req.wg.Done()
		}

		clear(batch)

		l.mu.Lock()
		l.spare = batch[:0]

		if len(l.pending) == 0 {
			l.writing = false
			l.mu.Unlock()

			return
		}
	}
}
