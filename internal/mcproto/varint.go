// Package mcproto implements the small slice of the Minecraft Java protocol
// this tunnel needs: the handshake packet that carries the hostname a player
// typed, and the login packet that follows it.
//
// The wire format has been stable for years, so parsing it is safe in a way
// that parsing most game protocols is not.
package mcproto

import (
	"errors"
	"fmt"
	"io"
)

// ErrVarIntTooBig guards against a malformed or hostile length prefix.
var ErrVarIntTooBig = errors.New("mcproto: VarInt exceeds 5 bytes")

// ReadVarInt decodes Minecraft's variable-length integer: seven bits of payload
// per byte, high bit set while more bytes follow.
func ReadVarInt(r io.ByteReader) (int32, error) {
	var (
		value int32
		shift uint
	)
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= int32(b&0x7F) << shift
		if b&0x80 == 0 {
			return value, nil
		}
		shift += 7
	}
	return 0, ErrVarIntTooBig
}

// AppendVarInt encodes v onto dst.
func AppendVarInt(dst []byte, v int32) []byte {
	u := uint32(v)
	for {
		b := byte(u & 0x7F)
		u >>= 7
		if u != 0 {
			dst = append(dst, b|0x80)
			continue
		}
		return append(dst, b)
	}
}

// VarIntLen reports the encoded size of v, needed to compute a packet's length
// prefix before building it.
func VarIntLen(v int32) int {
	n := 1
	for u := uint32(v) >> 7; u != 0; u >>= 7 {
		n++
	}
	return n
}

// maxStringLen bounds a decoded string. The protocol's own limits are smaller;
// this is a sanity cap so a corrupt length cannot drive a huge allocation.
const maxStringLen = 32767

// ReadString decodes a VarInt-prefixed UTF-8 string.
func ReadString(r io.Reader) (string, error) {
	br, ok := r.(io.ByteReader)
	if !ok {
		return "", errors.New("mcproto: reader must implement io.ByteReader")
	}
	n, err := ReadVarInt(br)
	if err != nil {
		return "", err
	}
	if n < 0 || n > maxStringLen {
		return "", fmt.Errorf("mcproto: string length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// AppendString encodes s with its VarInt length prefix.
func AppendString(dst []byte, s string) []byte {
	dst = AppendVarInt(dst, int32(len(s)))
	return append(dst, s...)
}

// StringLen reports the encoded size of s.
func StringLen(s string) int { return VarIntLen(int32(len(s))) + len(s) }
