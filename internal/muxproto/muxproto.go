// Package muxproto defines the control protocol spoken between the agent and
// the server over the yamux session.
//
// The framing is newline-delimited JSON. It is not the most compact choice, but
// control traffic is tiny next to tunnelled bytes, and being readable makes the
// protocol debuggable with nc.
package muxproto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Kind identifies a tunnel's protocol, which decides how the server routes to
// it and which transform the agent applies.
type Kind string

const (
	// KindTCP is a raw byte pipe. It carries no routing key, so the server
	// gives it a dedicated public port.
	KindTCP Kind = "tcp"
	// KindHTTP is proxied at L7 and routed by Host header.
	KindHTTP Kind = "http"
	// KindMinecraft is routed by the hostname in the Java handshake packet and
	// gets the BungeeCord address rewrite on the agent side.
	KindMinecraft Kind = "minecraft"
)

// TunnelSpec is a tunnel an agent asks the server to expose.
type TunnelSpec struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// LocalAddr is where the agent forwards to. The server never sees traffic
	// for this address; it is carried so the server can echo it back in status
	// output and, later, show it in the UI.
	LocalAddr string `json:"local_addr"`
	// Host is the public hostname for http and minecraft tunnels.
	Host string `json:"host,omitempty"`
	// PublicPort optionally requests a specific public port for tcp tunnels.
	// Zero means "assign one"; a reserved port is handed back automatically.
	PublicPort int `json:"public_port,omitempty"`
}

// Hello is the first message on the control stream.
type Hello struct {
	Token   string       `json:"token"`
	Tunnels []TunnelSpec `json:"tunnels"`
}

// TunnelResult reports where a requested tunnel ended up.
type TunnelResult struct {
	Name string `json:"name"`
	// ID addresses this tunnel in later StreamInit messages.
	ID string `json:"id"`
	// PublicAddr is what a user hands out, e.g. "vps.example.com:20001" or
	// "mc.example.com".
	PublicAddr string `json:"public_addr"`
}

// HelloOK accepts a registration.
type HelloOK struct {
	Tunnels []TunnelResult `json:"tunnels"`
}

// ClaimDomain requests a custom hostname at runtime, separately from Hello so a
// user can ask for one after connecting.
type ClaimDomain struct {
	Domain string `json:"domain"`
}

// ClaimReason explains a rejected claim. It is a typed value rather than free
// text so callers, and later the UI, can react to the specific case.
type ClaimReason string

const (
	ClaimOK ClaimReason = ""
	// ClaimTaken means another token already owns the domain.
	ClaimTaken ClaimReason = "taken"
	// ClaimReserved means the server uses the name itself.
	ClaimReserved ClaimReason = "reserved"
	// ClaimDisabled means the server was not started with custom domains on.
	ClaimDisabled ClaimReason = "disabled"
	// ClaimInvalid means the name is not a usable hostname.
	ClaimInvalid ClaimReason = "invalid"
	// ClaimNotFound means the token does not own the name it tried to release.
	ClaimNotFound ClaimReason = "not_found"
)

// ListDomains asks for the hostnames this token owns.
type ListDomains struct{}

// DomainList answers a ListDomains.
type DomainList struct {
	Domains []string `json:"domains"`
}

// ReleaseDomain gives up a claim.
type ReleaseDomain struct {
	Domain string `json:"domain"`
}

// ClaimResult answers a ClaimDomain, and a ReleaseDomain.
type ClaimResult struct {
	OK     bool        `json:"ok"`
	Reason ClaimReason `json:"reason,omitempty"`
	// NeedsDNS is set on success to remind the caller that claiming a name only
	// records intent on the server; the domain resolves, and a certificate can
	// be issued, only once its DNS points here.
	NeedsDNS bool `json:"needs_dns,omitempty"`
}

// StreamInit is the header the server writes when it opens a stream for an
// inbound public connection.
//
// ClientAddr is what makes real-client-IP forwarding a per-tunnel concern on
// the agent rather than a protocol change later: the server always reports it,
// and each tunnel kind decides whether and how to pass it on.
type StreamInit struct {
	TunnelID   string `json:"tunnel_id"`
	ClientAddr string `json:"client_addr"`
}

// Error is returned in place of any success message.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

// Common error codes.
const (
	CodeAuth      = "auth"
	CodeForbidden = "forbidden"
	CodeConflict  = "conflict"
	CodeBadReq    = "bad_request"
	CodeInternal  = "internal"
)

// envelope tags each message so a single stream can carry several types.
type envelope struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// typeName maps a message to its wire tag. Adding a message type means adding
// it here and in decode.
func typeName(v any) (string, error) {
	switch v.(type) {
	case *Hello:
		return "hello", nil
	case *HelloOK:
		return "hello_ok", nil
	case *ClaimDomain:
		return "claim_domain", nil
	case *ListDomains:
		return "list_domains", nil
	case *DomainList:
		return "domain_list", nil
	case *ReleaseDomain:
		return "release_domain", nil
	case *ClaimResult:
		return "claim_result", nil
	case *StreamInit:
		return "stream_init", nil
	case *Error:
		return "error", nil
	}
	return "", fmt.Errorf("muxproto: unknown message %T", v)
}

// Write encodes one message as a JSON line.
func Write(w io.Writer, v any) error {
	name, err := typeName(v)
	if err != nil {
		return err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line, err := json.Marshal(envelope{Type: name, Body: body})
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

// ErrUnexpected reports a message of the wrong type for the current state.
var ErrUnexpected = errors.New("muxproto: unexpected message type")

// Reader decodes messages from a stream. It owns a buffered reader, so one must
// be kept per stream for the lifetime of that stream rather than created per
// read, or buffered bytes are lost.
type Reader struct {
	br *bufio.Reader
}

func NewReader(r io.Reader) *Reader { return &Reader{br: bufio.NewReader(r)} }

// Read decodes the next message into out, which must be a pointer to the
// expected type. A protocol-level Error is returned as *Error, so callers can
// simply check err.
func (r *Reader) Read(out any) error {
	line, err := r.br.ReadBytes('\n')
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("muxproto: malformed frame: %w", err)
	}

	want, err := typeName(out)
	if err != nil {
		return err
	}
	if env.Type == "error" && want != "error" {
		var e Error
		if err := json.Unmarshal(env.Body, &e); err != nil {
			return fmt.Errorf("muxproto: malformed error frame: %w", err)
		}
		return &e
	}
	if env.Type != want {
		return fmt.Errorf("%w: got %q, want %q", ErrUnexpected, env.Type, want)
	}
	return json.Unmarshal(env.Body, out)
}

// ReadAny decodes the next frame without knowing its type, returning the wire
// tag and the undecoded body. The server's control loop uses this to dispatch
// between the several request types an agent may send at any time.
func (r *Reader) ReadAny() (string, json.RawMessage, error) {
	line, err := r.br.ReadBytes('\n')
	if err != nil {
		return "", nil, err
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return "", nil, fmt.Errorf("muxproto: malformed frame: %w", err)
	}
	return env.Type, env.Body, nil
}

// Buffered returns the underlying reader, positioned after the last decoded
// message.
//
// Decoding a frame can pull bytes behind it into the buffer. A caller that
// switches from control frames to raw payload on the same stream -- as the
// agent does after reading StreamInit -- must keep reading through this, or
// those buffered bytes are silently dropped.
func (r *Reader) Buffered() io.Reader { return r.br }
