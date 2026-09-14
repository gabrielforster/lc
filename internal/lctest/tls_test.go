package lctest

import (
	"context"
	"crypto/tls"
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

// The HTTPS path must work exactly as in production, minus the ACME round trip.
func TestHTTPSTerminatesAndProxies(t *testing.T) {
	var gotProto, gotXFF string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotXFF = r.Header.Get("X-Forwarded-For")
		fmt.Fprint(w, "over tls")
	}))
	t.Cleanup(local.Close)

	h := Start(t, Options{
		HTTPS:  true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "secure.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "secure.example.com",
			LocalAddr: strings.TrimPrefix(local.URL, "http://"),
		}},
	})

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: h.Certs.RootCAs()},
			// The certificate is issued for the claimed hostname, so the
			// connection is steered to the listener while SNI stays honest.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, h.HTTPSAddr)
			},
		},
	}
	resp, err := client.Get("https://secure.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "over tls" {
		t.Fatalf("body %q", body)
	}
	// The local service must be able to tell the user arrived over HTTPS.
	if gotProto != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotXFF == "" {
		t.Fatal("X-Forwarded-For missing on the TLS path")
	}
}

// A certificate must only be issued for a name the server actually serves.
func TestSelfSignedIssuesPerSNI(t *testing.T) {
	h := Start(t, Options{
		HTTPS:  true,
		Grants: []store.Grant{{Kind: store.GrantHost, Value: "secure.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "web", Kind: muxproto.KindHTTP, Host: "secure.example.com",
			LocalAddr: EchoService(t),
		}},
	})

	conn, err := tls.Dial("tcp", h.HTTPSAddr, &tls.Config{
		RootCAs:    h.Certs.RootCAs(),
		ServerName: "secure.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	leaf := conn.ConnectionState().PeerCertificates[0]
	if leaf.Subject.CommonName != "secure.example.com" {
		t.Fatalf("certificate issued for %q", leaf.Subject.CommonName)
	}
}
