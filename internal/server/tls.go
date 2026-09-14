package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// CertSource supplies certificates to the HTTPS frontend.
//
// It is an interface with three implementations so the HTTPS path can be
// exercised locally, with no Let's Encrypt round trip, exactly as it runs in
// production.
type CertSource interface {
	TLSConfig() *tls.Config
	// HTTPHandler wraps the plain-HTTP handler, letting a source intercept the
	// ACME challenge path. Sources that need nothing return next unchanged.
	HTTPHandler(next http.Handler) http.Handler
}

// FileCerts serves a fixed certificate and key from disk.
type FileCerts struct{ CertFile, KeyFile string }

func (f FileCerts) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Loaded per handshake so replacing the files on disk takes effect
			// without a restart.
			cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	}
}

func (f FileCerts) HTTPHandler(next http.Handler) http.Handler { return next }

// AutocertSource obtains certificates from Let's Encrypt.
type AutocertSource struct{ mgr *autocert.Manager }

// NewAutocert builds a manager whose host allowlist is consulted per request.
//
// The allowlist reads live state rather than a static list because domains are
// claimed at runtime. It also means a certificate can only ever be requested
// for a name some token actually owns, so a public server cannot be used to
// mint certificates for arbitrary domains.
func NewAutocert(cacheDir string, allowed func(host string) bool) *AutocertSource {
	return &AutocertSource{mgr: &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Cache:  autocert.DirCache(cacheDir),
		HostPolicy: func(_ context.Context, host string) error {
			if !allowed(host) {
				return fmt.Errorf("server: %q is not claimed by any token", host)
			}
			return nil
		},
	}}
}

func (a *AutocertSource) TLSConfig() *tls.Config {
	cfg := a.mgr.TLSConfig()
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}

// HTTPHandler routes ACME HTTP-01 challenges, which must be answered on port 80
// while every other request proxies as usual.
func (a *AutocertSource) HTTPHandler(next http.Handler) http.Handler {
	return a.mgr.HTTPHandler(next)
}

// SelfSigned generates an in-memory CA and issues certificates on demand.
//
// Its purpose is development: it exercises the real TLS path without needing
// DNS or a public address. Clients must be told to trust RootPEM, so this is
// useless for anything a browser will visit unprompted.
type SelfSigned struct {
	caCert  *x509.Certificate
	caKey   *ecdsa.PrivateKey
	RootPEM []byte
}

func NewSelfSigned() (*SelfSigned, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lc development CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &SelfSigned{caCert: caCert, caKey: key, RootPEM: der}, nil
}

// RootCAs returns a pool trusting this CA, for test clients.
func (s *SelfSigned) RootCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.caCert)
	return pool
}

func (s *SelfSigned) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.issue(hello.ServerName)
		},
	}
}

func (s *SelfSigned) HTTPHandler(next http.Handler) http.Handler { return next }

// issue mints a leaf for one server name.
func (s *SelfSigned) issue(name string) (*tls.Certificate, error) {
	if name == "" {
		name = "localhost"
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, &key.PublicKey, s.caKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, s.caCert.Raw},
		PrivateKey:  key,
		Leaf:        tmpl,
	}, nil
}

// ServeHTTPSListener terminates TLS on ln and proxies as the HTTP frontend does.
func (s *Server) ServeHTTPSListener(ctx context.Context, ln net.Listener, certs CertSource) error {
	h := s.HTTPHandler()
	srv := &http.Server{
		Handler:           withClientAddr(h),
		TLSConfig:         certs.TLSConfig(),
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	// The certificates come from TLSConfig, so no files are named here.
	if err := srv.ServeTLS(ln, "", ""); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
