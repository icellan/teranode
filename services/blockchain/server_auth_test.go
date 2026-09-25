package blockchain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	sqlstore "github.com/bsv-blockchain/teranode/stores/blockchain/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestProtectedMethodsCoverAllRPCs forces every unary RPC on both
// BlockchainAPI and PeerRegistryService (served on the same listener, see
// [Blockchain][Start]) to be classified as either admin-protected or
// explicitly public, so new mutating RPCs cannot ship unauthenticated by
// omission. Mirrors services/p2p/server_auth_test.go's
// TestAdminProtectedMethodsCoverAllRPCs.
func TestProtectedMethodsCoverAllRPCs(t *testing.T) {
	protected := protectedMethods

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
				"%s is not classified: add it to protectedMethods (state-mutating RPC) or the public list in this test (read-only)", fullMethod)
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
	// cannot be added to protectedMethods. It is deliberately left
	// unauthenticated rather than silently exempted: it only pushes
	// server-initiated block/subtree/FSM notifications to the caller, so an
	// unauthenticated subscriber can observe but not mutate chain state. This
	// assertion pins that decision - if BlockchainAPI ever gains a second
	// stream, or Subscribe starts accepting mutating input, this must be
	// revisited alongside stream auth.
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

	interceptor := util.CreateAuthInterceptor(apiKey, protectedMethods)

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

// TestAuthInterceptorProtectsReportPeerFailure exercises ReportPeerFailure,
// which, unprotected, would let an attacker reach the same b.notifications
// flood vector as SendNotification without ever calling SendNotification
// directly (see [Blockchain][ReportPeerFailure], which calls b.SendNotification
// as a plain Go method call that bypasses the gRPC interceptor boundary of
// SendNotification itself).
func TestAuthInterceptorProtectsReportPeerFailure(t *testing.T) {
	const apiKey = "test-admin-key"

	interceptor := util.CreateAuthInterceptor(apiKey, protectedMethods)

	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return "ok", nil
	}

	const method = "/blockchain_api.BlockchainAPI/ReportPeerFailure"

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

// TestAuthInterceptorLeavesPipelineRPCsPublic pins the deliberate split from
// Fix 1: RPCs Teranode's own services call in the normal course of following
// the chain (see the classification comment on protectedMethods) must stay
// reachable without an API key, even though they mutate state - otherwise a
// default (grpc_admin_api_key unset) deployment cannot add blocks, change FSM
// state, or leave IDLE.
func TestAuthInterceptorLeavesPipelineRPCsPublic(t *testing.T) {
	const apiKey = "test-admin-key"

	interceptor := util.CreateAuthInterceptor(apiKey, protectedMethods)

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	for method := range pipelinePublicBlockchainAPIMethods {
		_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		require.NoError(t, err, "%s must be reachable without an API key (pipeline RPC)", method)
	}
}

// TestResolveAdminAPIKey_Configured verifies that a configured, strong admin
// API key on a loopback listener is returned verbatim and no warning is
// logged.
func TestResolveAdminAPIKey_Configured(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	b := &Blockchain{
		logger: logger,
		settings: &settings.Settings{
			GRPCAdminAPIKey: "a-strong-random-admin-secret-value",
			BlockChain:      settings.BlockChainSettings{GRPCListenAddress: "127.0.0.1:8087"},
		},
	}

	apiKey, err := b.resolveAdminAPIKey()

	require.NoError(t, err)
	require.Equal(t, "a-strong-random-admin-secret-value", apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 0)
}

// TestResolveAdminAPIKey_ConfiguredWeakOrExposed verifies that a configured
// key still warns (but is not rejected) when it is short, or when the
// listener is not loopback-bound without verified TLS.
func TestResolveAdminAPIKey_ConfiguredWeakOrExposed(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	b := &Blockchain{
		logger: logger,
		settings: &settings.Settings{
			GRPCAdminAPIKey: "short-key",
			BlockChain:      settings.BlockChainSettings{GRPCListenAddress: "0.0.0.0:8087"},
		},
	}

	apiKey, err := b.resolveAdminAPIKey()

	require.NoError(t, err)
	require.Equal(t, "short-key", apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 2) // one length warning and one cleartext-exposure warning
}

// TestResolveAdminAPIKey_Placeholder verifies that a known placeholder key is
// ignored rather than trusted: util.ValidateAdminAPIKey (shared with
// services/legacy/Server.go and services/p2p/Server.go) reports it as a key
// to ignore, and resolveAdminAPIKey falls back to the same random-key,
// fail-closed path as an unset key, so admin-protected RPCs stay unreachable
// rather than being served behind a publicly known credential.
//
// This replaces the PR's original design, which treated a placeholder as a
// hard configuration error (resolveAdminAPIKey returning an error and the
// service refusing to start). Adopting main's shared, fail-closed helper
// means the outcome is "unreachable until fixed", matching legacy and p2p,
// not "service refuses to start".
func TestResolveAdminAPIKey_Placeholder(t *testing.T) {
	for _, key := range []string{"testkey", "TestKey", " testkey ", "changeme", "admin"} {
		b := &Blockchain{
			logger:   mocklogger.NewTestLogger(),
			settings: &settings.Settings{GRPCAdminAPIKey: key},
		}

		apiKey, err := b.resolveAdminAPIKey()

		require.NoError(t, err, "placeholder key %q must not error", key)
		require.NotEmpty(t, apiKey, "placeholder key %q must fall back to a random key, not an empty one", key)
		require.NotEqual(t, key, apiKey, "placeholder key %q must never be used verbatim as the credential", key)
	}
}

// TestResolveAdminAPIKey_Empty verifies that an unset admin API key falls
// back to a randomly generated key, so admin-protected RPCs stay unreachable
// until an operator configures a real secret - the fail-closed posture
// shared with services/legacy/Server.go and services/p2p/Server.go.
//
// This replaces the PR's original design, in which an unset key was returned
// as-is and no key was fabricated (admin RPCs were then reachable without any
// credential at all - fail open).
func TestResolveAdminAPIKey_Empty(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	b := &Blockchain{
		logger:   logger,
		settings: &settings.Settings{GRPCAdminAPIKey: ""},
	}

	apiKey, err := b.resolveAdminAPIKey()

	require.NoError(t, err)
	require.NotEmpty(t, apiKey, "an unset key must fall back to a random one, not stay empty (fail closed, not open)")
	logger.AssertNumberOfCalls(t, "Warnf", 1)
}

// TestGRPCAuthIsWiredIntoStart is the regression test the classification map
// itself cannot provide: it starts the real blockchain gRPC server through
// Start() -> util.StartGRPCServer with a key configured, dials it, and asserts
// a protected RPC is rejected without the header and accepted with it.
// Reverting the authOptions argument in Start() to nil fails here, which the
// map-only tests do not catch.
func TestGRPCAuthIsWiredIntoStart(t *testing.T) {
	const apiKey = "wiring-test-admin-key"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := ulogger.NewErrorTestLogger(t)

	tSettings := test.CreateBaseTestSettings(t)
	// A unique settings context keeps util.GetListener's listener cache from
	// colliding with other tests in this package.
	tSettings.Context = "auth-wiring-test"
	tSettings.GRPCAdminAPIKey = apiKey
	tSettings.BlockChain.GRPCListenAddress = "127.0.0.1:0"
	tSettings.BlockChain.HTTPListenAddress = "127.0.0.1:0"

	storeURL, err := url.Parse("sqlitememory:///blockchain_auth_wiring")
	require.NoError(t, err)

	blockchainStore, err := sqlstore.New(logger, storeURL, tSettings)
	require.NoError(t, err)

	server, err := New(ctx, logger, tSettings, blockchainStore, kafka.NewKafkaAsyncProducerMock())
	require.NoError(t, err)
	require.NoError(t, server.Init(ctx))

	readyCh := make(chan struct{})

	go func() {
		_ = server.Start(ctx, readyCh)
	}()

	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for blockchain server to start")
	}

	defer util.RemoveListener(tSettings.Context, "blockchain", "")
	defer util.RemoveListener(tSettings.Context, "blockchain", "http://")

	// The listener is cached by util.GetListener, so asking for it again
	// returns the one the server is serving on, along with its real address.
	_, address, _, err := util.GetListener(tSettings.Context, "blockchain", "", tSettings.BlockChain.GRPCListenAddress)
	require.NoError(t, err)

	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	defer func() {
		_ = conn.Close()
	}()

	client := blockchain_api.NewBlockchainAPIClient(conn)

	notification := &blockchain_api.Notification{
		Type: model.NotificationType_Block,
		Hash: make([]byte, chainhash.HashSize),
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()

	_, err = client.SendNotification(callCtx, notification)
	require.Equal(t, codes.Unauthenticated, status.Code(err), "SendNotification must be rejected without the API key - protectedMethods is not wired into Start()")

	authedCtx := metadata.AppendToOutgoingContext(callCtx, "x-api-key", apiKey)
	_, err = client.SendNotification(authedCtx, notification)
	require.NoError(t, err, "SendNotification must succeed with the configured API key")

	// A public RPC on the same listener must stay reachable without a key.
	_, err = client.GetFSMCurrentState(callCtx, &emptypb.Empty{})
	require.NotEqual(t, codes.Unauthenticated, status.Code(err), "a public RPC must not require the API key")
}

// TestHTTPAdminRoutesRequireAPIKey covers the second door onto
// InvalidateBlock/RevalidateBlock: the blockchain service's own HTTP listener.
// The gRPC auth interceptor never runs on that path, so the routes carry their
// own check. They are also POST-only, so a bare <img src="..."> on a page an
// operator visits cannot fire them.
//
// newServer sets b.adminAPIKey directly - the resolved key requireAdminAPIKey
// now compares against (see Fix 2) - rather than going through
// resolveAdminAPIKey/settings, since these subtests exercise the middleware's
// own comparison logic, not key resolution (covered separately by
// TestResolveAdminAPIKey_* and TestHTTPAdminRoutesRejectPlaceholderKey below).
func TestHTTPAdminRoutesRequireAPIKey(t *testing.T) {
	const apiKey = "http-admin-key"

	newServer := func(t *testing.T, resolvedKey string) (*Blockchain, *echo.Echo) {
		t.Helper()

		b := &Blockchain{logger: ulogger.TestLogger{}, settings: test.CreateBaseTestSettings(t), adminAPIKey: resolvedKey}

		e := echo.New()
		e.POST("/invalidate/:hash", func(c echo.Context) error {
			return c.String(http.StatusOK, "reached handler")
		}, b.requireAdminAPIKey)

		return b, e
	}

	hash := chainhash.Hash{}.String()

	t.Run("no header is rejected", func(t *testing.T) {
		_, e := newServer(t, apiKey)

		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invalidate/"+hash, nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong key is rejected", func(t *testing.T) {
		_, e := newServer(t, apiKey)

		req := httptest.NewRequest(http.MethodPost, "/invalidate/"+hash, nil)
		req.Header.Set("x-api-key", "wrong")

		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("correct key reaches the handler", func(t *testing.T) {
		_, e := newServer(t, apiKey)

		req := httptest.NewRequest(http.MethodPost, "/invalidate/"+hash, nil)
		req.Header.Set("x-api-key", apiKey)

		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("unset key closes the route rather than opening it", func(t *testing.T) {
		_, e := newServer(t, "")

		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/invalidate/"+hash, nil))
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("GET is not routed", func(t *testing.T) {
		b, _ := newServer(t, apiKey)

		// Build the real route table so the GET/POST decision is the
		// production one rather than the test's.
		e := echo.New()
		e.POST("/invalidate/:hash", b.invalidateHandler, b.requireAdminAPIKey)
		e.POST("/revalidate/:hash", b.revalidateHandler, b.requireAdminAPIKey)

		for _, path := range []string{"/invalidate/" + hash, "/revalidate/" + hash} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("x-api-key", apiKey)

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s must not be reachable by GET", path)
		}
	})
}

// TestHTTPAdminRoutesRejectPlaceholderKey is the regression test for the auth
// bypass fixed here: requireAdminAPIKey used to compare against the raw
// b.settings.GRPCAdminAPIKey rather than the resolved key, so a well-known
// placeholder like "testkey" - which resolveAdminAPIKey ignores in favour of a
// random key - was accepted verbatim over HTTP even though the gRPC
// interceptor (which uses the resolved key) rejects it. Both doors now share
// b.adminAPIKey, set once in Start() from resolveAdminAPIKey().
func TestHTTPAdminRoutesRejectPlaceholderKey(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.GRPCAdminAPIKey = "testkey" // well-known placeholder

	b := &Blockchain{logger: mocklogger.NewTestLogger(), settings: tSettings}

	resolvedKey, err := b.resolveAdminAPIKey()
	require.NoError(t, err)
	require.NotEqual(t, "testkey", resolvedKey, "resolveAdminAPIKey must not return the placeholder verbatim")

	b.adminAPIKey = resolvedKey

	e := echo.New()
	e.POST("/invalidate/:hash", func(c echo.Context) error {
		return c.String(http.StatusOK, "reached handler")
	}, b.requireAdminAPIKey)

	req := httptest.NewRequest(http.MethodPost, "/invalidate/"+chainhash.Hash{}.String(), nil)
	req.Header.Set("x-api-key", "testkey")

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"the raw placeholder must be rejected now that the HTTP route compares against the resolved key, not the raw setting")
}
