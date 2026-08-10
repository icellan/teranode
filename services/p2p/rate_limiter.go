// Package p2p provides peer-to-peer networking functionality for the Teranode system.
package p2p

import (
	"net/http"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"
)

// ipRateLimiterCapacity bounds the number of distinct per-IP buckets
// retained. Mirrors the LRU-bounded pattern in
// services/asset/httpimpl/rate_limiter.go: without a bound, an attacker
// rotating source IPs could grow the map without limit.
const ipRateLimiterCapacity = 50_000

// ipRateLimiter is a per-IP token-bucket rate limiter for the P2P HTTP
// server (/health, /p2p-ws). Unlike the Asset service's tieredRateLimiter,
// the P2P HTTP surface has no authenticated peer-tiering concept, so a
// single IP-keyed bucket is the smallest adaptation of that pattern that
// fits here.
type ipRateLimiter struct {
	limiters   *lru.Cache[string, *rate.Limiter]
	ratePerSec int
}

// newIPRateLimiter creates an ipRateLimiter. ratePerSec <= 0 disables the
// limiter entirely (Middleware becomes a no-op).
func newIPRateLimiter(ratePerSec int) *ipRateLimiter {
	// lru.New only errors when capacity <= 0, which ipRateLimiterCapacity never is.
	cache, _ := lru.New[string, *rate.Limiter](ipRateLimiterCapacity)

	return &ipRateLimiter{
		limiters:   cache,
		ratePerSec: ratePerSec,
	}
}

// Middleware returns the Echo middleware function for this rate limiter.
func (rl *ipRateLimiter) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if rl.ratePerSec <= 0 {
				return next(c)
			}

			if !rl.allow(c.RealIP()) {
				if prometheusP2PHTTPRateLimited != nil {
					prometheusP2PHTTPRateLimited.Inc()
				}

				return c.JSON(http.StatusTooManyRequests, map[string]string{"message": "rate limit exceeded"})
			}

			return next(c)
		}
	}
}

// allow consumes one token from rawIP's bucket, creating it if necessary.
// Uses load-then-store so a race-losing goroutine's allocation isn't
// stranded.
func (rl *ipRateLimiter) allow(rawIP string) bool {
	key := wsConnKey(rawIP)

	if lim, ok := rl.limiters.Get(key); ok {
		return lim.Allow()
	}

	lim := rate.NewLimiter(rate.Limit(rl.ratePerSec), rl.ratePerSec)
	rl.limiters.Add(key, lim)

	if existing, ok := rl.limiters.Get(key); ok {
		return existing.Allow()
	}

	return lim.Allow()
}
