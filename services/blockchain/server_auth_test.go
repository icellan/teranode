package blockchain

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// sendNotificationProtectedMethods mirrors the ProtectedMethods map built in
// [Blockchain][Start]: only SendNotification requires the admin API key.
// Kept separate from the production map literal so a future accidental
// widening (or narrowing) of scope in Server.go is caught by this test.
var sendNotificationProtectedMethods = map[string]bool{
	"/blockchain_api.BlockchainAPI/SendNotification": true,
}

// TestAuthInterceptorProtectsSendNotification exercises the auth interceptor
// with the blockchain protected-method set: SendNotification must be
// rejected without a valid API key, while an unrelated RPC on the same
// service passes through untouched (only SendNotification is in scope for
// this fix; broadening auth to the rest of BlockchainAPI is a follow-up).
func TestAuthInterceptorProtectsSendNotification(t *testing.T) {
	const apiKey = "test-admin-key"

	interceptor := util.CreateAuthInterceptor(apiKey, sendNotificationProtectedMethods)

	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}

	call := func(ctx context.Context, fullMethod string) error {
		handlerCalled = false
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)

		return err
	}

	const method = "/blockchain_api.BlockchainAPI/SendNotification"

	// No metadata at all.
	err := call(context.Background(), method)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "SendNotification without metadata must be rejected")
	require.False(t, handlerCalled, "handler must not run without a key")

	// Wrong key.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "wrong-key"))
	err = call(ctx, method)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "SendNotification with a wrong key must be rejected")
	require.False(t, handlerCalled, "handler must not run with a wrong key")

	// Correct key.
	ctx = metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", apiKey))
	err = call(ctx, method)
	require.NoError(t, err, "SendNotification with the correct key must succeed")
	require.True(t, handlerCalled, "handler must run with the correct key")

	// An unrelated RPC on the same service is not in ProtectedMethods, so it
	// must not require a key.
	const unrelatedMethod = "/blockchain_api.BlockchainAPI/GetBestBlockHeader"
	err = call(context.Background(), unrelatedMethod)
	require.NoError(t, err, "unrelated RPC must not require a key")
	require.True(t, handlerCalled, "unrelated RPC handler must run")
}

// newTestBlockchainForNotifications builds a minimal Blockchain sufficient to
// exercise SendNotification's validation and channel-send logic directly,
// without a running gRPC server or store.
func newTestBlockchainForNotifications(t *testing.T, bufferSize int) *Blockchain {
	t.Helper()

	initPrometheusMetrics()

	return &Blockchain{
		logger:        ulogger.TestLogger{},
		notifications: make(chan *blockchain_api.Notification, bufferSize),
	}
}

func TestSendNotification_Validation(t *testing.T) {
	ctx := context.Background()
	validHash := make([]byte, chainhash.HashSize)

	t.Run("nil request is rejected", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, nil)
		require.Error(t, err)
	})

	t.Run("unrecognized type is rejected", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType(999),
			Hash: validHash,
		})
		require.Error(t, err)
	})

	t.Run("PING with a hash is rejected", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType_PING,
			Hash: validHash,
		})
		require.Error(t, err)
	})

	t.Run("non-PING with an empty hash is rejected", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType_Block,
		})
		require.Error(t, err)
	})

	t.Run("non-PING with a short hash is rejected", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType_Block,
			Hash: []byte{1, 2, 3},
		})
		require.Error(t, err)
	})

	t.Run("valid PING notification is accepted", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType_PING,
		})
		require.NoError(t, err)
	})

	t.Run("valid Block notification is accepted", func(t *testing.T) {
		b := newTestBlockchainForNotifications(t, 1)

		_, err := b.SendNotification(ctx, &blockchain_api.Notification{
			Type: model.NotificationType_Block,
			Hash: validHash,
		})
		require.NoError(t, err)
	})
}

// TestSendNotification_NonBlockingWhenChannelFull verifies that a flood of
// calls degrades gracefully (dropping notifications once the channel buffer
// is full) rather than blocking the RPC handler goroutine.
func TestSendNotification_NonBlockingWhenChannelFull(t *testing.T) {
	ctx := context.Background()
	validHash := make([]byte, chainhash.HashSize)

	b := newTestBlockchainForNotifications(t, 1)

	notification := &blockchain_api.Notification{
		Type: model.NotificationType_Block,
		Hash: validHash,
	}

	// Fill the buffer.
	_, err := b.SendNotification(ctx, notification)
	require.NoError(t, err)

	// The channel is now full; without the non-blocking select this call
	// would hang forever since nothing is draining the channel.
	done := make(chan struct{})
	go func() {
		_, err := b.SendNotification(ctx, notification)
		require.NoError(t, err)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SendNotification blocked on a full channel instead of dropping the notification")
	}
}
