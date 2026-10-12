package subtreevalidation

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/testutil"
	"github.com/stretchr/testify/require"
)

// newCancelCtx returns a context that is cancelled when the test ends, so the
// goroutines New() starts on it (blockchainSubscriptionListener) exit with the
// test instead of leaking for the life of the test binary.
func newCancelCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return ctx
}

// subscribeOverrideClient wraps a real sqlitememory blockchain client and
// replaces Subscribe, signalling on called each time it is invoked.
type subscribeOverrideClient struct {
	blockchain.ClientI
	subscribe func() (chan *blockchain.Notification, error)
	called    chan struct{}
}

func (c *subscribeOverrideClient) Subscribe(_ context.Context, _ string) (chan *blockchain.Notification, error) {
	select {
	case c.called <- struct{}{}:
	default:
	}

	return c.subscribe()
}

// runListener starts blockchainSubscriptionListener on a cancellable ctx and
// returns the cancel func plus a channel closed when the listener returns.
func runListener(t *testing.T, client blockchain.ClientI) (*subscribeOverrideClient, context.CancelFunc, <-chan struct{}) {
	t.Helper()

	override, ok := client.(*subscribeOverrideClient)
	require.True(t, ok)

	server := &Server{logger: ulogger.TestLogger{}, blockchainClient: override}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})

	go func() {
		defer close(done)

		server.blockchainSubscriptionListener(ctx)
	}()

	return override, cancel, done
}

// TestBlockchainSubscriptionListenerExitsOnCancelWhileSubscribeFails pins the
// ctx.Done() branch of the 5s subscribe backoff: cancelling while Subscribe is
// failing must return the listener promptly rather than after the backoff.
func TestBlockchainSubscriptionListenerExitsOnCancelWhileSubscribeFails(t *testing.T) {
	common := testutil.NewCommonTestSetup(t)

	client := &subscribeOverrideClient{
		ClientI: testutil.NewMemorySQLiteBlockchainClient(common.Logger, common.Settings, t),
		subscribe: func() (chan *blockchain.Notification, error) {
			return nil, errors.NewServiceError("subscribe failed")
		},
		called: make(chan struct{}, 1),
	}

	_, cancel, done := runListener(t, client)

	select {
	case <-client.called:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Subscribe was never called")
	}

	// the listener is now parked in the 5s backoff
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		require.Fail(t, "listener did not exit promptly after ctx cancel during subscribe backoff")
	}
}

// TestBlockchainSubscriptionListenerExitsOnCancelDuringReconnectDelay pins the
// ctx.Done() branch of the 1s delay after the subscription channel closes.
func TestBlockchainSubscriptionListenerExitsOnCancelDuringReconnectDelay(t *testing.T) {
	common := testutil.NewCommonTestSetup(t)

	client := &subscribeOverrideClient{
		ClientI: testutil.NewMemorySQLiteBlockchainClient(common.Logger, common.Settings, t),
		subscribe: func() (chan *blockchain.Notification, error) {
			ch := make(chan *blockchain.Notification)
			close(ch)

			return ch, nil
		},
		called: make(chan struct{}, 1),
	}

	_, cancel, done := runListener(t, client)

	select {
	case <-client.called:
	case <-time.After(5 * time.Second):
		require.Fail(t, "Subscribe was never called")
	}

	// the listener has seen the closed channel and is parked in the 1s delay
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		require.Fail(t, "listener did not exit promptly after ctx cancel during reconnect delay")
	}
}
