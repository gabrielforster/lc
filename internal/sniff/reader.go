package sniff

import "bytes"

// newByteReader reads over peeked bytes without taking ownership of them.
//
// The slice Peek returns is only valid until the next read, so a sniffer must
// parse from it immediately rather than hold onto it.
func newByteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
