package httpimpl

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/teranode/services/asset/repository"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

const testHashHex = "0000000000000000000000000000000000000000000000000000000000000001"

// corsProbe serves a single GET through the supplied CORS config and returns
// the recorder so the response headers can be asserted.
func corsProbe(t *testing.T, cfg middleware.CORSConfig, origin string) *httptest.ResponseRecorder {
	t.Helper()

	e := echo.New()
	e.Use(middleware.CORSWithConfig(cfg))
	e.GET("/probe", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(echo.HeaderOrigin, origin)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	return rec
}

// TestAssetCORSConfig_EmptyAllowlistNeverAllowsCredentials — with no
// asset_corsAllowedOrigins the legacy reflect-any behaviour is preserved, but
// credentialed cross-origin responses must be refused. Reflecting an arbitrary
// origin *and* allowing credentials is what let a hostile same-site origin ride
// an operator's ambient cookie into the admin routes.
func TestAssetCORSConfig_EmptyAllowlistNeverAllowsCredentials(t *testing.T) {
	rec := corsProbe(t, assetCORSConfig(nil), "https://evil.example.com")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "https://evil.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin),
		"empty allowlist keeps the legacy reflect-any behaviour")
	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials),
		"a reflected arbitrary origin must never be granted credentials")
}

// TestAssetCORSConfig_ExplicitAllowlistIsStrict — a configured allowlist is
// matched exactly, and only then are credentials permitted.
func TestAssetCORSConfig_ExplicitAllowlistIsStrict(t *testing.T) {
	origins := parseCORSAllowedOrigins(" https://ops.example.com | https://admin.example.com ")
	require.Equal(t, []string{"https://ops.example.com", "https://admin.example.com"}, origins)

	cfg := assetCORSConfig(origins)

	t.Run("allowed origin gets credentials", func(t *testing.T) {
		rec := corsProbe(t, cfg, "https://ops.example.com")
		require.Equal(t, "https://ops.example.com", rec.Header().Get(echo.HeaderAccessControlAllowOrigin))
		require.Equal(t, "true", rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
	})

	t.Run("unlisted origin is not reflected", func(t *testing.T) {
		rec := corsProbe(t, cfg, "https://evil.example.com")
		require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowOrigin),
			"an unlisted origin must not be reflected")
		require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowCredentials))
	})
}

// newTestServer builds a real HTTP server over a minimal repository so route
// and middleware wiring can be exercised end to end.
func newTestServer(t *testing.T, tSettings *settings.Settings) *HTTP {
	t.Helper()

	repo, err := repository.NewRepository(ulogger.TestLogger{}, tSettings, nil, nil, &blockchain.Mock{}, nil, memory.New(), nil, nil, nil)
	require.NoError(t, err)

	srv, err := New(ulogger.TestLogger{}, tSettings, repo, nil)
	require.NoError(t, err)

	return srv
}

func baseTestSettings() *settings.Settings {
	return &settings.Settings{
		Asset: settings.AssetSettings{
			APIPrefix: "/api/v1",
		},
		Dashboard:         settings.DashboardSettings{Enabled: false},
		SecurityLevelHTTP: 0,
	}
}

// TestNew_DashboardCORSUsesTheSameAllowlist — there are two CORS configs on the
// Asset listener (the default one and the one registered in the dashboard
// branch). Narrowing only one leaves the defect intact, so both must honour
// asset_corsAllowedOrigins.
func TestNew_DashboardCORSUsesTheSameAllowlist(t *testing.T) {
	tSettings := baseTestSettings()
	tSettings.Asset.CORSAllowedOrigins = "https://ops.example.com"
	tSettings.Dashboard.Enabled = true
	tSettings.RPC = settings.RPCSettings{RPCUser: "bitcoin", RPCPass: "bitcoin"}

	srv := newTestServer(t, tSettings)

	req := httptest.NewRequest(http.MethodGet, "/alive", nil)
	req.Header.Set(echo.HeaderOrigin, "https://evil.example.com")
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)

	require.Empty(t, rec.Header().Get(echo.HeaderAccessControlAllowOrigin),
		"the dashboard CORS config must not reflect an unlisted origin either")
}

// TestNew_TrustedProxyCIDRsReplaceEchoDefaults — echo.TrustIPRange is additive:
// loopback, link-local and private networks stay trusted unless explicitly
// disabled. An operator who configures an allowlist means that list and nothing
// else, otherwise any RFC1918 client can forge X-Forwarded-For.
func TestNew_TrustedProxyCIDRsReplaceEchoDefaults(t *testing.T) {
	tSettings := baseTestSettings()
	tSettings.Asset.TrustedProxyCIDRs = "203.0.113.0/24"

	srv := newTestServer(t, tSettings)
	require.NotNil(t, srv.e.IPExtractor)

	t.Run("untrusted private client cannot forge XFF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/alive", nil)
		req.RemoteAddr = "10.1.2.3:4444"
		req.Header.Set(echo.HeaderXForwardedFor, "8.8.8.8")

		require.Equal(t, "10.1.2.3", srv.e.IPExtractor(req),
			"an RFC1918 peer outside the configured allowlist must not be trusted as a proxy")
	})

	t.Run("untrusted loopback client cannot forge XFF", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/alive", nil)
		req.RemoteAddr = "127.0.0.1:4444"
		req.Header.Set(echo.HeaderXForwardedFor, "8.8.8.8")

		require.Equal(t, "127.0.0.1", srv.e.IPExtractor(req),
			"loopback must not be trusted as a proxy when an explicit allowlist is configured")
	})

	t.Run("configured proxy is still trusted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/alive", nil)
		req.RemoteAddr = "203.0.113.7:4444"
		req.Header.Set(echo.HeaderXForwardedFor, "8.8.8.8")

		require.Equal(t, "8.8.8.8", srv.e.IPExtractor(req),
			"a peer inside the configured allowlist must still be trusted")
	})

	t.Run("empty setting keeps the documented Echo defaults", func(t *testing.T) {
		defaults := newTestServer(t, baseTestSettings())

		req := httptest.NewRequest(http.MethodGet, "/alive", nil)
		req.RemoteAddr = "10.1.2.3:4444"
		req.Header.Set(echo.HeaderXForwardedFor, "8.8.8.8")

		require.Equal(t, "8.8.8.8", defaults.e.IPExtractor(req),
			"with no allowlist the documented default (trust loopback and private networks) is unchanged")
	})
}

// TestNormalizeHTTPMethod — the Prometheus method label must come from a fixed
// domain; anything else buckets to "other".
func TestNormalizeHTTPMethod(t *testing.T) {
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace,
	} {
		require.Equal(t, method, normalizeHTTPMethod(method))
	}

	require.Equal(t, "other", normalizeHTTPMethod("FROBNICATE"))
	require.Equal(t, "other", normalizeHTTPMethod("get"))
	require.Equal(t, "other", normalizeHTTPMethod(""))
}

// TestAccessLogMiddleware_BoundsMethodLabelCardinality — arbitrary method
// tokens are accepted by net/http before routing and histogram children are
// never evicted, so a raw method label is permanent unbounded memory growth.
func TestAccessLogMiddleware_BoundsMethodLabelCardinality(t *testing.T) {
	initPrometheusMetrics()

	e := echo.New()
	e.Use(accessLogMiddleware(ulogger.TestLogger{}))
	e.Any("/cardinality", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/cardinality"})

	for _, method := range []string{"FROBNICATE", "WIBBLE", "ZORK"} {
		req := httptest.NewRequest(method, "/cardinality", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
	}

	require.Equal(t, 0,
		prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/cardinality", "method": "FROBNICATE"}),
		"a raw attacker-supplied method token must never become a label value")
	require.Equal(t, 1,
		prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/cardinality"}),
		"three distinct unknown method tokens must collapse into a single 'other' series")
}

// TestNew_RateLimitedRequestsAreNotMetered — the access-log middleware was
// registered outside the global limiter, so 429-rejected requests still minted
// histogram children (the cheapest way to drive the cardinality growth above).
// The limiter has its own counter; rejected requests must not also land in the
// request histograms.
func TestNew_RateLimitedRequestsAreNotMetered(t *testing.T) {
	initPrometheusMetrics()

	tSettings := baseTestSettings()
	tSettings.Asset.HTTPRateLimit = 1

	srv := newTestServer(t, tSettings)

	prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/alive"})

	var lastCode int

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/alive", nil)
		req.RemoteAddr = "198.51.100.9:5555"
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		lastCode = rec.Code
	}

	require.Equal(t, http.StatusTooManyRequests, lastCode, "second request must be rate limited")

	require.Equal(t, 0,
		prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/alive", "status": "429"}),
		"a rejected request must not create a request-histogram series; it is counted by http_rate_limited_total")
	require.Equal(t, 1,
		prometheusAssetHTTPRequestDuration.DeletePartialMatch(prometheus.Labels{"path": "/alive"}),
		"only the served request may create a histogram series")
}

// TestPostAuthEnforcement — asset_enforcePostAuth actually applies the
// dashboard POST credential check (Echo's Group.Use only covers routes
// registered afterwards, so the old call site was dead), while the peer-catchup
// POST stays reachable unconditionally.
func TestPostAuthEnforcement(t *testing.T) {
	const (
		catchupPath = "/api/v1/subtree/" + testHashHex + "/txs"
		bulkPath    = "/api/v1/utxos/json"
	)

	mkSettings := func(dashboard, enforce bool) *settings.Settings {
		tSettings := baseTestSettings()
		tSettings.Dashboard.Enabled = dashboard
		tSettings.Asset.EnforcePostAuth = enforce
		tSettings.RPC = settings.RPCSettings{RPCUser: "bitcoin", RPCPass: "bitcoin"}

		return tSettings
	}

	post := func(t *testing.T, srv *HTTP, path string) int {
		t.Helper()

		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)

		return rec.Code
	}

	t.Run("flag on: unauthenticated bulk POST is rejected", func(t *testing.T) {
		srv := newTestServer(t, mkSettings(false, true))
		require.Equal(t, http.StatusUnauthorized, post(t, srv, bulkPath))
	})

	t.Run("flag on: peer catchup POST stays reachable", func(t *testing.T) {
		srv := newTestServer(t, mkSettings(false, true))
		require.NotEqual(t, http.StatusUnauthorized, post(t, srv, catchupPath),
			"catchup requests carry no credentials; 401 here wedges every peer catching up from this node")
	})

	t.Run("flag on with dashboard enabled: peer catchup POST stays reachable", func(t *testing.T) {
		srv := newTestServer(t, mkSettings(true, true))
		require.Equal(t, http.StatusUnauthorized, post(t, srv, bulkPath))
		require.NotEqual(t, http.StatusUnauthorized, post(t, srv, catchupPath),
			"enabling the dashboard must never break peer catchup")
	})

	t.Run("dashboard enabled, flag off: no POST is rejected", func(t *testing.T) {
		srv := newTestServer(t, mkSettings(true, false))
		require.NotEqual(t, http.StatusUnauthorized, post(t, srv, bulkPath),
			"enforcement is opt-in; enabling the dashboard alone must not start rejecting POSTs")
		require.NotEqual(t, http.StatusUnauthorized, post(t, srv, catchupPath))
	})
}

// TestSign_DeclaresItsScope — X-Signature covers the resource identifier only,
// never the status or the serialized body. The header has to say so; clients
// must not read it as body integrity.
func TestSign_DeclaresItsScope(t *testing.T) {
	srv, _, c, rec := GetMockHTTP(t, nil)

	require.NoError(t, srv.Sign(c.Response(), []byte("resource-id")))
	require.NotEmpty(t, rec.Header().Get("X-Signature"))
	require.Equal(t, signatureScopeResourceIdentifier, rec.Header().Get("X-Signature-Scope"),
		"the signature scope must be stated on the wire so X-Signature is not mistaken for body integrity")
}
