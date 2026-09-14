package mcproto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestVarIntRoundTrip(t *testing.T) {
	// Boundary values around each 7-bit group, plus the signed extremes.
	for _, v := range []int32{0, 1, 2, 127, 128, 255, 2097151, 2147483647, -1, -2147483648} {
		enc := AppendVarInt(nil, v)
		if len(enc) != VarIntLen(v) {
			t.Fatalf("v=%d: VarIntLen said %d, encoded %d bytes", v, VarIntLen(v), len(enc))
		}
		got, err := ReadVarInt(bytes.NewReader(enc))
		if err != nil {
			t.Fatalf("v=%d: %v", v, err)
		}
		if got != v {
			t.Fatalf("round trip: got %d, want %d", got, v)
		}
	}
}

func TestVarIntRejectsOverlongEncoding(t *testing.T) {
	// Six continuation bytes: a malformed or hostile prefix.
	overlong := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ReadVarInt(bytes.NewReader(overlong)); err != ErrVarIntTooBig {
		t.Fatalf("got %v, want ErrVarIntTooBig", err)
	}
}

// The golden vector: a real handshake as a vanilla client sends it.
func TestHandshakeRoundTrip(t *testing.T) {
	want := &Handshake{
		ProtocolVersion: 765,
		ServerAddress:   "mc.example.com",
		ServerPort:      25565,
		NextState:       StateLogin,
	}
	enc := want.Encode()

	got, err := ReadHandshake(bufio.NewReader(bytes.NewReader(enc)))
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Re-encoding must reproduce the same bytes, or a replayed handshake would
	// not be byte-identical to what the client sent.
	if !bytes.Equal(got.Encode(), enc) {
		t.Fatal("re-encoded handshake differs from the original bytes")
	}
}

func TestHandshakeRejectsNonMinecraft(t *testing.T) {
	// An HTTP request arriving on the Minecraft port must be recognised as not
	// being a handshake rather than parsed into nonsense.
	for _, input := range []string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		"\x00",
		"",
	} {
		_, err := ReadHandshake(bufio.NewReader(strings.NewReader(input)))
		if err == nil {
			t.Fatalf("input %q parsed as a handshake", input)
		}
	}
}

// A truncated handshake must fail rather than block or misparse.
func TestHandshakeTruncated(t *testing.T) {
	full := (&Handshake{ProtocolVersion: 765, ServerAddress: "mc.example.com",
		ServerPort: 25565, NextState: StateLogin}).Encode()

	for n := 1; n < len(full); n++ {
		if _, err := ReadHandshake(bufio.NewReader(bytes.NewReader(full[:n]))); err == nil {
			t.Fatalf("truncation at %d bytes parsed successfully", n)
		}
	}
}

// Forge clients append a marker to the address, so the routing key must survive
// it -- otherwise modded players fail to route.
func TestHostnameStripsForgeMarkerAndCase(t *testing.T) {
	cases := map[string]string{
		"mc.example.com":            "mc.example.com",
		"mc.example.com\x00FML\x00": "mc.example.com",
		"MC.Example.COM":            "mc.example.com",
		"mc.example.com.":           "mc.example.com",
	}
	for addr, want := range cases {
		h := &Handshake{ServerAddress: addr}
		if got := h.Hostname(); got != want {
			t.Fatalf("address %q: got %q, want %q", addr, got, want)
		}
	}
}

// The UUID must match what a vanilla offline-mode server computes, or player
// data and whitelists would point at a different player.
func TestOfflineUUIDMatchesVanilla(t *testing.T) {
	// Golden values: the version-3 UUID of "OfflinePlayer:<name>", which is
	// what a vanilla server computes with online-mode=false. If these drift,
	// existing player data, bans and whitelist entries stop matching.
	golden := map[string]string{
		"Notch":  "b50ad385829d3141a2167e7d7539ba7f",
		"jeb_":   "a762f5604fce3236812ab80efff0b62b",
		"Player": "a01e3843e5213998958af459800e4d11",
	}
	for name, want := range golden {
		if got := OfflineUUID(name); got != want {
			t.Fatalf("OfflineUUID(%q) = %s, want %s", name, got, want)
		}
	}

	// Version nibble 3 and the RFC 4122 variant, spelled out so a regression
	// in the bit-twiddling is legible rather than just a hash mismatch.
	got := OfflineUUID("Notch")
	if got[12] != '3' {
		t.Fatalf("uuid %q: version nibble is %q, want 3", got, got[12])
	}
	if v := got[16]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("uuid %q: variant nibble is %q", got, v)
	}
}

// The forwarding payload's exact shape is what the server parses, so it is
// pinned here.
func TestRewriteForForwarding(t *testing.T) {
	h := &Handshake{
		ProtocolVersion: 765,
		ServerAddress:   "mc.example.com\x00FML\x00",
		ServerPort:      25565,
		NextState:       StateLogin,
	}
	uuid := OfflineUUID("Notch")
	h.RewriteForForwarding("203.0.113.9", uuid)

	parts := strings.Split(h.ServerAddress, "\x00")
	if len(parts) != 4 {
		t.Fatalf("want 4 null-separated fields, got %d: %q", len(parts), h.ServerAddress)
	}
	// The Forge marker must be gone, or the host field would carry it into the
	// forwarding payload.
	if parts[0] != "mc.example.com" {
		t.Fatalf("host field = %q", parts[0])
	}
	if parts[1] != "203.0.113.9" {
		t.Fatalf("ip field = %q", parts[1])
	}
	if parts[2] != uuid {
		t.Fatalf("uuid field = %q", parts[2])
	}
	if parts[3] != "[]" {
		t.Fatalf("properties field = %q, want an empty JSON array", parts[3])
	}

	// It must still be a well-formed handshake afterwards.
	got, err := ReadHandshake(bufio.NewReader(bytes.NewReader(h.Encode())))
	if err != nil {
		t.Fatalf("rewritten handshake no longer parses: %v", err)
	}
	if got.ServerAddress != h.ServerAddress {
		t.Fatal("rewritten address did not survive a round trip")
	}
}

func TestLoginStartKeepsRawBytes(t *testing.T) {
	body := AppendVarInt(nil, idLoginStart)
	body = AppendString(body, "Notch")
	body = append(body, 0x01, 0xAA, 0xBB) // trailing fields newer versions add

	packet := AppendVarInt(nil, int32(len(body)))
	packet = append(packet, body...)

	got, err := ReadLoginStart(bufio.NewReader(bytes.NewReader(packet)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "Notch" {
		t.Fatalf("username = %q", got.Username)
	}
	// Replaying verbatim is what keeps this parser independent of the fields
	// newer versions added after the username.
	if !bytes.Equal(got.Raw, packet) {
		t.Fatal("raw login packet was not preserved byte for byte")
	}
}
