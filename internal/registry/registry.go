// Package registry sits between the control protocol and the store. It decides
// what a token is allowed to claim, and tracks which agent session currently
// serves each live tunnel.
//
// The split is deliberate: the store holds durable ownership, the registry
// holds "who is connected right now". A restart forgets the latter and keeps
// the former.
package registry

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
	"github.com/hashicorp/yamux"
)

// Config is the server policy the registry enforces.
type Config struct {
	// PortMin and PortMax bound automatic public port assignment.
	PortMin, PortMax int
	// AllowCustomDomains lets an agent claim a hostname it was not granted,
	// first-come-first-served.
	AllowCustomDomains bool
	// ReservedHosts are names the server uses itself and will never hand out.
	ReservedHosts []string
	// PublicHost is the address users reach the server on, used to render the
	// public address of a tcp tunnel.
	PublicHost string
}

// Registry is safe for concurrent use.
type Registry struct {
	db  *store.DB
	cfg Config

	mu sync.RWMutex
	// byID and byHost index the same tunnels; byHost only holds those with a
	// hostname, and byPort only those with a public port.
	byID   map[string]*Tunnel
	byHost map[string]*Tunnel
	byPort map[int]*Tunnel
}

// Tunnel is a live, registered tunnel backed by a connected agent.
type Tunnel struct {
	ID         string
	Name       string
	Kind       muxproto.Kind
	Host       string
	PublicPort int
	TokenID    int64

	// Session is the agent's yamux session. Opening a stream on it reaches the
	// agent that registered this tunnel.
	Session *yamux.Session
}

func New(db *store.DB, cfg Config) *Registry {
	return &Registry{
		db:     db,
		cfg:    cfg,
		byID:   map[string]*Tunnel{},
		byHost: map[string]*Tunnel{},
		byPort: map[int]*Tunnel{},
	}
}

func (r *Registry) Config() Config { return r.cfg }

// Authenticate resolves a token secret.
func (r *Registry) Authenticate(secret string) (store.Token, error) {
	return r.db.TokenBySecret(secret)
}

var (
	// ErrForbidden means the token holds no grant covering the request.
	ErrForbidden = errors.New("registry: not permitted by any grant")
	// ErrHostBusy means another connected agent is already serving the host.
	ErrHostBusy = errors.New("registry: host already served by a live tunnel")
)

// Register validates a tunnel spec against the token's grants and durable
// claims, reserves what it needs, and publishes it as live.
func (r *Registry) Register(tok store.Token, sess *yamux.Session, spec muxproto.TunnelSpec, id string) (*Tunnel, error) {
	t := &Tunnel{
		ID:      id,
		Name:    spec.Name,
		Kind:    spec.Kind,
		TokenID: tok.ID,
		Session: sess,
	}

	switch spec.Kind {
	case muxproto.KindHTTP, muxproto.KindMinecraft:
		host := normalizeHost(spec.Host)
		if host == "" {
			return nil, fmt.Errorf("%w: %s tunnel needs a host", muxproto.ErrUnexpected, spec.Kind)
		}
		if err := r.authorizeHost(tok, host); err != nil {
			return nil, err
		}
		t.Host = host

	case muxproto.KindTCP:
		if err := r.authorizePort(tok, spec.PublicPort); err != nil {
			return nil, err
		}
		port, err := r.db.ReservePort(tok.ID, spec.Name, spec.PublicPort, r.cfg.PortMin, r.cfg.PortMax)
		if err != nil {
			return nil, err
		}
		t.PublicPort = port

	default:
		return nil, fmt.Errorf("registry: unknown tunnel kind %q", spec.Kind)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if t.Host != "" {
		if live, ok := r.byHost[t.Host]; ok && live.ID != t.ID {
			return nil, ErrHostBusy
		}
		r.byHost[t.Host] = t
	}
	if t.PublicPort != 0 {
		r.byPort[t.PublicPort] = t
	}
	r.byID[t.ID] = t
	return t, nil
}

// authorizeHost allows a host if a grant covers it, the token already owns it,
// or custom domains are on and it is free.
func (r *Registry) authorizeHost(tok store.Token, host string) error {
	grants, err := r.db.Grants(tok.ID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		switch g.Kind {
		case store.GrantHost:
			if normalizeHost(g.Value) == host {
				return nil
			}
		case store.GrantWildcard:
			if hostInZone(host, g.Value) {
				return nil
			}
		}
	}

	// A durable claim from an earlier session is as good as a grant.
	owner, err := r.db.DomainOwner(host)
	if err == nil {
		if owner == tok.ID {
			return nil
		}
		return ErrForbidden
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if _, reason := r.checkClaimable(host); reason == muxproto.ClaimOK {
		return r.db.ClaimDomain(tok.ID, host)
	}
	return ErrForbidden
}

func (r *Registry) authorizePort(tok store.Token, want int) error {
	grants, err := r.db.Grants(tok.ID)
	if err != nil {
		return err
	}
	for _, g := range grants {
		switch g.Kind {
		case store.GrantPortAuto:
			// Auto covers assignment; an explicit request still needs a match.
			if want == 0 {
				return nil
			}
		case store.GrantPort:
			if g.Value == fmt.Sprint(want) {
				return nil
			}
		}
	}
	// A port already reserved for this token stays available to it.
	if want != 0 {
		ports, err := r.db.Ports(tok.ID)
		if err != nil {
			return err
		}
		for _, p := range ports {
			if p.Port == want {
				return nil
			}
		}
	}
	return ErrForbidden
}

// Claim handles a runtime ClaimDomain request.
func (r *Registry) Claim(tok store.Token, host string) muxproto.ClaimResult {
	host = normalizeHost(host)
	if owner, err := r.db.DomainOwner(host); err == nil && owner == tok.ID {
		return muxproto.ClaimResult{OK: true, NeedsDNS: true} // already ours
	}
	if ok, reason := r.checkClaimable(host); !ok {
		return muxproto.ClaimResult{Reason: reason}
	}
	if err := r.db.ClaimDomain(tok.ID, host); err != nil {
		if errors.Is(err, store.ErrTaken) {
			return muxproto.ClaimResult{Reason: muxproto.ClaimTaken}
		}
		return muxproto.ClaimResult{Reason: muxproto.ClaimInvalid}
	}
	// The claim only records intent here; the name resolves, and a certificate
	// can be issued, once the user points DNS at this server.
	return muxproto.ClaimResult{OK: true, NeedsDNS: true}
}

// checkClaimable reports whether host may be claimed, and why not if it cannot.
func (r *Registry) checkClaimable(host string) (bool, muxproto.ClaimReason) {
	if !r.cfg.AllowCustomDomains {
		return false, muxproto.ClaimDisabled
	}
	if !validHost(host) {
		return false, muxproto.ClaimInvalid
	}
	for _, res := range r.cfg.ReservedHosts {
		if normalizeHost(res) == host {
			return false, muxproto.ClaimReserved
		}
	}
	if _, err := r.db.DomainOwner(host); err == nil {
		return false, muxproto.ClaimTaken
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, muxproto.ClaimInvalid
	}
	return true, muxproto.ClaimOK
}

// Release drops a live tunnel, called when its agent disconnects. The durable
// domain and port claims survive, so the agent gets them back on reconnect.
func (r *Registry) Release(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, t.ID)
	if live, ok := r.byHost[t.Host]; ok && live.ID == t.ID {
		delete(r.byHost, t.Host)
	}
	if live, ok := r.byPort[t.PublicPort]; ok && live.ID == t.ID {
		delete(r.byPort, t.PublicPort)
	}
}

// LookupHost finds the live tunnel serving a hostname.
func (r *Registry) LookupHost(host string) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byHost[normalizeHost(host)]
	return t, ok
}

// LookupPort finds the live tunnel behind a public port.
func (r *Registry) LookupPort(port int) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byPort[port]
	return t, ok
}

// AllowedDomains is the autocert allowlist: a certificate may only be requested
// for a name some token actually owns.
func (r *Registry) AllowedDomains() ([]string, error) { return r.db.AllDomains() }

// normalizeHost lowercases and strips any port, so routing keys compare equal
// regardless of how a client spelled them.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// hostInZone reports whether host sits under a wildcard zone such as
// ".mc.example.com". The zone itself does not match, only names below it.
func hostInZone(host, zone string) bool {
	zone = normalizeHost(zone)
	if !strings.HasPrefix(zone, ".") {
		zone = "." + zone
	}
	return strings.HasSuffix(host, zone) && len(host) > len(zone)
}

// validHost applies a conservative hostname check. It is stricter than DNS
// allows, which is fine: these names are typed by people into config.
func validHost(h string) bool {
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			isAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' {
				return false
			}
		}
	}
	return true
}
