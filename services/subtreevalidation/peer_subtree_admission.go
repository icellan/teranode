package subtreevalidation

import (
	"context"
	"sync"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
)

// admissionGate bounds how many peer subtrees hold their per-subtree working
// set at once (subtreevalidation_peerSubtreeConcurrency). It is created on
// first use so struct-literal Servers (tests) get it too.
type admissionGate struct {
	once  sync.Once
	slots chan struct{}
}

// acquire waits for a slot and returns the function that frees it. A limit of
// zero or less disables the gate; the limit is read on the first call only.
// While it waits it polls bypass, and once bypass reports true it returns
// without taking a slot: a subtree that block validation is waiting for must
// not queue behind peer subtrees.
func (g *admissionGate) acquire(ctx context.Context, limit int, bypass func() bool) (func(), error) {
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
		return g.release, nil
	default:
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case g.slots <- struct{}{}:
			return g.release, nil
		case <-ticker.C:
			if bypass() {
				return func() {}, nil
			}
		case <-ctx.Done():
			return nil, errors.NewContextCanceledError("[ValidateSubtreeInternal] cancelled while waiting for peer subtree admission", ctx.Err())
		}
	}
}

func (g *admissionGate) release() {
	<-g.slots
}
