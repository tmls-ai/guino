package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Proxy classification decides which bucket a request is charged to, so a pattern
// that is too loose hands an attacker the generous limit for control-plane calls —
// exactly the "arbitrary key" hole the IP-only keying was written to close.
func TestIsProxyPath(t *testing.T) {
	proxy := []string{
		"/api/v1/sandboxes/abc123/proxy/8080/",
		"/api/v1/sandboxes/abc123/proxy/8080",
		"/api/v1/sandboxes/abc123/proxy/6080/websockify",
		"/api/v1/sandboxes/abc123/proxy/8080/assets/app.js?v=2",
	}
	control := []string{
		"/api/v1/sandboxes",
		"/api/v1/sandboxes/abc123",
		"/api/v1/sandboxes/abc123/exec",
		"/api/v1/sandboxes/abc123/sessions/s/exec",
		"/api/v1/sandboxes//proxy/8080/",       // empty id
		"/api/v1/sandboxes/abc123/proxyfoo/80", // prefix lookalike
		"/api/v1/snapshots/abc/proxy/8080/",
		"/proxy/8080/",
		"/api/v1/health",
	}
	for _, p := range proxy {
		if !isProxyPath(p) {
			t.Errorf("expected proxy classification for %q", p)
		}
	}
	for _, p := range control {
		if isProxyPath(p) {
			t.Errorf("expected control-plane classification for %q", p)
		}
	}
}

// The whole point of the separate bucket: a burst that would 429 the control plane
// must sail through the proxy, and the two must not drain each other.
func TestProxyBurstDoesNotExhaustControlPlane(t *testing.T) {
	rl := NewRateLimiter(2, 2, 100, 100)
	defer rl.Close()

	handler := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	call := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 40 asset requests, the shape of loading a preview page.
	for i := 0; i < 40; i++ {
		if code := call("/api/v1/sandboxes/s1/proxy/8080/asset.js"); code != http.StatusOK {
			t.Fatalf("proxy request %d rejected with %d; the preview would break", i, code)
		}
	}

	// The control-plane bucket is untouched by that burst.
	if code := call("/api/v1/sandboxes"); code != http.StatusOK {
		t.Fatalf("control-plane request rejected with %d after proxy burst", code)
	}

	// And the control plane is still actually limited — burst 2, one already spent.
	call("/api/v1/sandboxes")
	if code := call("/api/v1/sandboxes"); code != http.StatusTooManyRequests {
		t.Fatalf("control plane returned %d; the limit is not being enforced", code)
	}

	// The proxy bucket survived the control plane hitting its limit.
	if code := call("/api/v1/sandboxes/s1/proxy/8080/asset.js"); code != http.StatusOK {
		t.Fatalf("proxy rejected with %d after the control plane was throttled", code)
	}
}

// Zero means "reuse the control-plane limits", so an operator who never sets the new
// keys gets the old behaviour rather than an accidental unlimited proxy.
func TestProxyLimitsDefaultToControlPlane(t *testing.T) {
	rl := NewRateLimiter(2, 2, 0, 0)
	defer rl.Close()

	handler := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var last int
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/s1/proxy/8080/x", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected the proxy to inherit the strict limit, got %d", last)
	}
}

// A zero proxy burst with a non-zero rate would refuse every proxied request.
func TestZeroProxyBurstFallsBack(t *testing.T) {
	rl := NewRateLimiter(10, 20, 200, 0)
	defer rl.Close()
	handler := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/s1/proxy/8080/x", nil)
	req.RemoteAddr = "10.0.0.3:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first proxied request got %d", rec.Code)
	}
}

// The proxy bucket is chosen before auth runs, so failed auth there must spend
// the strict bucket: key guessing via /proxy/ is capped at the control-plane rate.
// Authenticated proxy traffic must not spend it.
func TestProxyAuthFailuresSpendTheStrictBucket(t *testing.T) {
	rl := NewRateLimiter(1, 2, 200, 400)
	defer rl.Close()
	var doPath func(ip, key, path string) int
	handler := rl.Middleware()(Auth([]string{"good"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sandbox app has a login page of its own: its 401s are not auth
		// failures and must never lock the preview.
		if r.URL.Path == "/api/v1/sandboxes/s1/proxy/8080/login" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))
	do := func(ip, key string) int { return doPath(ip, key, "/api/v1/sandboxes/s1/proxy/8080/x") }
	doPath = func(ip, key, path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 0; i < 50; i++ {
		if got := do("10.0.0.4", "good"); got != http.StatusOK {
			t.Fatalf("authenticated proxy request %d got %d", i, got)
		}
	}
	for i := 0; i < 50; i++ {
		if got := doPath("10.0.0.6", "good", "/api/v1/sandboxes/s1/proxy/8080/login"); got != http.StatusUnauthorized {
			t.Fatalf("app login request %d got %d", i, got)
		}
	}
	if got := do("10.0.0.6", "good"); got != http.StatusOK {
		t.Fatalf("the app's own 401s throttled the preview: %d", got)
	}
	codes := []int{do("10.0.0.5", "bad"), do("10.0.0.5", "bad"), do("10.0.0.5", "bad")}
	if codes[0] != http.StatusUnauthorized || codes[1] != http.StatusUnauthorized || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("guessing was not capped at the strict burst: %v", codes)
	}
}
