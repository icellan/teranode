// Package p2p provides peer-to-peer networking functionality for the Teranode system.
package p2p

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// TestIPRateLimiter_AllowsUpToConfiguredRate verifies that a fresh bucket
// grants exactly ratePerSec requests (the initial burst) before rejecting.
func TestIPRateLimiter_AllowsUpToConfiguredRate(t *testing.T) {
	rl := newIPRateLimiter(3)

	for i := 0; i < 3; i++ {
		require.True(t, rl.allow("10.0.0.1"), "request %d should be within the configured rate", i+1)
	}
}

// TestIPRateLimiter_RejectsBeyondRate verifies that once the burst is
// exhausted, further immediate requests from the same IP are rejected.
func TestIPRateLimiter_RejectsBeyondRate(t *testing.T) {
	rl := newIPRateLimiter(2)

	require.True(t, rl.allow("10.0.0.1"))
	require.True(t, rl.allow("10.0.0.1"))
	require.False(t, rl.allow("10.0.0.1"), "request beyond the configured rate must be rejected")
}

// TestIPRateLimiter_KeysPerIP verifies that each source IP gets its own
// independent bucket, so one IP exhausting its rate doesn't affect another.
func TestIPRateLimiter_KeysPerIP(t *testing.T) {
	rl := newIPRateLimiter(1)

	require.True(t, rl.allow("10.0.0.1"))
	require.False(t, rl.allow("10.0.0.1"), "10.0.0.1's burst should already be exhausted")

	require.True(t, rl.allow("10.0.0.2"), "a different IP must not share 10.0.0.1's bucket")
}

// TestIPRateLimiter_MiddlewarePassThroughWhenDisabled verifies that a
// non-positive rate disables the limiter entirely, so Middleware never
// rejects a request.
func TestIPRateLimiter_MiddlewarePassThroughWhenDisabled(t *testing.T) {
	e := echo.New()
	e.Use(newIPRateLimiter(0).Middleware())
	e.GET("/test", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "request %d should pass when rate limiting is disabled", i+1)
	}
}

// TestIPRateLimiter_MiddlewareRejectsBeyondRate verifies the Middleware path
// (not just the underlying allow()) returns 429 once the configured rate is
// exceeded for a given IP.
func TestIPRateLimiter_MiddlewareRejectsBeyondRate(t *testing.T) {
	e := echo.New()
	e.Use(newIPRateLimiter(1).Middleware())
	e.GET("/test", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/test", nil))
	require.Equal(t, http.StatusOK, rec1.Code, "first request should pass")

	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/test", nil))
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, "second request should be rate-limited")
}
