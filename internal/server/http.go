package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/gabrielforster/lc/internal/registry"
)

// HTTPHandler routes HTTP requests to tunnels by Host header.
//
// HTTP is proxied at L7 rather than piped as bytes, because terminating TLS
// means the server already has the parsed request. Going through
// httputil.ReverseProxy gets correct forwarded headers, keep-alive reuse and
// WebSocket upgrades for free instead of reimplementing them over a pipe.
type HTTPHandler struct {
	srv   *Server
	proxy *httputil.ReverseProxy

	mu sync.Mutex
	// transports are cached per tunnel id. A transport pools connections, so
	// building one per request would discard keep-alive entirely.
	transports map[string]*http.Transport
}

func (s *Server) HTTPHandler() *HTTPHandler {
	h := &HTTPHandler{srv: s, transports: map[string]*http.Transport{}}
	h.proxy = &httputil.ReverseProxy{
		Rewrite:      h.rewrite,
		Transport:    h,
		ErrorHandler: h.onError,
	}
	return h
}

// tunnelKey carries the resolved tunnel from ServeHTTP to RoundTrip without
// threading it through headers, where a client could forge it.
type tunnelKey struct{}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tun, ok := h.srv.reg.LookupHost(r.Host)
	if !ok {
		http.Error(w, "no tunnel is serving this hostname", http.StatusBadGateway)
		return
	}
	h.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tunnelKey{}, tun)))
}

// rewrite sets the forwarded headers. ReverseProxy's SetXForwarded handles
// X-Forwarded-For, -Host and -Proto, including dropping any client-supplied
// value so it cannot be spoofed.
func (h *HTTPHandler) rewrite(pr *httputil.ProxyRequest) {
	pr.SetXForwarded()
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = pr.In.Host
	// The local service usually cares about the name the user typed, so the
	// original Host is preserved rather than rewritten to the backend address.
	pr.Out.Host = pr.In.Host

	if pr.In.TLS != nil {
		pr.Out.Header.Set("X-Forwarded-Proto", "https")
	}
	if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
		pr.Out.Header.Set("X-Real-IP", ip)
	}
}

// RoundTrip dispatches to the transport belonging to the request's tunnel.
func (h *HTTPHandler) RoundTrip(r *http.Request) (*http.Response, error) {
	tun, _ := r.Context().Value(tunnelKey{}).(*registry.Tunnel)
	if tun == nil {
		return nil, fmt.Errorf("server: request reached the proxy without a tunnel")
	}
	return h.transportFor(tun).RoundTrip(r)
}

func (h *HTTPHandler) transportFor(t *registry.Tunnel) *http.Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tr, ok := h.transports[t.ID]; ok {
		return tr
	}
	tr := &http.Transport{
		// The address is ignored: every connection for this tunnel is a fresh
		// yamux stream to the agent that registered it.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			client, _ := ctx.Value(clientAddrKey{}).(string)
			return Dial(t, client)
		},
		MaxIdleConns:        32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	h.transports[t.ID] = tr
	return tr
}

// clientAddrKey passes the real client address down to DialContext so the
// agent's StreamInit reports the browser, not the server.
type clientAddrKey struct{}

func (h *HTTPHandler) onError(w http.ResponseWriter, r *http.Request, err error) {
	h.srv.log.Warn("http proxy error", "host", r.Host, "err", err)
	http.Error(w, "tunnel unavailable", http.StatusBadGateway)
}

// ServeHTTPListener serves plain HTTP on ln until ctx is cancelled.
func (s *Server) ServeHTTPListener(ctx context.Context, ln net.Listener) error {
	h := s.HTTPHandler()
	srv := &http.Server{
		Handler: withClientAddr(h),
		// A slow or absent header line must not hold a connection open
		// indefinitely once this port is public.
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// withClientAddr puts the inbound remote address on the request context, where
// the tunnel dialer can reach it.
func withClientAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientAddrKey{}, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
