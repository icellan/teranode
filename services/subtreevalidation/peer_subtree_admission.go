package subtreevalidation

import (
	"context"
	"sync"

	"github.com/bsv-blockchain/teranode/errors"
)

// admissionGate bounds how many peer subtrees are being validated at once
// (subtreevalidation_peerSubtreeConcurrency), so a burst of peer subtrees
// after a block cannot each allocate its working set (about 1 GiB for 1M
// transactions) at the same time. It is created on first use so
// struct-literal Servers (tests) get it too.
type admissionGate struct {
	once  sync.Once
	slots chan struct{}
}

// acquire waits for a slot and returns the function that frees it. A limit of
// zero or less disables the gate; the limit is read on the first call only.
func (g *admissionGate) acquire(ctx context.Context, limit int) (func(), error) {
	g.once.Do(func() {
		if limit > 0 {
			g.slots = make(chan struct{}, limit)
		}
	})

	if g.slots == nil {
		return func() {}, nil
	}

	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	case <-ctx.Done():
		return nil, errors.NewContextCanceledError("[subtreesHandler] cancelled while waiting for peer subtree admission", ctx.Err())
	}
}
