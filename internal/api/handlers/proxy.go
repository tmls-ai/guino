package handlers

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
)

// ProxyHandler forwards HTTP and WebSocket traffic to a port inside a sandbox,
// through Guino's own authenticated control plane.
//
// Why this exists rather than publishing ports: Docker publishes to the loopback
// of whatever host the daemon runs on. When Guino runs inside a VM (how a
// desktop install ships, because the agent's toolchain is Linux) that loopback
// is the guest's, and the application on the real host cannot reach it. Only
// Guino's control port is forwarded out.
//
// Routing through the control plane also settles three other things at once:
//   - dynamic ports need no publishing, so POST /ports (which Docker cannot do
//     for a running container) stops being a missing feature;
//   - nothing is exposed on the host's network, so a published port cannot
//     become LAN-reachable by accident;
//   - the traffic inherits the control plane's authentication instead of being
//     an unauthenticated side channel.
type ProxyHandler struct {
	engine         *engine.Engine
	logger         *slog.Logger
	allowedOrigins []string
}

// NewProxyHandler creates a new ProxyHandler. allowedOrigins gates browser
// WebSocket upgrades exactly as the exec WebSocket does.
func NewProxyHandler(eng *engine.Engine, logger *slog.Logger, allowedOrigins []string) *ProxyHandler {
	return &ProxyHandler{engine: eng, logger: logger, allowedOrigins: allowedOrigins}
}

// proxyTransport dials sandboxes directly. Not http.DefaultTransport: that one
// honours HTTP_PROXY, which would send sandbox traffic (and request bodies) to
// an outside proxy on any machine that sets it.
var proxyTransport = &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
}

var (
	errInvalidPort = errors.New("invalid port")
	errNoAddress   = errors.New("sandbox has no address (not running?)")
)

// target resolves the sandbox's address for a requested port. The host is
// always the sandbox's own IP as reported by the runtime: the caller picks only
// the port, so this cannot be pointed at an arbitrary host.
func (h *ProxyHandler) target(r *http.Request, id, portStr string) (*url.URL, error) {
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, errInvalidPort
	}

	info, err := h.engine.Info(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if info.IP == "" {
		// A stopped sandbox has no address. Saying so beats a dial timeout.
		return nil, errNoAddress
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(info.IP, strconv.Itoa(port))}, nil
}

// originAllowed mirrors the exec WebSocket's check: non-browser clients send no
// Origin; a browser must come from an allowed origin. Without it any web page
// could open a sandbox's noVNC on a server running without auth.
func (h *ProxyHandler) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	for _, allowed := range h.allowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

// Proxy handles /api/v1/sandboxes/{id}/proxy/{port}/*.
//
// httputil.ReverseProxy carries both plain HTTP and WebSocket upgrades (noVNC is
// a WebSocket); it strips hop-by-hop headers and only switches protocols when the
// sandbox actually answers 101.
func (h *ProxyHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	portStr := chi.URLParam(r, "port")

	if !h.originAllowed(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}

	upstream, err := h.target(r, id, portStr)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrNotFound):
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errors.Is(err, errInvalidPort), errors.Is(err, errNoAddress):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			h.logger.Error("sandbox proxy lookup failed", "sandbox", id, "error", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	// Someone watching a preview or a VNC session is using this sandbox even
	// though no exec or file call is made. Held for the whole request, so a
	// WebSocket open longer than the idle timeout is not reaped under a live
	// screen.
	if sb, err := h.engine.GetSandbox(id); err == nil {
		defer sb.Hold()()
	}
	// Everything after /proxy/{port} is the sandbox's path. chi matches on the
	// escaped path when the request has one (an encoded "/" for example), so the
	// wildcard is escaped exactly when r.URL.RawPath is set.
	rest, rawRest := "/"+chi.URLParam(r, "*"), ""
	if r.URL.RawPath != "" {
		rawRest = rest
		if rest, err = url.PathUnescape(rawRest); err != nil {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
	}

	// A preview can stream (SSE, a long download) for longer than the server's
	// WriteTimeout, which exists for control-plane calls. Lift it for this
	// response only; the route is authenticated like every other.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			pr.Out.URL.Path = rest
			pr.Out.URL.RawPath = rawRest
			pr.Out.Host = upstream.Host
			// The sandbox must never see Guino's credentials.
			pr.Out.Header.Del("X-API-Key")
			pr.Out.Header.Del("Authorization")
		},
		Transport: proxyTransport,
		// Streaming responses (SSE, long downloads) must not be buffered.
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			h.logger.Debug("sandbox proxy upstream error", "sandbox", id, "port", portStr, "error", err)
			writeError(w, http.StatusBadGateway, "sandbox did not answer on that port")
		},
	}
	rp.ServeHTTP(w, r)
}
