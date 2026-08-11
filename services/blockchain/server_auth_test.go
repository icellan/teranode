package blockchain

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// publicBlockchainAPIMethods are the BlockchainAPI RPCs deliberately
// reachable without the admin API key: read-only queries. Every mutating RPC
// belongs in protectedMethods() (Server.go) instead. Adding an RPC to
// neither this list nor protectedMethods() fails
// TestProtectedMethodsCoverAllRPCs.
var publicBlockchainAPIMethods = map[string]bool{
	"/blockchain_api.BlockchainAPI/HealthGRPC":                           true,
	"/blockchain_api.BlockchainAPI/GetBlock":                             true,
	"/blockchain_api.BlockchainAPI/GetBlocks":                            true,
	"/blockchain_api.BlockchainAPI/GetBlockByHeight":                     true,
	"/blockchain_api.BlockchainAPI/GetBlockByID":                         true,
	"/blockchain_api.BlockchainAPI/GetNextBlockID":                       true,
	"/blockchain_api.BlockchainAPI/GetBlockStats":                        true,
	"/blockchain_api.BlockchainAPI/GetBlockGraphData":                    true,
	"/blockchain_api.BlockchainAPI/GetLastNBlocks":                       true,
	"/blockchain_api.BlockchainAPI/GetLastNInvalidBlocks":                true,
	"/blockchain_api.BlockchainAPI/GetSuitableBlock":                     true,
	"/blockchain_api.BlockchainAPI/GetHashOfAncestorBlock":               true,
	"/blockchain_api.BlockchainAPI/GetLatestBlockHeaderFromBlockLocator": true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersFromOldest":            true,
	"/blockchain_api.BlockchainAPI/GetNextWorkRequired":                  true,
	"/blockchain_api.BlockchainAPI/GetBlockExists":                       true,
	"/blockchain_api.BlockchainAPI/GetBlockHeaders":                      true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersToCommonAncestor":      true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersFromCommonAncestor":    true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersFromTill":              true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersFromHeight":            true,
	"/blockchain_api.BlockchainAPI/GetBlockHeadersByHeight":              true,
	"/blockchain_api.BlockchainAPI/GetMedianTimePastByHeights":           true,
	"/blockchain_api.BlockchainAPI/GetBlocksByHeight":                    true,
	"/blockchain_api.BlockchainAPI/FindBlocksContainingSubtree":          true,
	"/blockchain_api.BlockchainAPI/GetBlockHeaderIDs":                    true,
	"/blockchain_api.BlockchainAPI/GetBestBlockHeader":                   true,
	"/blockchain_api.BlockchainAPI/CheckBlockIsInCurrentChain":           true,
	"/blockchain_api.BlockchainAPI/CheckBlockIsAncestorOfBlock":          true,
	"/blockchain_api.BlockchainAPI/GetChainTips":                         true,
	"/blockchain_api.BlockchainAPI/GetBlockHeader":                       true,
	"/blockchain_api.BlockchainAPI/GetSubscribers":                       true,
	"/blockchain_api.BlockchainAPI/GetState":                             true,
	"/blockchain_api.BlockchainAPI/GetBlockIsMined":                      true,
	"/blockchain_api.BlockchainAPI/GetBlocksMinedNotSet":                 true,
	"/blockchain_api.BlockchainAPI/GetBlocksNotPersisted":                true,
	"/blockchain_api.BlockchainAPI/GetBlocksSubtreesNotSet":              true,
	"/blockchain_api.BlockchainAPI/GetFSMCurrentState":                   true,
	"/blockchain_api.BlockchainAPI/WaitUntilFSMTransitionFromIdleState":  true,
	"/blockchain_api.BlockchainAPI/GetBlockLocator":                      true,
	"/blockchain_api.BlockchainAPI/LocateBlockHeaders":                   true,
	"/blockchain_api.BlockchainAPI/GetBestHeightAndTime":                 true,
	"/blockchain_api.BlockchainAPI/ListScheduledDeletions":               true,
	"/blockchain_api.BlockchainAPI/GetPendingBlobDeletions":              true,
}

// publicPeerRegistryServiceMethods are the PeerRegistryService RPCs
// deliberately reachable without the admin API key: read-only queries.
var publicPeerRegistryServiceMethods = map[string]bool{
	"/blockchain_api.PeerRegistryService/GetPeer":         true,
	"/blockchain_api.PeerRegistryService/ListPeers":       true,
	"/blockchain_api.PeerRegistryService/IsPeerBanned":    true,
	"/blockchain_api.PeerRegistryService/ListBannedPeers": true,
}

// TestProtectedMethodsCoverAllRPCs forces every unary RPC on both
// BlockchainAPI and PeerRegistryService (served on the same listener, see
// [Blockchain][Start]) to be classified as either admin-protected or
// explicitly public, so new mutating RPCs cannot ship unauthenticated by
// omission. Mirrors services/p2p/server_auth_test.go's
// TestAdminProtectedMethodsCoverAllRPCs.
func TestProtectedMethodsCoverAllRPCs(t *testing.T) {
	protected := protectedMethods()

	checkCoverage := func(t *testing.T, serviceName string, methods []grpc.MethodDesc, public map[string]bool) {
		t.Helper()

		registered := make(map[string]bool, len(methods))

		for _, m := range methods {
			fullMethod := "/" + serviceName + "/" + m.MethodName
			registered[fullMethod] = true

			isProtected := protected[fullMethod]
			isPublic := public[fullMethod]

			require.False(t, isProtected && isPublic, "%s is both protected and public", fullMethod)
			require.True(t, isProtected || isPublic,
				"%s is not classified: add it to protectedMethods() (state-mutating RPC) or the public list in this test (read-only)", fullMethod)
		}

		for method := range public {
			require.True(t, registered[method], "public method %s is not a registered %s RPC", method, serviceName)
		}
	}

	checkCoverage(t, blockchain_api.BlockchainAPI_ServiceDesc.ServiceName, blockchain_api.BlockchainAPI_ServiceDesc.Methods, publicBlockchainAPIMethods)
	checkCoverage(t, blockchain_api.PeerRegistryService_ServiceDesc.ServiceName, blockchain_api.PeerRegistryService_ServiceDesc.Methods, publicPeerRegistryServiceMethods)

	// Every protected entry must correspond to a real, registered RPC on one
	// of the two services (catches typos).
	allRegistered := make(map[string]bool)

	for _, m := range blockchain_api.BlockchainAPI_ServiceDesc.Methods {
		allRegistered["/"+blockchain_api.BlockchainAPI_ServiceDesc.ServiceName+"/"+m.MethodName] = true
	}

	for _, m := range blockchain_api.PeerRegistryService_ServiceDesc.Methods {
		allRegistered["/"+blockchain_api.PeerRegistryService_ServiceDesc.ServiceName+"/"+m.MethodName] = true
	}

	for method := range protected {
		require.True(t, allRegistered[method], "protected method %s is not a registered RPC", method)
	}

	// BlockchainAPI declares one streaming RPC, Subscribe. The auth
	// interceptor installed in util.StartGRPCServer is unary-only (see
	// util/grpc.go), so Subscribe bypasses authentication entirely and
	// cannot be added to protectedMethods(). It is deliberately left
	// unauthenticated rather than silently exempted: it only pushes
	// server-initiated block/subtree/FSM notifications to the caller (no
	// caller-supplied state is written), so an unauthenticated subscriber can
	// observe but not mutate chain state. This assertion pins that decision -
	// if BlockchainAPI ever gains a second stream, or Subscribe starts
	// accepting mutating input, this must be revisited alongside stream auth.
	require.Len(t, blockchain_api.BlockchainAPI_ServiceDesc.Streams, 1,
		"BlockchainAPI stream count changed; re-evaluate stream auth before adding more streams")
	require.Equal(t, "Subscribe", blockchain_api.BlockchainAPI_ServiceDesc.Streams[0].StreamName,
		"BlockchainAPI's only stream is expected to be Subscribe (deliberately left unauthenticated, see comment above)")
	require.Empty(t, blockchain_api.PeerRegistryService_ServiceDesc.Streams,
		"PeerRegistryService has streaming RPCs but the auth interceptor only covers unary methods; add stream auth before registering streams")
}

// TestAuthInterceptorProtectsSendNotification exercises the auth interceptor
// with the production blockchain protected-method set: SendNotification must
// be rejected without a valid API key, while an unrelated RPC on the same
// service passes through untouched.
func TestAuthInterceptorProtectsSendNotification(t *testing.T) {
	const apiKey = "test-admin-key"

	interceptor := util.CreateAuthInterceptor(apiKey, protectedMethods())

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

	// An unrelated, public RPC on the same service must not require a key.
	const unrelatedMethod = "/blockchain_api.BlockchainAPI/GetBestBlockHeader"
	err = call(context.Background(), unrelatedMethod)
	require.NoError(t, err, "unrelated RPC must not require a key")
	require.True(t, handlerCalled, "unrelated RPC handler must run")
}

// TestAuthInterceptorProtectsReportPeerFailureAndSetBlockSubtreesSet exercises
// the two RPCs that, unprotected, let an attacker reach the same
// b.notifications flood vector as SendNotification without ever calling
// SendNotification directly (see [Blockchain][ReportPeerFailure] and
// [Blockchain][SetBlockSubtreesSet], both of which call b.SendNotification as
// a plain Go method call that bypasses the gRPC interceptor boundary of
// SendNotification itself).
func TestAuthInterceptorProtectsReportPeerFailureAndSetBlockSubtreesSet(t *testing.T) {
	const apiKey = "test-admin-key"

	interceptor := util.CreateAuthInterceptor(apiKey, protectedMethods())

	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}

	for _, method := range []string{
		"/blockchain_api.BlockchainAPI/ReportPeerFailure",
		"/blockchain_api.BlockchainAPI/SetBlockSubtreesSet",
	} {
		handlerCalled = false
		_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err), "%s without metadata must be rejected", method)
		require.False(t, handlerCalled, "%s handler must not run without a key", method)

		handlerCalled = false
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", apiKey))
		_, err = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		require.NoError(t, err, "%s with the correct key must succeed", method)
		require.True(t, handlerCalled, "%s handler must run with the correct key", method)
	}
}

// TestResolveAdminAPIKey_Configured verifies that a configured admin API key
// is returned verbatim and no warning is logged.
func TestResolveAdminAPIKey_Configured(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	b := &Blockchain{
		logger:   logger,
		settings: &settings.Settings{GRPCAdminAPIKey: "configured-key"},
	}

	apiKey := b.resolveAdminAPIKey()

	require.Equal(t, "configured-key", apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 0)
}

// TestResolveAdminAPIKey_Empty verifies that an unset admin API key is
// returned as-is (no key is fabricated - a generated key no client could
// ever learn just masked the exposure) and a single warning is logged
// naming the exposure.
func TestResolveAdminAPIKey_Empty(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	b := &Blockchain{
		logger:   logger,
		settings: &settings.Settings{GRPCAdminAPIKey: ""},
	}

	apiKey := b.resolveAdminAPIKey()

	require.Empty(t, apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 1)
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
