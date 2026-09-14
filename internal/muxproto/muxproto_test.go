package muxproto

import (
	"bytes"
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	out := &Hello{Token: "t", Tunnels: []TunnelSpec{{Name: "mc", Kind: KindMinecraft, Host: "a.example.com"}}}
	if err := Write(&buf, out); err != nil {
		t.Fatal(err)
	}
	var got Hello
	if err := NewReader(&buf).Read(&got); err != nil {
		t.Fatal(err)
	}
	if got.Token != "t" || len(got.Tunnels) != 1 || got.Tunnels[0].Host != "a.example.com" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

// An Error may arrive wherever a success message was expected, so it is
// surfaced as an error rather than a type mismatch.
func TestErrorSurfacesWhenSuccessExpected(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, &Error{Code: CodeAuth, Msg: "bad token"}); err != nil {
		t.Fatal(err)
	}
	var ok HelloOK
	err := NewReader(&buf).Read(&ok)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want *Error", err)
	}
	if pe.Code != CodeAuth {
		t.Fatalf("code = %q", pe.Code)
	}
}

func TestWrongTypeIsUnexpected(t *testing.T) {
	var buf bytes.Buffer
	Write(&buf, &StreamInit{TunnelID: "x"})
	var ok HelloOK
	if err := NewReader(&buf).Read(&ok); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("got %v, want ErrUnexpected", err)
	}
}

// Several messages share one stream, so the reader must keep its buffer across
// reads rather than losing bytes between them.
func TestSequentialReadsShareBuffer(t *testing.T) {
	var buf bytes.Buffer
	Write(&buf, &StreamInit{TunnelID: "a", ClientAddr: "1.2.3.4:5"})
	Write(&buf, &StreamInit{TunnelID: "b", ClientAddr: "6.7.8.9:1"})

	r := NewReader(&buf)
	for _, want := range []string{"a", "b"} {
		var si StreamInit
		if err := r.Read(&si); err != nil {
			t.Fatal(err)
		}
		if si.TunnelID != want {
			t.Fatalf("got %q, want %q", si.TunnelID, want)
		}
	}
}
