package handlers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

// sandboxApp stands in for a server running inside the sandbox: it echoes what
// it received and speaks just enough WebSocket handshake to echo raw bytes.
func sandboxApp(t *testing.T) (port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = buf.Flush()
			_, _ = io.Copy(conn, buf)
			return
		}
		_, _ = fmt.Fprintf(w, "path=%s raw=%s query=%s key=%q auth=%q",
			r.URL.Path, r.URL.RawPath, r.URL.RawQuery, r.Header.Get("X-API-Key"), r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Port()
}

// proxyServer serves the proxy routes as server.go registers them, on a real
// listener so connections can be hijacked.
func proxyServer(t *testing.T) (*httptest.Server, string) {
	srv, id, _ := proxyServerWithEngine(t)
	return srv, id
}

func proxyServerWithEngine(t *testing.T) (*httptest.Server, string, *engine.Engine) {
	t.Helper()
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	mock := enginetest.NewMockRuntime()
	mock.IP = "127.0.0.1"
	eng := engine.NewEngine(mock, st, config.SandboxConfig{
		DefaultImage: "test:latest", DefaultTimeout: 10 * time.Minute, MaxSandboxes: 5,
	}, config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
	require.NoError(t, err)

	ph := NewProxyHandler(eng, slog.Default(), []string{"http://allowed.example"})
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.HandleFunc("/sandboxes/{id}/proxy/{port}", ph.Proxy)
		r.HandleFunc("/sandboxes/{id}/proxy/{port}/*", ph.Proxy)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, sb.ID, eng
}

func TestProxyForwardsPathQueryAndStripsCredentials(t *testing.T) {
	app := sandboxApp(t)
	srv, id := proxyServer(t)

	req, err := http.NewRequest("GET", srv.URL+"/api/v1/sandboxes/"+id+"/proxy/"+app+"/a/b%2Fc?x=1", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "secret")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `path=/a/b/c raw=/a/b%2Fc query=x=1 key="" auth=""`, string(body))
}

func TestProxyBarePortIsRoot(t *testing.T) {
	app := sandboxApp(t)
	srv, id := proxyServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/sandboxes/" + id + "/proxy/" + app)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	assert.True(t, strings.HasPrefix(string(body), "path=/ "), string(body))
}

func TestProxyRefusals(t *testing.T) {
	srv, id := proxyServer(t)

	// A port nothing listens on: grab one and release it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	cases := map[string]int{
		"/api/v1/sandboxes/nope/proxy/80/":                   http.StatusNotFound,
		"/api/v1/sandboxes/" + id + "/proxy/0/":              http.StatusBadRequest,
		"/api/v1/sandboxes/" + id + "/proxy/70000/":          http.StatusBadRequest,
		"/api/v1/sandboxes/" + id + "/proxy/x/":              http.StatusBadRequest,
		"/api/v1/sandboxes/" + id + "/proxy/" + closed + "/": http.StatusBadGateway,
	}
	for path, want := range cases {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, want, resp.StatusCode, path)
	}
}

// wsHandshake opens a raw upgrade through the proxy and returns the response.
func wsHandshake(t *testing.T, srv *httptest.Server, id, app, origin string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	originHeader := ""
	if origin != "" {
		originHeader = "Origin: " + origin + "\r\n"
	}
	_, err = fmt.Fprintf(conn, "GET /api/v1/sandboxes/%s/proxy/%s/websockify HTTP/1.1\r\n"+
		"Host: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n%s"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", id, app, originHeader)
	require.NoError(t, err)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	return conn, br, resp
}

func TestProxyCarriesWebSocket(t *testing.T) {
	app := sandboxApp(t)
	srv, id := proxyServer(t)

	conn, br, resp := wsHandshake(t, srv, id, app, "")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	_, err := conn.Write([]byte("ping"))
	require.NoError(t, err)
	got := make([]byte, 4)
	_, err = io.ReadFull(br, got)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(got))
}

// A browser on another site must not open a sandbox's WebSocket (noVNC) through
// a server running without auth.
func TestProxyWebSocketChecksOrigin(t *testing.T) {
	app := sandboxApp(t)
	srv, id := proxyServer(t)

	_, _, resp := wsHandshake(t, srv, id, app, "http://evil.example")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	_, _, resp = wsHandshake(t, srv, id, app, "http://allowed.example")
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
}

// A proxied request holds the sandbox for its whole duration (a noVNC socket can
// outlive the idle timeout), and finishing it counts as activity.
func TestProxyHoldsTheSandboxWhileOpen(t *testing.T) {
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	mock := enginetest.NewMockRuntime()
	mock.IP = "127.0.0.1"
	eng := engine.NewEngine(mock, st, config.SandboxConfig{
		DefaultImage: "test:latest", DefaultTimeout: 10 * time.Minute, MaxSandboxes: 5,
	}, config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Timeout: 20 * time.Millisecond})
	require.NoError(t, err)

	expiredDuringRequest := make(chan bool, 1)
	app := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(60 * time.Millisecond) // well past the 20ms idle window
		expiredDuringRequest <- sb.IsExpired()
	}))
	t.Cleanup(app.Close)
	u, err := url.Parse(app.URL)
	require.NoError(t, err)

	ph := NewProxyHandler(eng, slog.Default(), nil)
	r := chi.NewRouter()
	r.HandleFunc("/api/v1/sandboxes/{id}/proxy/{port}/*", ph.Proxy)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/sandboxes/" + sb.ID + "/proxy/" + u.Port() + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.False(t, <-expiredDuringRequest, "the sandbox expired while a proxied request was open")
	assert.False(t, sb.IsExpired(), "finishing the request must count as activity")
}
