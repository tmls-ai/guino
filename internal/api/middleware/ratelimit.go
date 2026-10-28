package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// maxLimiterEntries caps the number of tracked rate limiter entries
	// to prevent memory exhaustion from large IP address spaces (IPv6).
	maxLimiterEntries = 100_000
)

// limiterEntry wraps a rate.Limiter with its last access time for TTL-based cleanup.
type limiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// RateLimiter provides per-key rate limiting.
//
// Proxied sandbox traffic gets its own bucket and its own limits. It is a
// different class of request from a control-plane call: creating a sandbox is one
// request, while opening a noVNC session or a preview page is dozens of asset
// requests in a burst. Sharing one bucket means the preview rate-limits itself the
// moment it starts working, and raising the shared limit to compensate would
// weaken the protection on the operations that actually matter.
type RateLimiter struct {
	mu         sync.Mutex
	limiters   map[string]*limiterEntry
	rate       rate.Limit
	burst      int
	proxyRate  rate.Limit
	proxyBurst int
	stopCh     chan struct{}
	closeOnce  sync.Once
}

// NewRateLimiter creates a new rate limiter with a cleanup goroutine.
// proxyRPS/proxyBurst apply to sandbox proxy traffic; 0 in either reuses
// rps/burst (a zero burst would otherwise refuse every proxied request).
func NewRateLimiter(rps float64, burst int, proxyRPS float64, proxyBurst int) *RateLimiter {
	if proxyRPS <= 0 || proxyBurst <= 0 {
		proxyRPS, proxyBurst = rps, burst
	}
	rl := &RateLimiter{
		limiters:   make(map[string]*limiterEntry),
		rate:       rate.Limit(rps),
		burst:      burst,
		proxyRate:  rate.Limit(proxyRPS),
		proxyBurst: proxyBurst,
		stopCh:     make(chan struct{}),
	}

	// Cleanup stale entries periodically (TTL-based, not full reset)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.mu.Lock()
				cutoff := time.Now().Add(-10 * time.Minute)
				for key, entry := range rl.limiters {
					if entry.lastAccess.Before(cutoff) {
						delete(rl.limiters, key)
					}
				}
				rl.mu.Unlock()
			case <-rl.stopCh:
				return
			}
		}
	}()

	return rl
}

// Close stops the cleanup goroutine. Safe to call multiple times.
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() { close(rl.stopCh) })
}

// isProxyPath reports whether a request is proxied sandbox traffic.
// Matched on the path rather than injected by the router because the limiter runs
// above routing — chi has not resolved the pattern yet at this point.
func isProxyPath(path string) bool {
	const prefix = "/api/v1/sandboxes/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	id, _, found := strings.Cut(rest, "/")
	return found && id != "" && strings.HasPrefix(rest[len(id):], "/proxy/")
}

func (rl *RateLimiter) getLimiter(key string, limit rate.Limit, burst int) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.limiters[key]
	if !exists {
		// Cap the number of tracked entries to prevent memory exhaustion
		if len(rl.limiters) >= maxLimiterEntries {
			// Return a restrictive limiter without storing it
			return rate.NewLimiter(rate.Limit(0.1), 1)
		}
		entry = &limiterEntry{
			limiter: rate.NewLimiter(limit, burst),
		}
		rl.limiters[key] = entry
	}
	entry.lastAccess = time.Now()
	return entry.limiter
}

// Middleware returns a rate limiting middleware.
// Always rate limits by IP address to prevent key manipulation attacks.
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Always use IP as rate limit key to prevent attackers from
			// sending arbitrary X-API-Key values to get separate buckets.
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !isProxyPath(r.URL.Path) {
				if !rl.getLimiter("ip:"+host, rl.rate, rl.burst).Allow() {
					tooMany(w)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// The generous proxy bucket is picked by path, before auth has run.
			// Failed auth on a proxy path is charged to a bucket with the strict
			// control-plane limits, and an exhausted one refuses: guessing keys
			// through /proxy/ is no faster than anywhere else. Its own key, so
			// busy control-plane traffic never throttles a working preview.
			// Only Auth's own rejection counts (it sets the flag), never a 401
			// the sandbox app returns through the proxy.
			// ponytail: check-then-spend is not atomic; a few concurrent guesses
			// can pass on an empty bucket, which does not change the rate.
			strict := rl.getLimiter("authfail:"+host, rl.rate, rl.burst)
			if strict.Tokens() < 1 || !rl.getLimiter("proxy:"+host, rl.proxyRate, rl.proxyBurst).Allow() {
				tooMany(w)
				return
			}
			failed := new(bool)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authFailedKey{}, failed)))
			if *failed {
				strict.Allow()
			}
		})
	}
}

func tooMany(w http.ResponseWriter) {
	http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
}

// authFailedKey carries a *bool that Auth sets when it rejects a request.
type authFailedKey struct{}

// markAuthFailed records an auth rejection for the rate limiter, if it asked.
func markAuthFailed(r *http.Request) {
	if p, ok := r.Context().Value(authFailedKey{}).(*bool); ok {
		*p = true
	}
}
