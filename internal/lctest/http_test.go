package lctest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

// get issues a request to the public frontend with an explicit Host, which is
// how a browser reaching the real hostname would look.
func get(t *testing.T, h *Harness, host, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+h.HTTPAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestHTTPRoutesByHostAndForwardsClientIP(t *testing.T) {
	var gotXFF, gotHost string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotHost = r.Host
		fmt.Fprint(w, "served locally")
	}))
	t.Cleanup(local.Close)

	h := Start(t, Options{
		HTTP:   true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "web.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP,
			Host:      "web.example.com",
			LocalAddr: strings.TrimPrefix(local.URL, "http://"),
		}},
	})

	resp, body := get(t, h, "web.example.com", "/")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body != "served locally" {
		t.Fatalf("body %q", body)
	}
	// Without forwarding, the local service would see the tunnel, not the user.
	if gotXFF == "" || strings.HasPrefix(gotXFF, "127.0.0.1,") {
		t.Fatalf("X-Forwarded-For = %q, want the real client address", gotXFF)
	}
	// The service should see the name the user typed, not the backend address.
	if gotHost != "web.example.com" {
		t.Fatalf("Host = %q, want web.example.com", gotHost)
	}
}

// A client must not be able to forge its apparent origin.
func TestClientSuppliedForwardedForIsReplaced(t *testing.T) {
	var gotXFF string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
	}))
	t.Cleanup(local.Close)

	h := Start(t, Options{
		HTTP:   true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "web.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "web.example.com",
			LocalAddr: strings.TrimPrefix(local.URL, "http://"),
		}},
	})

	req, _ := http.NewRequest("GET", "http://"+h.HTTPAddr+"/", nil)
	req.Host = "web.example.com"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if strings.HasPrefix(gotXFF, "203.0.113.7") {
		t.Fatalf("X-Forwarded-For = %q: client-supplied value was trusted", gotXFF)
	}
}

func TestUnknownHostIsBadGateway(t *testing.T) {
	h := Start(t, Options{
		HTTP:   true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "web.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "web.example.com",
			LocalAddr: EchoService(t),
		}},
	})
	resp, _ := get(t, h, "nobody.example.com", "/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

// A dead local service must surface as a gateway error, not a hang.
func TestDeadLocalServiceIsBadGateway(t *testing.T) {
	// Bind then immediately release a port so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	h := Start(t, Options{
		HTTP:   true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "web.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "web.example.com", LocalAddr: dead,
		}},
	})
	resp, _ := get(t, h, "web.example.com", "/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

// Keep-alive reuse is why HTTP is proxied at L7 rather than piped.
func TestKeepAliveReusesTunnelStream(t *testing.T) {
	var requests int
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprintf(w, "request %d", requests)
	}))
	t.Cleanup(local.Close)

	h := Start(t, Options{
		HTTP:   true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "web.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "web.example.com",
			LocalAddr: strings.TrimPrefix(local.URL, "http://"),
		}},
	})

	for i := 1; i <= 3; i++ {
		_, body := get(t, h, "web.example.com", "/")
		if body != fmt.Sprintf("request %d", i) {
			t.Fatalf("got %q on request %d", body, i)
		}
	}
}
