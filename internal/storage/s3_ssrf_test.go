package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	serverconfig "github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
)

// TestResolveS3Credentials_GateB_and_overrides pins the resolution matrix that
// transitively covers all three client-construction paths (engine s3 hooks and
// the two API handlers all funnel through ResolveS3Credentials):
//
//   - Gate B refuses a per-sandbox endpoint override while the internal
//     exemption is active (the exemption is pinned to ONE server endpoint).
//   - Bucket/credential overrides remain permitted even with the exemption on
//     (they change the object namespace, not the network host).
//   - AllowInternalEndpoint is threaded into the resolved credentials.
func TestResolveS3Credentials_GateB_and_overrides(t *testing.T) {
	base := serverconfig.S3Config{
		Endpoint:  "http://minio:9000",
		Region:    "us-east-1",
		AccessKey: "server-ak",
		SecretKey: "server-sk",
	}

	t.Run("Gate B refuses per-sandbox endpoint override when exemption active", func(t *testing.T) {
		srv := base
		srv.AllowInternalEndpoint = true
		_, err := ResolveS3Credentials(&runtime.S3SyncConfig{
			Endpoint: "http://attacker:9000", Bucket: "b",
		}, srv)
		if err == nil || !strings.Contains(err.Error(), "per-sandbox S3 endpoint override is refused") {
			t.Fatalf("want Gate B refusal, got %v", err)
		}
	})

	t.Run("identical per-sandbox endpoint is not an override", func(t *testing.T) {
		srv := base
		srv.AllowInternalEndpoint = true
		creds, err := ResolveS3Credentials(&runtime.S3SyncConfig{
			Endpoint: "http://minio:9000", Bucket: "b",
		}, srv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !creds.AllowInternalEndpoint {
			t.Error("AllowInternalEndpoint not threaded into creds")
		}
		if creds.Endpoint != "http://minio:9000" {
			t.Errorf("Endpoint = %q, want server endpoint", creds.Endpoint)
		}
	})

	t.Run("bucket and credential overrides permitted with exemption active", func(t *testing.T) {
		srv := base
		srv.AllowInternalEndpoint = true
		creds, err := ResolveS3Credentials(&runtime.S3SyncConfig{
			Bucket: "sandbox-bucket", AccessKey: "sb-ak", SecretKey: "sb-sk",
		}, srv)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if creds.Bucket != "sandbox-bucket" || creds.AccessKey != "sb-ak" || creds.SecretKey != "sb-sk" {
			t.Errorf("overrides not applied: %+v", creds)
		}
	})

	t.Run("per-sandbox endpoint override permitted when exemption OFF (back-compat)", func(t *testing.T) {
		creds, err := ResolveS3Credentials(&runtime.S3SyncConfig{
			Endpoint: "https://s3.other.example", Bucket: "b",
		}, base)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if creds.Endpoint != "https://s3.other.example" {
			t.Errorf("Endpoint = %q, want sandbox override", creds.Endpoint)
		}
		if creds.AllowInternalEndpoint {
			t.Error("AllowInternalEndpoint must be false when server flag off")
		}
	})

	t.Run("missing bucket is an error", func(t *testing.T) {
		if _, err := ResolveS3Credentials(nil, base); err == nil {
			t.Fatal("want bucket-required error, got nil")
		}
	})
}

// TestSSRFPinnedHTTPClient_NeverExemptIsHardError proves a configured endpoint
// whose construction-time IP set touches a crown-jewel range fails fast at
// client construction — not at the first object operation — regardless of the
// exemption flag.
func TestSSRFPinnedHTTPClient_NeverExemptIsHardError(t *testing.T) {
	for _, ep := range []string{
		"http://169.254.169.254:80",   // cloud metadata
		"http://[fe80::1]:9000",       // link-local
		"http://169.254.169.253:9000", // Azure alt metadata
	} {
		if _, err := ssrfPinnedHTTPClient(context.Background(), ep, true); err == nil ||
			!strings.Contains(err.Error(), "never-exempt") {
			t.Errorf("ssrfPinnedHTTPClient(%q, allow=true) = %v, want never-exempt hard error", ep, err)
		}
	}
}

// TestSSRFPinnedHTTPClient_LoopbackBlockedUnlessExempt drives a real loopback
// listener: the construction-time pin succeeds (loopback is exemptable, not a
// crown jewel) but the dial is refused unless the operator exemption is on.
func TestSSRFPinnedHTTPClient_LoopbackBlockedUnlessExempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Exemption OFF: client constructs (127.0.0.1 is not never-exempt) but the
	// dialer refuses every pinned address.
	off, err := ssrfPinnedHTTPClient(context.Background(), srv.URL, false)
	if err != nil {
		t.Fatalf("construction with allow=false must still succeed: %v", err)
	}
	if _, err := off.Get(srv.URL); err == nil ||
		!strings.Contains(err.Error(), "no pinned endpoint address is dialable") {
		t.Fatalf("allow=false dial: want SSRF refusal, got %v", err)
	}

	// Exemption ON: the pinned loopback address is now dialable.
	on, err := ssrfPinnedHTTPClient(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatalf("construction with allow=true: %v", err)
	}
	resp, err := on.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow=true dial must succeed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestSSRFPinnedHTTPClient_FrozenSetNoReresolution is the DNS-rebind proof: the
// dialer dials ONLY the construction-time pinned IP and never resolves the
// per-request host. Requesting a host that does not exist still reaches the
// server because the bogus host is never looked up.
func TestSSRFPinnedHTTPClient_FrozenSetNoReresolution(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pin to the server's loopback address with the exemption on.
	client, err := ssrfPinnedHTTPClient(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	// Ask for a host that would NXDOMAIN if resolved. The frozen pin means the
	// dialer ignores it and dials the pinned loopback IP on the same port.
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	resp, err := client.Get("http://rebind-target.invalid:" + port + "/")
	if err != nil {
		t.Fatalf("frozen-set request failed (host was re-resolved?): %v", err)
	}
	_ = resp.Body.Close()
	if !hit || resp.StatusCode != http.StatusOK {
		t.Errorf("server not reached via pinned IP (hit=%v status=%d)", hit, resp.StatusCode)
	}
}

// TestSSRFPinnedHTTPClient_RedirectRevalidated proves CheckRedirect re-runs the
// SSRF predicate on every hop: a 3xx to a cloud-metadata host is refused even
// though the original endpoint was a permitted pinned loopback.
func TestSSRFPinnedHTTPClient_RedirectRevalidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client, err := ssrfPinnedHTTPClient(context.Background(), srv.URL, true)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	_, err = client.Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "blocked S3 redirect") {
		t.Fatalf("redirect to metadata: want blocked-redirect error, got %v", err)
	}
}
